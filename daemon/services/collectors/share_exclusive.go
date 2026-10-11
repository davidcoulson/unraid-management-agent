package collectors

import (
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

// defaultMntRoot is where Unraid mounts array disks and pools.
const defaultMntRoot = "/mnt"

// sharePaths holds the file locations the share collector reads. Empty
// fields fall back to the production defaults so a zero-value ShareCollector
// keeps working; tests point them at fixture files.
type sharePaths struct {
	mntRoot   string
	sharesIni string
	varIni    string
	disksIni  string
}

func (p sharePaths) mnt() string {
	if p.mntRoot == "" {
		return defaultMntRoot
	}
	return p.mntRoot
}

func (p sharePaths) shares() string {
	if p.sharesIni == "" {
		return constants.SharesIni
	}
	return p.sharesIni
}

func (p sharePaths) vars() string {
	if p.varIni == "" {
		return constants.VarIni
	}
	return p.varIni
}

func (p sharePaths) disks() string {
	if p.disksIni == "" {
		return constants.DisksIni
	}
	return p.disksIni
}

// applyExclusiveState completes the exclusive-share fields (Unraid 6.12+).
//
// With exclusive shares enabled globally, Unraid replaces /mnt/user/<share>
// with a symlink to /mnt/<pool>/<share> for every share that lives on a single
// pool, so I/O bypasses the FUSE user share layer (shfs). If a top-level folder
// for the share also exists on another pool or array disk, Unraid silently
// keeps the share on FUSE instead.
//
// Exclusive comes from the "exclusive" flag emhttp writes to shares.ini when
// it sets up the symlink, so nothing under /mnt/user (FUSE) is touched. The
// fields are cleared when the release has no exclusive-share setting or the
// user share file system is not mounted (array stopped, user shares disabled).
func (p sharePaths) applyExclusiveState(shares []dto.ShareInfo) {
	settings, supported := p.exclusiveSettings()
	if !supported || !settings.userSharesMounted {
		for i := range shares {
			shares[i].Exclusive = nil
		}
		return
	}

	var branches []storageBranch // loaded lazily: only needed for shares on FUSE
	for i := range shares {
		share := &shares[i]
		if share.Exclusive == nil || validateShareName(share.Name) != nil {
			share.Exclusive = nil
			continue
		}
		exclusive := *share.Exclusive
		eligible := settings.enabled && share.UseCache == "only" && share.CachePool != "" &&
			share.CachePool2 == "" && (!settings.nfsBlocks || !share.NFSExport)
		share.ExclusiveEligible = &eligible
		if exclusive {
			share.ExclusivePool = share.CachePool
		}

		if eligible && !exclusive {
			if branches == nil {
				branches = p.storageBranches(settings)
			}
			share.Locations, share.LocationsUnchecked = p.shareLocations(branches, share.Name)
		}
	}
}

// exclusiveSettings holds the var.ini settings the exclusive share check uses.
type exclusiveSettings struct {
	enabled           bool // "Permit exclusive shares"
	nfsBlocks         bool // release refuses exclusive access for NFS exported shares
	userSharesMounted bool // user shares enabled and the array started
	include           map[string]bool
	exclude           map[string]bool
}

// exclusiveSettings reads the global share settings from var.ini. supported
// is false when the exclusive share key is absent (Unraid before 6.12).
// Unraid lifted the NFS export restriction in 7.2.0.
func (p sharePaths) exclusiveSettings() (exclusiveSettings, bool) {
	cfg, err := ini.Load(p.vars())
	if err != nil {
		logger.Debug("Share: cannot read %s for exclusive share setting: %v", p.vars(), err)
		return exclusiveSettings{}, false
	}
	section := cfg.Section("")
	if !section.HasKey("shareUserExclusive") {
		return exclusiveSettings{}, false
	}
	return exclusiveSettings{
		enabled:   section.Key("shareUserExclusive").String() == "yes",
		nfsBlocks: releaseBefore(section.Key("version").String(), 7, 2),
		userSharesMounted: section.Key("shareUser").String() == "e" &&
			section.Key("fsState").String() == "Started",
		include: diskSet(section.Key("shareUserInclude").String()),
		exclude: diskSet(section.Key("shareUserExclude").String()),
	}, true
}

// diskSet parses a comma-separated disk list such as "disk1,disk3".
func diskSet(list string) map[string]bool {
	set := map[string]bool{}
	for disk := range strings.SplitSeq(list, ",") {
		if disk = strings.TrimSpace(disk); disk != "" {
			set[disk] = true
		}
	}
	return set
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

// storageBranch is an array disk or pool that is part of the user share
// file system.
type storageBranch struct {
	name string
	// asleep is set when the disk, or any device of the pool, is spun down.
	// Its folders are not probed so the check never wakes a disk.
	asleep bool
}

// storageBranches returns the mounted array disks and pools that make up the
// user share file system, from disks.ini: mounted Data disks allowed by the
// global included/excluded disk settings and mounted pools that are not
// excluded from user shares. Pool member devices (cache2, ...) carry no
// fsStatus and flash/parity entries have other types, so they never match.
func (p sharePaths) storageBranches(settings exclusiveSettings) []storageBranch {
	// #nosec G304 -- path is the fixed emhttp disks.ini (overridable only in tests)
	data, err := os.ReadFile(p.disks())
	if err != nil {
		logger.Debug("Share: cannot read %s for share locations: %v", p.disks(), err)
		return []storageBranch{}
	}
	sections := parseINISections(string(data))

	// Pool devices follow the pool entry in slot order, so a pool spans the
	// idx range [idx, idx+slots).
	spunDown := map[int]bool{}
	for _, kv := range sections {
		if idx, err := strconv.Atoi(kv["idx"]); err == nil {
			spunDown[idx] = kv["spundown"] == "1"
		}
	}

	branches := []storageBranch{}
	for section, kv := range sections {
		name := kv["name"]
		if name == "" {
			name = section
		}
		if kv["fsStatus"] != "Mounted" || validateShareName(name) != nil {
			continue
		}
		switch kv["type"] {
		case "Data":
			if (len(settings.include) > 0 && !settings.include[name]) || settings.exclude[name] {
				continue
			}
		case "Cache":
			if kv["shareEnabled"] == "no" {
				continue
			}
		default:
			continue
		}
		branches = append(branches, storageBranch{name: name, asleep: branchAsleep(kv, spunDown)})
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].name < branches[j].name })
	return branches
}

// branchAsleep reports whether a disk or any device of a pool is spun down.
func branchAsleep(kv map[string]string, spunDown map[int]bool) bool {
	idx, err := strconv.Atoi(kv["idx"])
	if err != nil {
		return kv["spundown"] == "1"
	}
	slots, err := strconv.Atoi(kv["slots"])
	if err != nil || slots < 1 {
		slots = 1
	}
	for i := idx; i < idx+slots; i++ {
		if spunDown[i] {
			return true
		}
	}
	return false
}

// shareLocations lists the awake disks and pools holding a top-level folder
// for the share (one lstat of /mnt/<branch>/<name> each) and the sleeping
// ones that were left unchecked.
func (p sharePaths) shareLocations(branches []storageBranch, name string) (found, unchecked []string) {
	found = []string{}
	for _, branch := range branches {
		if branch.asleep {
			unchecked = append(unchecked, branch.name)
			continue
		}
		info, err := os.Lstat(filepath.Join(p.mnt(), branch.name, name))
		if err == nil && info.IsDir() {
			found = append(found, branch.name)
		}
	}
	return found, unchecked
}
