package collectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitalocean/go-libvirt"

	"github.com/tinkerologie/unraid-management-agent/daemon/constants"
	"github.com/tinkerologie/unraid-management-agent/daemon/domain"
	"github.com/tinkerologie/unraid-management-agent/daemon/dto"
	"github.com/tinkerologie/unraid-management-agent/daemon/logger"
)

// Commands run inside the guest for the full update. nvidia-smi reports memory
// in MiB. The PowerShell query lists each physical disk with its reliability
// counters.
const (
	guestNvidiaSMIWindows = `C:\Windows\System32\nvidia-smi.exe`
	guestNvidiaSMILinux   = "nvidia-smi"
	guestNvidiaSMIQuery   = "--query-gpu=index,name,temperature.gpu,utilization.gpu,memory.used,memory.total,power.draw,fan.speed,clocks.gr"
	guestPowerShell       = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	guestDiskScript       = `Get-PhysicalDisk | ForEach-Object { $r = $_ | Get-StorageReliabilityCounter; ` +
		`[pscustomobject]@{Model=$_.FriendlyName; Serial=$_.SerialNumber; Bus=[string]$_.BusType; ` +
		`Temp=$r.Temperature; TempMax=$r.TemperatureMax; Wear=$r.Wear} } | ConvertTo-Json -Compress`
)

var (
	// guestAgentTimeout is how long libvirt waits for one guest agent reply.
	guestAgentTimeout int32 = 5
	// guestExecTimeout is how long a command run in the guest may take.
	guestExecTimeout = 30 * time.Second
	// guestExecPoll is the wait between guest-exec-status calls.
	guestExecPoll = 250 * time.Millisecond
)

// guestConn is the part of *libvirt.Libvirt the guest poller uses.
type guestConn interface {
	DomainLookupByName(name string) (libvirt.Domain, error)
	DomainGetState(dom libvirt.Domain, flags uint32) (int32, int32, error)
	QEMUDomainAgentCommand(dom libvirt.Domain, cmd string, timeout int32, flags uint32) (libvirt.OptString, error)
	Disconnect() error
}

func dialLibvirt() (guestConn, error) {
	uri, _ := url.Parse(string(libvirt.QEMUSystem))
	return libvirt.ConnectToURI(uri)
}

// vmGuestPoller reads data from inside VMs through the QEMU guest agent: a
// full update (host name, OS, filesystems, GPUs, disks) every interval, and
// each custom sensor on its own interval. Every change publishes a snapshot of
// all configured VMs.
type vmGuestPoller struct {
	cfg  domain.VMGuestConfig
	hub  *domain.EventBus
	dial func() (guestConn, error)

	mu    sync.Mutex
	infos map[string]*dto.VMGuestInfo
}

func newVMGuestPoller(cfg domain.VMGuestConfig, hub *domain.EventBus) *vmGuestPoller {
	p := &vmGuestPoller{cfg: cfg, hub: hub, dial: dialLibvirt, infos: make(map[string]*dto.VMGuestInfo)}
	for _, vm := range cfg.VMs {
		p.infos[vm] = &dto.VMGuestInfo{
			VMName:      vm,
			Filesystems: []dto.VMGuestFilesystem{},
			GPUs:        []dto.VMGuestGPU{},
			Disks:       []dto.VMGuestDisk{},
			Sensors:     []dto.VMGuestSensor{},
		}
	}
	for _, s := range cfg.Sensors {
		info := p.infos[s.VM]
		info.Sensors = append(info.Sensors, dto.VMGuestSensor{Name: s.Name})
	}
	return p
}

// Start runs the full update and every sensor until ctx is cancelled.
func (p *vmGuestPoller) Start(ctx context.Context) {
	logger.Info("Starting VM guest agent reads for %d VM(s), %d sensor(s)", len(p.cfg.VMs), len(p.cfg.Sensors))
	var wg sync.WaitGroup
	for _, s := range p.cfg.Sensors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.loop(ctx, time.Duration(s.Interval)*time.Second, func() { p.runSensor(s) })
		}()
	}
	p.loop(ctx, time.Duration(p.cfg.Interval)*time.Second, p.updateAll)
	wg.Wait()
}

// loop calls fn now and then every interval, recovering from panics.
func (p *vmGuestPoller) loop(ctx context.Context, interval time.Duration, fn func()) {
	run := func() {
		defer func() {
			if r := recover(); r != nil {
				logger.LogPanicWithStack("VM guest agent", r)
			}
		}()
		fn()
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// publish sends a copy of every VM's current data.
func (p *vmGuestPoller) publish() {
	p.mu.Lock()
	out := make([]*dto.VMGuestInfo, 0, len(p.cfg.VMs))
	for _, vm := range p.cfg.VMs {
		info := *p.infos[vm]
		info.Filesystems = append([]dto.VMGuestFilesystem{}, info.Filesystems...)
		info.GPUs = append([]dto.VMGuestGPU{}, info.GPUs...)
		info.Disks = append([]dto.VMGuestDisk{}, info.Disks...)
		info.Sensors = append([]dto.VMGuestSensor{}, info.Sensors...)
		out = append(out, &info)
	}
	p.mu.Unlock()
	domain.Publish(p.hub, constants.TopicVMGuestUpdate, out)
}

// updateAll runs the full update for every VM and publishes once.
func (p *vmGuestPoller) updateAll() {
	conn, err := p.dial()
	if err != nil {
		logger.Debug("VM guest agent: libvirt connection failed: %v", err)
		for _, vm := range p.cfg.VMs {
			p.setOffline(vm, false, "libvirt connection failed")
		}
		p.publish()
		return
	}
	defer conn.Disconnect() //nolint:errcheck
	for _, vm := range p.cfg.VMs {
		p.updateVM(conn, vm)
	}
	p.publish()
}

// runningDomain looks up a VM and reports whether it is running.
func runningDomain(conn guestConn, vm string) (libvirt.Domain, bool, error) {
	dom, err := conn.DomainLookupByName(vm)
	if err != nil {
		return dom, false, fmt.Errorf("VM not found: %w", err)
	}
	state, _, err := conn.DomainGetState(dom, 0)
	if err != nil {
		return dom, false, err
	}
	return dom, libvirt.DomainState(state) == libvirt.DomainRunning, nil
}

func (p *vmGuestPoller) updateVM(conn guestConn, vm string) {
	dom, running, err := runningDomain(conn, vm)
	if err != nil || !running {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		p.setOffline(vm, false, msg)
		return
	}
	if err := agentCall(conn, dom, "guest-ping", nil, nil); err != nil {
		p.setOffline(vm, true, "guest agent not responding")
		return
	}

	var host struct {
		HostName string `json:"host-name"`
	}
	var osInfo struct {
		ID         string `json:"id"`
		PrettyName string `json:"pretty-name"`
	}
	_ = agentCall(conn, dom, "guest-get-host-name", nil, &host)
	_ = agentCall(conn, dom, "guest-get-osinfo", nil, &osInfo)

	var fsList []guestFSInfo
	fsErr := agentCall(conn, dom, "guest-get-fsinfo", nil, &fsList)

	smi := guestNvidiaSMILinux
	if osInfo.ID == "mswindows" {
		smi = guestNvidiaSMIWindows
	}
	smiOut, gpuErr := guestExec(conn, dom, smi, []string{guestNvidiaSMIQuery, "--format=csv,noheader,nounits"})
	var gpus []dto.VMGuestGPU
	if gpuErr == nil {
		gpus = parseNvidiaSMI(smiOut)
	} else {
		logger.Debug("VM guest agent: %s: nvidia-smi: %v", vm, gpuErr)
	}

	var disks []dto.VMGuestDisk
	var diskErr error
	if osInfo.ID == "mswindows" {
		var out string
		out, diskErr = guestExec(conn, dom, guestPowerShell, []string{"-NoProfile", "-NonInteractive", "-Command", guestDiskScript})
		if diskErr == nil {
			disks, diskErr = parseGuestDisks(out)
		}
		if diskErr != nil {
			logger.Debug("VM guest agent: %s: disk query: %v", vm, diskErr)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	info := p.infos[vm]
	info.Running = true
	info.AgentConnected = true
	info.Error = ""
	info.Hostname = host.HostName
	info.OSName = osInfo.PrettyName
	info.Timestamp = time.Now()
	if fsErr == nil {
		info.Filesystems = filesystemsFromFSInfo(fsList)
	} else {
		info.Filesystems = clearFilesystems(info.Filesystems)
	}
	if gpuErr == nil {
		info.GPUs = gpus
	} else {
		info.GPUs = clearGPUs(info.GPUs)
	}
	if diskErr == nil && disks != nil {
		info.Disks = disks
	} else {
		info.Disks = clearDisks(info.Disks)
	}
}

// setOffline keeps the VM's GPUs, disks, filesystems and sensors with every
// reading set to null.
func (p *vmGuestPoller) setOffline(vm string, running bool, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := p.infos[vm]
	info.Running = running
	info.AgentConnected = false
	info.Error = msg
	info.Timestamp = time.Now()
	info.Filesystems = clearFilesystems(info.Filesystems)
	info.GPUs = clearGPUs(info.GPUs)
	info.Disks = clearDisks(info.Disks)
	for i := range info.Sensors {
		info.Sensors[i].Value = nil
		info.Sensors[i].Error = ""
	}
}

// runSensor runs one custom sensor command and publishes the result.
func (p *vmGuestPoller) runSensor(s domain.VMGuestSensorConfig) {
	var value *string
	errMsg := ""
	conn, err := p.dial()
	if err != nil {
		errMsg = "libvirt connection failed"
	} else {
		defer conn.Disconnect() //nolint:errcheck
		dom, running, err := runningDomain(conn, s.VM)
		switch {
		case err != nil:
			errMsg = err.Error()
		case running:
			out, err := guestExec(conn, dom, s.Command[0], s.Command[1:])
			if err != nil {
				errMsg = err.Error()
				logger.Debug("VM guest agent: %s: sensor %s: %v", s.VM, s.Name, err)
			} else {
				v := strings.TrimSpace(out)
				value = &v
			}
		}
	}

	now := time.Now()
	p.mu.Lock()
	for i := range p.infos[s.VM].Sensors {
		sensor := &p.infos[s.VM].Sensors[i]
		if sensor.Name == s.Name {
			sensor.Value = value
			sensor.Error = errMsg
			if value != nil {
				sensor.UpdatedAt = &now
			}
		}
	}
	p.mu.Unlock()
	p.publish()
}

// agentCall runs a guest agent command and decodes its "return" value into
// out, which may be nil.
func agentCall(conn guestConn, dom libvirt.Domain, cmd string, args any, out any) error {
	req := map[string]any{"execute": cmd}
	if args != nil {
		req["arguments"] = args
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	res, err := conn.QEMUDomainAgentCommand(dom, string(body), guestAgentTimeout, 0)
	if err != nil {
		return err
	}
	if len(res) == 0 {
		return errors.New("empty guest agent reply")
	}
	var reply struct {
		Return json.RawMessage `json:"return"`
	}
	if err := json.Unmarshal([]byte(res[0]), &reply); err != nil {
		return fmt.Errorf("decode guest agent reply: %w", err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(reply.Return, out)
}

// guestExec runs a program inside the guest without a shell and returns its
// standard output. A non-zero exit code is an error.
func guestExec(conn guestConn, dom libvirt.Domain, path string, args []string) (string, error) {
	var started struct {
		PID int `json:"pid"`
	}
	execArgs := map[string]any{"path": path, "arg": args, "capture-output": true}
	if err := agentCall(conn, dom, "guest-exec", execArgs, &started); err != nil {
		return "", err
	}
	deadline := time.Now().Add(guestExecTimeout)
	for {
		var status struct {
			Exited   bool   `json:"exited"`
			ExitCode int    `json:"exitcode"`
			OutData  string `json:"out-data"`
			ErrData  string `json:"err-data"`
		}
		if err := agentCall(conn, dom, "guest-exec-status", map[string]int{"pid": started.PID}, &status); err != nil {
			return "", err
		}
		if status.Exited {
			out, _ := base64.StdEncoding.DecodeString(status.OutData)
			if status.ExitCode != 0 {
				stderr, _ := base64.StdEncoding.DecodeString(status.ErrData)
				line, _, _ := strings.Cut(strings.TrimSpace(string(stderr)), "\n")
				return "", fmt.Errorf("exit code %d: %s", status.ExitCode, truncate(strings.TrimSpace(line), 200))
			}
			return string(out), nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out after %v", guestExecTimeout)
		}
		time.Sleep(guestExecPoll)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// guestFSInfo is one entry of guest-get-fsinfo.
type guestFSInfo struct {
	Mountpoint string  `json:"mountpoint"`
	Type       string  `json:"type"`
	TotalBytes *uint64 `json:"total-bytes"`
	UsedBytes  *uint64 `json:"used-bytes"`
}

// filesystemsFromFSInfo keeps mounted filesystems that report a size.
func filesystemsFromFSInfo(list []guestFSInfo) []dto.VMGuestFilesystem {
	out := []dto.VMGuestFilesystem{}
	for _, f := range list {
		if f.Mountpoint == "" || f.TotalBytes == nil {
			continue
		}
		out = append(out, dto.VMGuestFilesystem{Mountpoint: f.Mountpoint, Type: f.Type, TotalBytes: f.TotalBytes, UsedBytes: f.UsedBytes})
	}
	return out
}

// parseNvidiaSMI parses nvidia-smi CSV output (noheader, nounits). The name is
// everything between the index and the last seven fields, so a comma in it
// does not shift the readings.
func parseNvidiaSMI(out string) []dto.VMGuestGPU {
	gpus := []dto.VMGuestGPU{}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 9 {
			continue
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		index, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		n := len(fields)
		r := fields[n-7:]
		gpus = append(gpus, dto.VMGuestGPU{
			Index:              index,
			Name:               strings.Join(fields[1:n-7], ","),
			TemperatureC:       smiFloat(r[0]),
			UtilizationPercent: smiFloat(r[1]),
			MemoryUsedBytes:    smiMiB(r[2]),
			MemoryTotalBytes:   smiMiB(r[3]),
			PowerWatts:         smiFloat(r[4]),
			FanPercent:         smiFloat(r[5]),
			ClockMHz:           smiFloat(r[6]),
		})
	}
	return gpus
}

// smiFloat parses a reading; "[N/A]", "[Not Supported]" and the like are nil.
func smiFloat(s string) *float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

func smiMiB(s string) *uint64 {
	v := smiFloat(s)
	if v == nil || *v < 0 {
		return nil
	}
	b := uint64(*v) * 1024 * 1024
	return &b
}

// parseGuestDisks parses the PowerShell disk query. ConvertTo-Json writes a
// single object, not an array, when there is one disk. A temperature of 0
// means the drive does not report it.
func parseGuestDisks(out string) ([]dto.VMGuestDisk, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return []dto.VMGuestDisk{}, nil
	}
	if strings.HasPrefix(out, "{") {
		out = "[" + out + "]"
	}
	var raw []struct {
		Model   string   `json:"Model"`
		Serial  string   `json:"Serial"`
		Bus     string   `json:"Bus"`
		Temp    *float64 `json:"Temp"`
		TempMax *float64 `json:"TempMax"`
		Wear    *float64 `json:"Wear"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("decode disk query: %w", err)
	}
	disks := make([]dto.VMGuestDisk, 0, len(raw))
	for _, d := range raw {
		disks = append(disks, dto.VMGuestDisk{
			Model:           strings.TrimSpace(d.Model),
			Serial:          strings.TrimSpace(d.Serial),
			Bus:             d.Bus,
			TemperatureC:    nonZero(d.Temp),
			TemperatureMaxC: nonZero(d.TempMax),
			WearPercent:     d.Wear,
		})
	}
	return disks, nil
}

func nonZero(v *float64) *float64 {
	if v == nil || *v == 0 {
		return nil
	}
	return v
}

func clearFilesystems(in []dto.VMGuestFilesystem) []dto.VMGuestFilesystem {
	out := make([]dto.VMGuestFilesystem, len(in))
	for i, f := range in {
		out[i] = dto.VMGuestFilesystem{Mountpoint: f.Mountpoint, Type: f.Type}
	}
	return out
}

func clearGPUs(in []dto.VMGuestGPU) []dto.VMGuestGPU {
	out := make([]dto.VMGuestGPU, len(in))
	for i, g := range in {
		out[i] = dto.VMGuestGPU{Index: g.Index, Name: g.Name}
	}
	return out
}

func clearDisks(in []dto.VMGuestDisk) []dto.VMGuestDisk {
	out := make([]dto.VMGuestDisk, len(in))
	for i, d := range in {
		out[i] = dto.VMGuestDisk{Model: d.Model, Serial: d.Serial, Bus: d.Bus}
	}
	return out
}
