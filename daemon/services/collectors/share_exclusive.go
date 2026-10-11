package collectors

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/ini.v1"

	"github.com/tinkerologie/unraid-management-agent/daemon/constants"
	"github.com/tinkerologie/unraid-management-agent/daemon/dto"
	"github.com/tinkerologie/unraid-management-agent/daemon/logger"
)

// defaultMntRoot is where Unraid mounts array disks, pools and the user share
// file system (/mnt/user).
const defaultMntRoot = "/mnt"

// exclusivePaths holds the file system locations the exclusive-share check
// reads. Empty fields fall back to the production defaults so a zero-value
// ShareCollector keeps working; tests point them at fixture directories.
type exclusivePaths struct {
	mntRoot  string
	varIni   string
	disksIni string
}

func (p exclusivePaths) mnt() string {
	if p.mntRoot == "" {
		return defaultMntRoot
	}
	return p.mntRoot
}

func (p exclusivePaths) vars() string {
	if p.varIni == "" {
		return constants.VarIni
	}
	return p.varIni
}

func (p exclusivePaths) disks() string {
	if p.disksIni == "" {
		return constants.DisksIni
	}
	return p.disksIni
}

// applyExclusiveState fills in the exclusive-share fields (Unraid 6.12+).
//
// With exclusive shares enabled globally, Unraid replaces /mnt/user/<share>
// with a symlink to /mnt/<pool>/<share> for every share that lives on a single
// pool, so I/O bypasses the FUSE user share layer (shfs). If a top-level folder
// for the share also exists on another pool or array disk, Unraid silently
// keeps the share on FUSE instead. This reports which of the two happened.
//
// Only lstat/readlink are used on /mnt/user, so nothing is read through FUSE.
// The fields stay unset when the release has no exclusive-share setting or the
// user share file system is not mounted (array stopped, user shares disabled).
func (p exclusivePaths) applyExclusiveState(shares []dto.ShareInfo) {
	globalEnabled, nfsBlocks, supported := p.exclusiveSetting()
	if !supported {
		return
	}

	userRoot := filepath.Join(p.mnt(), "user")
	if info, err := os.Stat(userRoot); err != nil || !info.IsDir() {
		logger.Debug("Share: %s not available, skipping exclusive share check", userRoot)
		return
	}

	var branches []string // loaded lazily: only needed for shares on FUSE
	for i := range shares {
		share := &shares[i]
		if validateShareName(share.Name) != nil {
			continue
		}

		exclusive, pool, ok := p.shareLink(userRoot, share.Name)
		if !ok {
			continue
		}
		eligible := globalEnabled && share.UseCache == "only" && share.CachePool != "" &&
			share.CachePool2 == "" && (!nfsBlocks || !share.NFSExport)

		share.Exclusive = &exclusive
		share.ExclusiveEligible = &eligible
		share.ExclusivePool = pool

		if eligible && !exclusive {
			if branches == nil {
				branches = p.storageBranches()
			}
			share.Locations = p.shareLocations(branches, share.Name)
		}
	}
}

// exclusiveSetting reads the global "Permit exclusive shares" setting from
// var.ini. supported is false when the key is absent (Unraid before 6.12).
// nfsBlocks reports whether this release refuses exclusive access for NFS
// exported shares, a restriction Unraid lifted in 7.2.0.
func (p exclusivePaths) exclusiveSetting() (enabled, nfsBlocks, supported bool) {
	cfg, err := ini.Load(p.vars())
	if err != nil {
		logger.Debug("Share: cannot read %s for exclusive share setting: %v", p.vars(), err)
		return false, false, false
	}
	section := cfg.Section("")
	if !section.HasKey("shareUserExclusive") {
		return false, false, false
	}
	enabled = section.Key("shareUserExclusive").String() == "yes"
	return enabled, releaseBefore(section.Key("version").String(), 7, 2), true
}

// releaseBefore reports whether an Unraid version string such as
// "6.12.10" or "7.2.0-rc.1" is older than major.minor. Unparsable versions
// count as current.
func releaseBefore(version string, major, minor int) bool {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	gotMajor, errMajor := strconv.Atoi(parts[0])
	gotMinor, errMinor := strconv.Atoi(strings.SplitN(parts[1], "-", 2)[0])
	if errMajor != nil || errMinor != nil {
		return false
	}
	return gotMajor < major || (gotMajor == major && gotMinor < minor)
}

// shareLink inspects /mnt/user/<name> without following it. A symlink means
// the share is exclusive; pool is the pool it points at. ok is false when the
// entry cannot be inspected for a reason other than not existing.
func (p exclusivePaths) shareLink(userRoot, name string) (exclusive bool, pool string, ok bool) {
	path := filepath.Join(userRoot, name)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, "", true
		}
		logger.Debug("Share: cannot lstat %s: %v", path, err)
		return false, "", false
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return false, "", true
	}

	target, err := os.Readlink(path)
	if err != nil {
		logger.Debug("Share: cannot read link %s: %v", path, err)
		return true, "", true
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(userRoot, target)
	}
	// Expect /mnt/<pool>/<name>; anything else leaves the pool unknown.
	target = filepath.Clean(target)
	if filepath.Dir(filepath.Dir(target)) == filepath.Clean(p.mnt()) {
		pool = filepath.Base(filepath.Dir(target))
	}
	return true, pool, true
}

// storageBranches returns the mounted array disks and pools that make up the
// user share file system, from disks.ini. Pool member devices (cache2, ...)
// carry no fsStatus and the flash/parity entries have other types, so only
// mounted Data and Cache entries are kept.
func (p exclusivePaths) storageBranches() []string {
	// #nosec G304 -- path is the fixed emhttp disks.ini (overridable only in tests)
	data, err := os.ReadFile(p.disks())
	if err != nil {
		logger.Debug("Share: cannot read %s for share locations: %v", p.disks(), err)
		return []string{}
	}
	branches := []string{}
	for section, kv := range parseINISections(string(data)) {
		name := kv["name"]
		if name == "" {
			name = section
		}
		if kv["fsStatus"] != "Mounted" || validateShareName(name) != nil {
			continue
		}
		switch kv["type"] {
		case "Data", "Cache":
			branches = append(branches, name)
		}
	}
	sort.Strings(branches)
	return branches
}

// shareLocations lists the disks and pools holding a top-level folder for the
// share. Each check is a single lstat of /mnt/<branch>/<name>.
func (p exclusivePaths) shareLocations(branches []string, name string) []string {
	locations := []string{}
	for _, branch := range branches {
		info, err := os.Lstat(filepath.Join(p.mnt(), branch, name))
		if err == nil && info.IsDir() {
			locations = append(locations, branch)
		}
	}
	return locations
}
