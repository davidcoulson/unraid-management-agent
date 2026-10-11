package collectors

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tinkerologie/unraid-management-agent/daemon/dto"
)

const exclusiveDisksIni = `["parity"]
name="parity"
type="Parity"
status="DISK_NP"
["disk1"]
name="disk1"
type="Data"
fsStatus="Mounted"
["disk2"]
name="disk2"
type="Data"
fsStatus="Mounted"
["disk3"]
name="disk3"
type="Data"
fsStatus="Unmountable: unsupported or no file system"
["cache"]
name="cache"
type="Cache"
fsStatus="Mounted"
["cache2"]
name="cache2"
type="Cache"
["nvme"]
type="Cache"
fsStatus="Mounted"
["flash"]
name="flash"
type="Boot"
fsStatus="Mounted"
`

// exclusiveFixture builds a fake /mnt tree, var.ini and disks.ini in a temp dir.
func exclusiveFixture(t *testing.T, varIni string) exclusivePaths {
	t.Helper()
	root := t.TempDir()
	mnt := filepath.Join(root, "mnt")
	for _, dir := range []string{
		"user/media",       // FUSE share (not eligible)
		"user/appdata",     // FUSE fallback: stray folder on disk1
		"cache/appdata",    // primary pool copy
		"disk1/appdata",    // stray copy
		"disk2",            // holds a file named appdata
		"cache/system",     // target of an exclusive symlink
		"nvme/vms",         // target of an exclusive symlink
		"cache2/appdata",   // pool member device, never a branch
		"flash/appdata",    // boot device, never a branch
		"disk3/appdata",    // unmountable disk, never a branch
		"cache/downloads",  // FUSE fallback with no stray folder found
		"user/downloads",   // ...
		"elsewhere/other1", // odd symlink target
	} {
		if err := os.MkdirAll(filepath.Join(mnt, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A plain file named after the share is not a folder and does not count.
	if err := os.WriteFile(filepath.Join(mnt, "disk2", "appdata"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"system": "../cache/system",
		"vms":    filepath.Join(mnt, "nvme", "vms"),
		"other":  "../elsewhere/other1/deeper",
	} {
		if err := os.Symlink(target, filepath.Join(mnt, "user", link)); err != nil {
			t.Fatal(err)
		}
	}

	p := exclusivePaths{
		mntRoot:  mnt,
		varIni:   filepath.Join(root, "var.ini"),
		disksIni: filepath.Join(root, "disks.ini"),
	}
	if err := os.WriteFile(p.disksIni, []byte(exclusiveDisksIni), 0o600); err != nil {
		t.Fatal(err)
	}
	if varIni != "" {
		if err := os.WriteFile(p.varIni, []byte(varIni), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func poolOnly(name, pool string) dto.ShareInfo {
	return dto.ShareInfo{Name: name, UseCache: "only", CachePool: pool}
}

func boolPtr(b bool) *bool { return &b }

func TestApplyExclusiveState(t *testing.T) {
	p := exclusiveFixture(t, "shareUser=\"e\"\nshareUserExclusive=\"yes\"\n")
	shares := []dto.ShareInfo{
		poolOnly("system", "cache"),
		poolOnly("vms", "nvme"),
		poolOnly("appdata", "cache"),
		poolOnly("downloads", "cache"),
		{Name: "media", UseCache: "yes", CachePool: "cache"},
		{Name: "backup", UseCache: "only", CachePool: "cache", CachePool2: "disk1"},
		poolOnly("missing", "cache"),
		poolOnly("other", "cache"),
		poolOnly("../escape", "cache"),
	}
	p.applyExclusiveState(shares)

	want := []struct {
		exclusive, eligible *bool
		pool                string
		locations           []string
	}{
		{boolPtr(true), boolPtr(true), "cache", nil},
		{boolPtr(true), boolPtr(true), "nvme", nil},
		{boolPtr(false), boolPtr(true), "", []string{"cache", "disk1"}},
		{boolPtr(false), boolPtr(true), "", []string{"cache"}},
		{boolPtr(false), boolPtr(false), "", nil},
		{boolPtr(false), boolPtr(false), "", nil},
		{boolPtr(false), boolPtr(true), "", []string{}},
		{boolPtr(true), boolPtr(true), "", nil},
		{nil, nil, "", nil},
	}
	for i, w := range want {
		got := shares[i]
		if !reflect.DeepEqual(got.Exclusive, w.exclusive) ||
			!reflect.DeepEqual(got.ExclusiveEligible, w.eligible) ||
			got.ExclusivePool != w.pool ||
			!reflect.DeepEqual(got.Locations, w.locations) {
			t.Errorf("%s: got exclusive=%v eligible=%v pool=%q locations=%#v, want %v %v %q %#v",
				got.Name, deref(got.Exclusive), deref(got.ExclusiveEligible), got.ExclusivePool, got.Locations,
				deref(w.exclusive), deref(w.eligible), w.pool, w.locations)
		}
	}
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func TestApplyExclusiveStateGloballyDisabled(t *testing.T) {
	p := exclusiveFixture(t, "shareUserExclusive=\"no\"\n")
	shares := []dto.ShareInfo{poolOnly("appdata", "cache")}
	p.applyExclusiveState(shares)
	if shares[0].Exclusive == nil || *shares[0].Exclusive || shares[0].ExclusiveEligible == nil || *shares[0].ExclusiveEligible {
		t.Fatalf("want exclusive=false eligible=false, got %+v", shares[0])
	}
	if shares[0].Locations != nil {
		t.Errorf("locations reported for an ineligible share: %v", shares[0].Locations)
	}
}

func TestApplyExclusiveStateUnavailable(t *testing.T) {
	tests := map[string]func(t *testing.T) exclusivePaths{
		"no var.ini": func(t *testing.T) exclusivePaths { return exclusiveFixture(t, "") },
		"pre-6.12 var.ini": func(t *testing.T) exclusivePaths {
			return exclusiveFixture(t, "shareUser=\"e\"\n")
		},
		"user shares not mounted": func(t *testing.T) exclusivePaths {
			p := exclusiveFixture(t, "shareUserExclusive=\"yes\"\n")
			if err := os.RemoveAll(filepath.Join(p.mntRoot, "user")); err != nil {
				t.Fatal(err)
			}
			return p
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			shares := []dto.ShareInfo{poolOnly("system", "cache")}
			setup(t).applyExclusiveState(shares)
			if shares[0].Exclusive != nil || shares[0].ExclusiveEligible != nil {
				t.Errorf("fields set although state is unknown: %+v", shares[0])
			}
		})
	}
}

func TestApplyExclusiveStateNoDisksIni(t *testing.T) {
	p := exclusiveFixture(t, "shareUserExclusive=\"yes\"\n")
	if err := os.Remove(p.disksIni); err != nil {
		t.Fatal(err)
	}
	shares := []dto.ShareInfo{poolOnly("appdata", "cache"), poolOnly("downloads", "cache")}
	p.applyExclusiveState(shares)
	for _, s := range shares {
		if s.Exclusive == nil || *s.Exclusive || len(s.Locations) != 0 {
			t.Errorf("%s: want FUSE with no known locations, got %+v", s.Name, s)
		}
	}
}

func TestApplyExclusiveStateLstatError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	p := exclusiveFixture(t, "shareUserExclusive=\"yes\"\n")
	userRoot := filepath.Join(p.mntRoot, "user")
	if err := os.Chmod(userRoot, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(userRoot, 0o755) }) // #nosec G302 -- restore test dir for cleanup

	shares := []dto.ShareInfo{poolOnly("system", "cache")}
	p.applyExclusiveState(shares)
	if shares[0].Exclusive != nil {
		t.Errorf("exclusive reported although /mnt/user/system could not be inspected: %v", *shares[0].Exclusive)
	}
}

func TestApplyExclusiveStateNFSBeforeRelease72(t *testing.T) {
	nfsShare := poolOnly("appdata", "cache")
	nfsShare.NFSExport = true
	for _, tt := range []struct {
		version  string
		eligible bool
	}{
		{"6.12.10", false},
		{"7.1.4", false},
		{"7.2.0-rc.1", true},
		{"7.4.0", true},
	} {
		p := exclusiveFixture(t, "shareUserExclusive=\"yes\"\nversion=\""+tt.version+"\"\n")
		shares := []dto.ShareInfo{nfsShare, poolOnly("downloads", "cache")}
		p.applyExclusiveState(shares)
		if got := *shares[0].ExclusiveEligible; got != tt.eligible {
			t.Errorf("%s: NFS share eligible=%v, want %v", tt.version, got, tt.eligible)
		}
		if !*shares[1].ExclusiveEligible {
			t.Errorf("%s: non-NFS share not eligible", tt.version)
		}
	}
}

func TestReleaseBefore(t *testing.T) {
	for version, want := range map[string]bool{
		"6.12.10":    true,
		"7.1.4":      true,
		"7.2":        false,
		"7.2.0-rc.1": false,
		"7.10.0":     false,
		"8.0.0":      false,
		"7.1-beta":   true,
		"":           false,
		"7":          false,
		"x.y.z":      false,
	} {
		if got := releaseBefore(version, 7, 2); got != want {
			t.Errorf("releaseBefore(%q, 7, 2) = %v, want %v", version, got, want)
		}
	}
}

func TestExclusivePathsDefaults(t *testing.T) {
	var p exclusivePaths
	if p.mnt() != "/mnt" || p.vars() != "/var/local/emhttp/var.ini" || p.disks() != "/var/local/emhttp/disks.ini" {
		t.Errorf("unexpected defaults: %s %s %s", p.mnt(), p.vars(), p.disks())
	}
}
