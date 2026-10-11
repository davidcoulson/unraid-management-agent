package collectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tinkerologie/unraid-management-agent/daemon/constants"
	"github.com/tinkerologie/unraid-management-agent/daemon/dto"
	"github.com/tinkerologie/unraid-management-agent/daemon/lib"
	"github.com/tinkerologie/unraid-management-agent/daemon/logger"
)

const (
	// driveHealthTTL is how long a disk's SMART reading is reused before
	// smartctl is run again. SMART values change slowly; this keeps the cost
	// at one smartctl run per pool member every five minutes however short the
	// ZFS interval is.
	driveHealthTTL = 5 * time.Minute
	// driveHealthTimeout bounds one smartctl run.
	driveHealthTimeout = 15 * time.Second
	// driveHealthWorkers bounds how many smartctl runs happen at once.
	driveHealthWorkers = 4
)

// blockDeviceName matches a kernel block device name such as sda or nvme0n1.
var blockDeviceName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// smartctlRunner runs smartctl for one disk and returns its stdout. The
// channel is closed once the process has been reaped.
type smartctlRunner func(timeout time.Duration, disk string) (string, <-chan struct{}, error)

// runSmartctl reads a disk's SMART data as JSON without waking it from standby.
func runSmartctl(timeout time.Duration, disk string) (string, <-chan struct{}, error) {
	return lib.ExecCommandStdoutGuarded(context.Background(), timeout,
		constants.SmartctlBin, "-j", "-a", "-n", "standby", "/dev/"+disk)
}

// driveHealthEntry is the cached state of one disk.
type driveHealthEntry struct {
	health    *dto.DriveHealth // last successful read, nil if none
	standby   bool             // asleep at the last attempt
	attempted time.Time        // last time smartctl was run
	inflight  <-chan struct{}  // closed once the last smartctl process exited
}

// driveHealthCache reads SMART data for ZFS pool members and caches it per disk.
type driveHealthCache struct {
	mu      sync.Mutex
	binary  string
	ttl     time.Duration
	timeout time.Duration
	workers int
	now     func() time.Time
	run     smartctlRunner
	entries map[string]*driveHealthEntry
}

func newDriveHealthCache() *driveHealthCache {
	return &driveHealthCache{
		binary:  constants.SmartctlBin,
		ttl:     driveHealthTTL,
		timeout: driveHealthTimeout,
		workers: driveHealthWorkers,
		now:     time.Now,
		run:     runSmartctl,
		entries: make(map[string]*driveHealthEntry),
	}
}

// lookup returns the health of each disk, refreshing entries older than the
// TTL. Disks that have never been read successfully and are not asleep are
// left out. Entries for disks no longer asked for are dropped.
func (c *driveHealthCache) lookup(disks []string) map[string]*dto.DriveHealth {
	if _, err := os.Stat(c.binary); err != nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	wanted := make(map[string]bool, len(disks))
	var stale []string
	for _, disk := range disks {
		if wanted[disk] || !blockDeviceName.MatchString(disk) {
			continue
		}
		wanted[disk] = true
		entry := c.entries[disk]
		if entry == nil {
			entry = &driveHealthEntry{}
			c.entries[disk] = entry
		}
		if !entry.attempted.IsZero() && now.Sub(entry.attempted) < c.ttl {
			continue
		}
		if entry.inflight != nil && !closed(entry.inflight) {
			// A previous smartctl for this disk is still stuck; don't pile up.
			continue
		}
		stale = append(stale, disk)
	}
	for disk := range c.entries {
		if !wanted[disk] {
			delete(c.entries, disk)
		}
	}

	c.refresh(stale, now)

	result := make(map[string]*dto.DriveHealth, len(wanted))
	for disk := range wanted {
		entry := c.entries[disk]
		switch {
		case entry.health != nil:
			health := *entry.health
			health.Standby = entry.standby
			result[disk] = &health
		case entry.standby:
			result[disk] = &dto.DriveHealth{Standby: true}
		}
	}
	return result
}

// refresh runs smartctl for the given disks, at most c.workers at a time.
func (c *driveHealthCache) refresh(disks []string, now time.Time) {
	type outcome struct {
		disk     string
		health   *dto.DriveHealth
		standby  bool
		inflight <-chan struct{}
	}
	results := make(chan outcome, len(disks))
	sem := make(chan struct{}, max(c.workers, 1))
	var wg sync.WaitGroup
	for _, disk := range disks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out, done, runErr := c.run(c.timeout, disk)
			health, standby, err := parseSmartctlJSON(out, now)
			if err != nil {
				logger.Debug("ZFS: SMART read of %s failed: %v (smartctl: %v)", disk, err, runErr)
			}
			results <- outcome{disk: disk, health: health, standby: standby, inflight: done}
		}()
	}
	wg.Wait()
	close(results)

	for r := range results {
		entry := c.entries[r.disk]
		entry.attempted = now
		entry.inflight = r.inflight
		switch {
		case r.standby:
			entry.standby = true
		case r.health != nil:
			entry.health = r.health
			entry.standby = false
		}
	}
}

// closed reports whether ch has been closed.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// smartctlJSON is the subset of `smartctl -j -a` output the agent uses.
type smartctlJSON struct {
	Smartctl struct {
		ExitStatus int `json:"exit_status"`
		Messages   []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
	Device struct {
		Protocol string `json:"protocol"`
	} `json:"device"`
	ModelName    string `json:"model_name"`
	RotationRate *int   `json:"rotation_rate"`
	SmartStatus  *struct {
		Passed *bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current *int `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours *uint64 `json:"hours"`
	} `json:"power_on_time"`
	NVMeLog *struct {
		CriticalWarning *int    `json:"critical_warning"`
		AvailableSpare  *int    `json:"available_spare"`
		PercentageUsed  *int    `json:"percentage_used"`
		MediaErrors     *uint64 `json:"media_errors"`
	} `json:"nvme_smart_health_information_log"`
	SCSIErrors *struct {
		Read   *scsiErrorCounters `json:"read"`
		Write  *scsiErrorCounters `json:"write"`
		Verify *scsiErrorCounters `json:"verify"`
	} `json:"scsi_error_counter_log"`
	ATAAttributes *struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value *uint64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
}

type scsiErrorCounters struct {
	TotalUncorrected *uint64 `json:"total_uncorrected_errors"`
}

// ataReportedUncorrect is ATA SMART attribute 187 (Reported_Uncorrect).
const ataReportedUncorrect = 187

// parseSmartctlJSON turns `smartctl -j -a -n standby` output into a
// DriveHealth. standby is true when smartctl skipped the disk because it was
// asleep. smartctl's exit status is a bitmask that is non-zero for a failing
// drive too, so the output is parsed whatever the exit status was.
func parseSmartctlJSON(out string, now time.Time) (*dto.DriveHealth, bool, error) {
	if strings.TrimSpace(out) == "" {
		return nil, false, errors.New("no output")
	}
	var raw smartctlJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		// A value that does not fit its field (e.g. a 128-bit NVMe counter)
		// only loses that field; anything else is unusable output.
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return nil, false, fmt.Errorf("parse smartctl JSON: %w", err)
		}
	}
	for _, msg := range raw.Smartctl.Messages {
		upper := strings.ToUpper(msg.String)
		if strings.Contains(upper, "STANDBY") || strings.Contains(upper, "SLEEP") {
			return nil, true, nil
		}
	}
	if raw.Device.Protocol == "" && raw.ModelName == "" {
		return nil, false, fmt.Errorf("no device data (exit status %d)", raw.Smartctl.ExitStatus)
	}

	health := &dto.DriveHealth{
		Model:       strings.TrimSpace(raw.ModelName),
		Protocol:    raw.Device.Protocol,
		CollectedAt: now,
	}
	switch {
	case raw.Device.Protocol == "NVMe":
		health.MediaType = "nvme"
	case raw.RotationRate != nil && *raw.RotationRate == 0:
		health.MediaType = "ssd"
	case raw.RotationRate != nil:
		health.MediaType = "hdd"
	}
	if raw.SmartStatus != nil {
		health.SmartPassed = raw.SmartStatus.Passed
	}
	if raw.Temperature != nil {
		health.TemperatureCelsius = raw.Temperature.Current
	}
	if raw.PowerOnTime != nil {
		health.PowerOnHours = raw.PowerOnTime.Hours
	}
	if log := raw.NVMeLog; log != nil {
		health.PercentageUsed = log.PercentageUsed
		health.AvailableSpare = log.AvailableSpare
		health.CriticalWarning = log.CriticalWarning
		health.MediaErrors = log.MediaErrors
	}
	if scsi := raw.SCSIErrors; scsi != nil {
		health.MediaErrors = sumUncorrected(scsi.Read, scsi.Write, scsi.Verify)
	}
	if ata := raw.ATAAttributes; ata != nil && health.MediaErrors == nil {
		for _, attr := range ata.Table {
			if attr.ID == ataReportedUncorrect {
				health.MediaErrors = attr.Raw.Value
				break
			}
		}
	}
	return health, false, nil
}

// sumUncorrected adds the SCSI uncorrected-error counters that are present,
// or returns nil when none are.
func sumUncorrected(logs ...*scsiErrorCounters) *uint64 {
	var total uint64
	found := false
	for _, l := range logs {
		if l != nil && l.TotalUncorrected != nil {
			total += *l.TotalUncorrected
			found = true
		}
	}
	if !found {
		return nil
	}
	return &total
}

// poolDrives lists the leaf members of a pool: the devices under each vdev,
// or the vdev itself when it is a single disk. It returns nil when the pool
// has no vdevs (status unavailable).
func poolDrives(pool *dto.ZFSPool) []dto.ZFSDrive {
	var drives []dto.ZFSDrive
	for _, vdev := range pool.VDEVs {
		if len(vdev.Devices) == 0 {
			if vdev.Type == "disk" {
				drives = append(drives, dto.ZFSDrive{Name: vdev.Name, State: vdev.State})
			}
			continue
		}
		for _, dev := range vdev.Devices {
			drives = append(drives, dto.ZFSDrive{Name: dev.Name, Vdev: vdev.Name, State: dev.State})
		}
	}
	return drives
}

// resolveWholeDisk maps a pool member as `zpool status` prints it (a
// partition such as sdb1 or nvme0n1p1, a whole disk, a device-mapper name or
// a /dev/disk/by-* link name) to the kernel name of the disk it lives on.
// It returns "" when the member cannot be found (e.g. a missing device that
// zpool shows by GUID).
func resolveWholeDisk(sysRoot, devRoot, member string) string {
	node := filepath.Base(member)
	if !blockDeviceName.MatchString(node) {
		return ""
	}
	if _, err := os.Stat(filepath.Join(sysRoot, "class", "block", node)); err != nil {
		node = ""
		for _, dir := range []string{"mapper", filepath.Join("disk", "by-id"), filepath.Join("disk", "by-partuuid")} {
			target, err := filepath.EvalSymlinks(filepath.Join(devRoot, dir, filepath.Base(member)))
			if err == nil {
				node = filepath.Base(target)
				break
			}
		}
		if node == "" {
			return ""
		}
	}

	// Follow device-mapper layers (LUKS) down to the partition, then the
	// partition up to its disk. Three hops cover dm -> partition -> disk.
	for range 3 {
		block := filepath.Join(sysRoot, "class", "block", node)
		if slaves, err := os.ReadDir(filepath.Join(block, "slaves")); err == nil && len(slaves) > 0 {
			node = slaves[0].Name()
			continue
		}
		// A partition's sysfs directory sits inside its disk's directory.
		if real, err := filepath.EvalSymlinks(filepath.Join(block, "partition")); err == nil {
			return filepath.Base(filepath.Dir(filepath.Dir(real)))
		}
		if _, err := os.Stat(block); err != nil {
			return ""
		}
		return node
	}
	return ""
}

// attachDrives fills each pool's Drives with its members, the disk each one
// is on, and that disk's cached SMART health.
func (c *ZFSCollector) attachDrives(pools []dto.ZFSPool) {
	var disks []string
	for i := range pools {
		pools[i].Drives = poolDrives(&pools[i])
		for j := range pools[i].Drives {
			drive := &pools[i].Drives[j]
			drive.Disk = resolveWholeDisk(c.sysRoot, c.devRoot, drive.Name)
			if drive.Disk != "" {
				disks = append(disks, drive.Disk)
			}
		}
	}
	if c.driveHealth == nil {
		return
	}
	health := c.driveHealth.lookup(disks)
	for i := range pools {
		for j := range pools[i].Drives {
			drive := &pools[i].Drives[j]
			if h, ok := health[drive.Disk]; ok {
				drive.Health = h
			}
		}
	}
}
