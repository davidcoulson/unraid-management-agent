package collectors

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/tinkerologie/unraid-management-agent/daemon/dto"
)

func readSmartctlFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "smartctl", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

// TestParseSmartctlJSON checks the fields taken from real `smartctl -j -a`
// output of an NVMe, a SAS and a SATA drive (identifiers removed), and the
// standby, failure and malformed cases.
func TestParseSmartctlJSON(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		output      string
		want        *dto.DriveHealth
		wantStandby bool
		wantErr     bool
	}{
		{
			name:   "nvme",
			output: readSmartctlFixture(t, "nvme.json"),
			want: &dto.DriveHealth{
				Model:              "MKNSSDPE2TB-D8",
				Protocol:           "NVMe",
				MediaType:          "nvme",
				SmartPassed:        new(true),
				TemperatureCelsius: new(35),
				PercentageUsed:     new(89),
				AvailableSpare:     new(100),
				CriticalWarning:    new(0),
				MediaErrors:        new(uint64(0)),
				PowerOnHours:       new(uint64(31520)),
				CollectedAt:        now,
			},
		},
		{
			name:   "sas hdd",
			output: readSmartctlFixture(t, "scsi.json"),
			want: &dto.DriveHealth{
				Model:              "SEAGATE ST24000NM007H",
				Protocol:           "SCSI",
				MediaType:          "hdd",
				SmartPassed:        new(true),
				TemperatureCelsius: new(37),
				MediaErrors:        new(uint64(0)),
				PowerOnHours:       new(uint64(14219)),
				CollectedAt:        now,
			},
		},
		{
			name:   "sata ssd",
			output: readSmartctlFixture(t, "ata.json"),
			want: &dto.DriveHealth{
				Model:              "Micron_5400_MTFDDAK7T6TGA",
				Protocol:           "ATA",
				MediaType:          "ssd",
				SmartPassed:        new(true),
				TemperatureCelsius: new(32),
				MediaErrors:        new(uint64(0)),
				PowerOnHours:       new(uint64(14791)),
				CollectedAt:        now,
			},
		},
		{
			name:        "standby",
			output:      readSmartctlFixture(t, "standby.json"),
			wantStandby: true,
		},
		{
			name:    "open failed",
			output:  readSmartctlFixture(t, "open-failed.json"),
			wantErr: true,
		},
		{
			name:    "empty",
			output:  "  \n",
			wantErr: true,
		},
		{
			name:    "not json",
			output:  "smartctl: command not found",
			wantErr: true,
		},
		{
			name: "failing drive, scsi counters partly present, oversized nvme counter",
			output: `{"smartctl":{"exit_status":8},"device":{"protocol":"SCSI"},"model_name":" X ",
				"rotation_rate":7200,"smart_status":{"passed":false},
				"scsi_error_counter_log":{"read":{"total_uncorrected_errors":2},"verify":{"total_uncorrected_errors":3}},
				"ata_smart_attributes":{"table":[{"id":187,"raw":{"value":9}}]},
				"nvme_smart_health_information_log":{"media_errors":340282366920938463463374607431768211455}}`,
			want: &dto.DriveHealth{
				Model:       "X",
				Protocol:    "SCSI",
				MediaType:   "hdd",
				SmartPassed: new(false),
				MediaErrors: new(uint64(5)),
				CollectedAt: now,
			},
		},
		{
			name: "scsi log without uncorrected counters, ata without attribute 187",
			output: `{"device":{"protocol":"ATA"},
				"scsi_error_counter_log":{"read":{"total_errors_corrected":1}},
				"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":1}}]}}`,
			want: &dto.DriveHealth{Protocol: "ATA", CollectedAt: now},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, standby, err := parseSmartctlJSON(tt.output, now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if standby != tt.wantStandby {
				t.Errorf("standby = %v, want %v", standby, tt.wantStandby)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("health = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// fakeSysfs builds a sysfs/dev tree: an NVMe disk with a partition, a whole
// SATA disk with a partition, a LUKS mapping over a partition, a by-id link,
// and broken device-mapper chains.
func fakeSysfs(t *testing.T) (sysRoot, devRoot string) {
	t.Helper()
	root := t.TempDir()
	sysRoot = filepath.Join(root, "sys")
	devRoot = filepath.Join(root, "dev")
	block := filepath.Join(sysRoot, "class", "block")

	mkdir := func(p string) {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(p string) {
		t.Helper()
		mkdir(filepath.Dir(p))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, name string) {
		t.Helper()
		mkdir(filepath.Dir(name))
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
	}
	addDisk := func(disk string, parts ...string) {
		dir := filepath.Join(sysRoot, "devices", "pci", disk)
		mkdir(dir)
		link(dir, filepath.Join(block, disk))
		for _, part := range parts {
			touch(filepath.Join(dir, part, "partition"))
			link(filepath.Join(dir, part), filepath.Join(block, part))
		}
	}

	addDisk("nvme0n1", "nvme0n1p1")
	addDisk("sdb", "sdb1")
	addDisk("sdc", "sdc1")

	// dm-0 is a LUKS mapping on sdc1, reachable as /dev/mapper/crypt1.
	touch(filepath.Join(block, "dm-0", "slaves", "sdc1"))
	touch(filepath.Join(devRoot, "dm-0"))
	link(filepath.Join(devRoot, "dm-0"), filepath.Join(devRoot, "mapper", "crypt1"))

	// A by-id link to sdb1.
	touch(filepath.Join(devRoot, "sdb1"))
	link(filepath.Join(devRoot, "sdb1"), filepath.Join(devRoot, "disk", "by-id", "ata-MODEL-part1"))

	// dm-1 -> dm-2 -> dm-3 -> dm-4: deeper than the resolver follows.
	for i, next := range []string{"dm-2", "dm-3", "dm-4", "dm-5"} {
		touch(filepath.Join(block, "dm-"+string(rune('1'+i)), "slaves", next))
	}
	// dm-9 names a slave that does not exist.
	touch(filepath.Join(block, "dm-9", "slaves", "ghost"))

	return sysRoot, devRoot
}

func TestResolveWholeDisk(t *testing.T) {
	sysRoot, devRoot := fakeSysfs(t)

	tests := []struct {
		member string
		want   string
	}{
		{"nvme0n1p1", "nvme0n1"},
		{"sdb", "sdb"},
		{"sdb1", "sdb"},
		{"/dev/sdb1", "sdb"},
		{"crypt1", "sdc"},
		{"dm-0", "sdc"},
		{"ata-MODEL-part1", "sdb"},
		{"12345678901234567890", ""}, // missing device shown by GUID
		{"bad name!", ""},
		{"dm-1", ""}, // too many layers
		{"dm-9", ""}, // dangling slave
	}
	for _, tt := range tests {
		if got := resolveWholeDisk(sysRoot, devRoot, tt.member); got != tt.want {
			t.Errorf("resolveWholeDisk(%q) = %q, want %q", tt.member, got, tt.want)
		}
	}
}

func TestPoolDrives(t *testing.T) {
	pool := &dto.ZFSPool{VDEVs: []dto.ZFSVdev{
		{Name: "raidz2-0", Type: "raidz2", Devices: []dto.ZFSDevice{
			{Name: "sdb1", State: "ONLINE"},
			{Name: "nvme0n1p1", State: "DEGRADED"},
		}},
		{Name: "sdc1", Type: "disk", State: "ONLINE"},
		{Name: "mirror-1", Type: "mirror", State: "ONLINE"}, // no devices parsed
	}}
	want := []dto.ZFSDrive{
		{Name: "sdb1", Vdev: "raidz2-0", State: "ONLINE"},
		{Name: "nvme0n1p1", Vdev: "raidz2-0", State: "DEGRADED"},
		{Name: "sdc1", State: "ONLINE"},
	}
	if got := poolDrives(pool); !reflect.DeepEqual(got, want) {
		t.Errorf("poolDrives() = %+v, want %+v", got, want)
	}
	if got := poolDrives(&dto.ZFSPool{}); got != nil {
		t.Errorf("poolDrives(no vdevs) = %+v, want nil", got)
	}
}

// fakeSmartctl serves smartctl output per disk and records the calls.
type fakeSmartctl struct {
	mu       sync.Mutex
	outputs  map[string]string
	calls    []string
	inflight map[string]chan struct{}
}

func (f *fakeSmartctl) run(_ time.Duration, disk string) (string, <-chan struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, disk)
	done := f.inflight[disk]
	if done == nil {
		done = make(chan struct{})
		close(done)
	}
	out, ok := f.outputs[disk]
	if !ok {
		return "", done, errors.New("exit status 2")
	}
	return out, done, nil
}

func (f *fakeSmartctl) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

func newTestCache(t *testing.T, runner *fakeSmartctl, clock *time.Time) *driveHealthCache {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "smartctl")
	if err := os.WriteFile(binary, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	cache := newDriveHealthCache()
	cache.binary = binary
	cache.workers = 2
	cache.now = func() time.Time { return *clock }
	cache.run = runner.run
	return cache
}

func sameSet(a, b []string) bool {
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return len(a) == len(b)
}

func TestDriveHealthCacheLookup(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	start := clock
	runner := &fakeSmartctl{outputs: map[string]string{
		"nvme0n1": readSmartctlFixture(t, "nvme.json"),
		"sdb":     readSmartctlFixture(t, "scsi.json"),
		"sdx":     readSmartctlFixture(t, "standby.json"),
	}}
	cache := newTestCache(t, runner, &clock)

	// First lookup reads every distinct, valid disk once.
	got := cache.lookup([]string{"nvme0n1", "sdb", "nvme0n1", "sdx", "sdfail", "bad/name"})
	if calls := runner.takeCalls(); !sameSet(calls, []string{"nvme0n1", "sdb", "sdx", "sdfail"}) {
		t.Fatalf("calls = %v", calls)
	}
	if got["nvme0n1"] == nil || *got["nvme0n1"].PercentageUsed != 89 || got["nvme0n1"].Standby {
		t.Errorf("nvme0n1 = %+v", got["nvme0n1"])
	}
	if got["sdb"] == nil || got["sdb"].MediaType != "hdd" {
		t.Errorf("sdb = %+v", got["sdb"])
	}
	if want := (&dto.DriveHealth{Standby: true}); !reflect.DeepEqual(got["sdx"], want) {
		t.Errorf("sdx (asleep, never read) = %+v, want %+v", got["sdx"], want)
	}
	if _, ok := got["sdfail"]; ok {
		t.Errorf("sdfail should be absent, got %+v", got["sdfail"])
	}

	// Within the TTL nothing is re-read.
	clock = clock.Add(time.Minute)
	cache.lookup([]string{"nvme0n1", "sdb", "sdx", "sdfail"})
	if calls := runner.takeCalls(); len(calls) != 0 {
		t.Fatalf("calls within TTL = %v", calls)
	}

	// After the TTL: sdb is now asleep and keeps its last reading; nvme0n1
	// fails and keeps its last reading; sdfail now reads; sdx is dropped.
	runner.mu.Lock()
	runner.outputs["sdb"] = readSmartctlFixture(t, "standby.json")
	delete(runner.outputs, "nvme0n1")
	runner.outputs["sdfail"] = readSmartctlFixture(t, "ata.json")
	runner.mu.Unlock()
	clock = clock.Add(driveHealthTTL)
	got = cache.lookup([]string{"nvme0n1", "sdb", "sdfail"})
	if calls := runner.takeCalls(); !sameSet(calls, []string{"nvme0n1", "sdb", "sdfail"}) {
		t.Fatalf("calls after TTL = %v", calls)
	}
	if h := got["sdb"]; h == nil || !h.Standby || h.MediaType != "hdd" || !h.CollectedAt.Equal(start) {
		t.Errorf("sdb (asleep after a read) = %+v", h)
	}
	if h := got["nvme0n1"]; h == nil || h.Standby || !h.CollectedAt.Equal(start) {
		t.Errorf("nvme0n1 (failed re-read) = %+v", h)
	}
	if h := got["sdfail"]; h == nil || h.Protocol != "ATA" {
		t.Errorf("sdfail = %+v", h)
	}
	if _, ok := cache.entries["sdx"]; ok {
		t.Error("sdx entry should have been pruned")
	}

	// sdb wakes up: standby clears.
	runner.mu.Lock()
	runner.outputs["sdb"] = readSmartctlFixture(t, "scsi.json")
	runner.mu.Unlock()
	clock = clock.Add(driveHealthTTL)
	got = cache.lookup([]string{"sdb"})
	if h := got["sdb"]; h == nil || h.Standby || !h.CollectedAt.Equal(clock) {
		t.Errorf("sdb (awake again) = %+v", h)
	}
	runner.takeCalls()
}

func TestDriveHealthCacheSkipsStuckSmartctl(t *testing.T) {
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	stuck := make(chan struct{})
	runner := &fakeSmartctl{
		outputs:  map[string]string{"sdb": readSmartctlFixture(t, "scsi.json")},
		inflight: map[string]chan struct{}{"sdb": stuck},
	}
	cache := newTestCache(t, runner, &clock)

	cache.lookup([]string{"sdb"})
	runner.takeCalls()

	clock = clock.Add(driveHealthTTL)
	got := cache.lookup([]string{"sdb"})
	if calls := runner.takeCalls(); len(calls) != 0 {
		t.Fatalf("smartctl re-run while the previous one is stuck: %v", calls)
	}
	if got["sdb"] == nil {
		t.Error("cached reading should still be returned")
	}

	close(stuck)
	cache.lookup([]string{"sdb"})
	if calls := runner.takeCalls(); len(calls) != 1 {
		t.Fatalf("calls after the stuck run exited = %v", calls)
	}
}

func TestDriveHealthCacheWithoutSmartctl(t *testing.T) {
	cache := newDriveHealthCache()
	cache.binary = filepath.Join(t.TempDir(), "missing")
	cache.run = func(time.Duration, string) (string, <-chan struct{}, error) {
		t.Fatal("smartctl must not run when the binary is missing")
		return "", nil, nil
	}
	if got := cache.lookup([]string{"sdb"}); got != nil {
		t.Errorf("lookup() = %v, want nil", got)
	}
}

func TestRunSmartctlReportsStartFailure(t *testing.T) {
	// smartctl is not installed on the test host (or /dev/zz-none does not
	// exist), so either the start or the run fails.
	if _, _, err := runSmartctl(time.Second, "zz-none"); err == nil {
		t.Error("runSmartctl() on a missing device returned no error")
	}
}

func TestAttachDrives(t *testing.T) {
	sysRoot, devRoot := fakeSysfs(t)
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	runner := &fakeSmartctl{outputs: map[string]string{
		"nvme0n1": readSmartctlFixture(t, "nvme.json"),
		"sdb":     readSmartctlFixture(t, "scsi.json"),
	}}
	collector := &ZFSCollector{sysRoot: sysRoot, devRoot: devRoot, driveHealth: newTestCache(t, runner, &clock)}

	pools := []dto.ZFSPool{
		{Name: "fast", VDEVs: []dto.ZFSVdev{{Name: "mirror-0", Type: "mirror", Devices: []dto.ZFSDevice{
			{Name: "nvme0n1p1", State: "ONLINE"},
			{Name: "9876543210", State: "UNAVAIL"},
		}}}},
		{Name: "slow", VDEVs: []dto.ZFSVdev{{Name: "sdb1", Type: "disk", State: "ONLINE"}}},
		{Name: "unknown"},
	}
	collector.attachDrives(pools)

	fast := pools[0].Drives
	if len(fast) != 2 || fast[0].Disk != "nvme0n1" || fast[0].Health == nil || *fast[0].Health.PercentageUsed != 89 {
		t.Errorf("fast drives = %+v", fast)
	}
	if fast[1].Disk != "" || fast[1].Health != nil {
		t.Errorf("missing member = %+v", fast[1])
	}
	slow := pools[1].Drives
	if len(slow) != 1 || slow[0].Disk != "sdb" || slow[0].Vdev != "" || slow[0].Health == nil || slow[0].Health.Protocol != "SCSI" {
		t.Errorf("slow drives = %+v", slow)
	}
	if pools[2].Drives != nil {
		t.Errorf("pool without status should have no drives, got %+v", pools[2].Drives)
	}

	// Without a health cache the members are still listed.
	pools = []dto.ZFSPool{{Name: "slow", VDEVs: []dto.ZFSVdev{{Name: "sdb1", Type: "disk", State: "ONLINE"}}}}
	(&ZFSCollector{sysRoot: sysRoot, devRoot: devRoot}).attachDrives(pools)
	if d := pools[0].Drives; len(d) != 1 || d[0].Disk != "sdb" || d[0].Health != nil {
		t.Errorf("drives without health cache = %+v", d)
	}
}

// TestCollectPoolsAttachesDrives runs collectPools against canned zpool
// output and checks each pool comes back with its members and their health.
func TestCollectPoolsAttachesDrives(t *testing.T) {
	sysRoot, devRoot := fakeSysfs(t)
	clock := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	runner := &fakeSmartctl{outputs: map[string]string{"nvme0n1": readSmartctlFixture(t, "nvme.json")}}

	status := "  pool: fast\n state: ONLINE\nconfig:\n\n\tNAME           STATE     READ WRITE CKSUM\n" +
		"\tfast           ONLINE       0     0     0\n" +
		"\t  mirror-0     ONLINE       0     0     0\n" +
		"\t    nvme0n1p1  ONLINE       0     0     0\n" +
		"\t    sdb1       ONLINE       0     0     0\n\nerrors: No known data errors\n"
	exec := func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "list":
			if len(args) == 4 { // list -H -o name
				return "fast\n", nil
			}
			return "fast\t1000\t400\t600\t5\t40\t1.00\tONLINE\t-\n", nil
		case "get":
			return "guid\t42\nautotrim\ton\n", nil
		case "status":
			return status, nil
		}
		return "", errors.New("unexpected zpool call")
	}
	collector := &ZFSCollector{execOutput: exec, sysRoot: sysRoot, devRoot: devRoot,
		driveHealth: newTestCache(t, runner, &clock)}

	pools, err := collector.collectPools()
	if err != nil {
		t.Fatalf("collectPools() error = %v", err)
	}
	if len(pools) != 1 || pools[0].GUID != "42" {
		t.Fatalf("pools = %+v", pools)
	}
	drives := pools[0].Drives
	if len(drives) != 2 || drives[0].Disk != "nvme0n1" || drives[0].Vdev != "mirror-0" ||
		drives[0].Health == nil || drives[1].Disk != "sdb" || drives[1].Health != nil {
		t.Errorf("drives = %+v", drives)
	}
}
