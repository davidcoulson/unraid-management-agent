package collectors

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ruaan-deysel/unraid-management-agent/daemon/constants"
	"github.com/ruaan-deysel/unraid-management-agent/daemon/domain"
	"github.com/ruaan-deysel/unraid-management-agent/daemon/dto"
	"github.com/ruaan-deysel/unraid-management-agent/daemon/lib"
	"github.com/ruaan-deysel/unraid-management-agent/daemon/logger"
)

// ZFSCollector collects ZFS pool, dataset, and ARC statistics
type ZFSCollector struct {
	ctx *domain.Context

	// execOutput runs `zpool status`; injectable so tests can feed captured output.
	execOutput func(command string, args ...string) (string, error)
}

// NewZFSCollector creates a new ZFS collector
func NewZFSCollector(ctx *domain.Context) *ZFSCollector {
	return &ZFSCollector{ctx: ctx, execOutput: lib.ExecCommandOutput}
}

// Start begins the ZFS collection loop
func (c *ZFSCollector) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Info("ZFS collector started (interval: %v)", interval)

	// Collect immediately on start with panic recovery
	func() {
		defer func() {
			if r := recover(); r != nil {
				logger.LogPanicWithStack("ZFS collector", r)
			}
		}()
		collectWithWatchdog(ctx, "ZFS", interval, c.collect)
	}()

	for {
		select {
		case <-ctx.Done():
			logger.Info("ZFS collector stopped")
			return
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.LogPanicWithStack("ZFS collector", r)
					}
				}()
				collectWithWatchdog(ctx, "ZFS", interval, c.collect)
			}()
		}
	}
}

// collect gathers all ZFS data and publishes events
func (c *ZFSCollector) collect() {
	defer func() {
		if r := recover(); r != nil {
			logger.LogPanicWithStack("ZFS collector (collect)", r)
		}
	}()

	// Check if ZFS is available
	if !c.isZFSAvailable() {
		logger.Debug("ZFS not available, skipping collection")
		return
	}

	// Collect pools
	pools, err := c.collectPools()
	if err != nil {
		logger.Warning("Failed to collect ZFS pools: %v", err)
	} else if len(pools) > 0 {
		domain.Publish(c.ctx.Hub, constants.TopicZFSPoolsUpdate, pools)
		logger.Debug("Published ZFS pools update (count: %d)", len(pools))
	}

	// Collect datasets
	datasets, err := c.collectDatasets()
	if err != nil {
		logger.Warning("Failed to collect ZFS datasets: %v", err)
	} else if len(datasets) > 0 {
		domain.Publish(c.ctx.Hub, constants.TopicZFSDatasetsUpdate, datasets)
		logger.Debug("Published ZFS datasets update (count: %d)", len(datasets))
	}

	// Collect snapshots
	snapshots, err := c.collectSnapshots()
	if err != nil {
		logger.Warning("Failed to collect ZFS snapshots: %v", err)
	} else if len(snapshots) > 0 {
		domain.Publish(c.ctx.Hub, constants.TopicZFSSnapshotsUpdate, snapshots)
		logger.Debug("Published ZFS snapshots update (count: %d)", len(snapshots))
	}

	// Collect ARC stats
	arcStats, err := c.collectARCStats()
	if err != nil {
		logger.Warning("Failed to collect ZFS ARC stats: %v", err)
	} else {
		domain.Publish(c.ctx.Hub, constants.TopicZFSARCStatsUpdate, arcStats)
		logger.Debug("Published ZFS ARC stats update")
	}
}

// isZFSAvailable checks if ZFS kernel module is loaded and binaries exist
func (c *ZFSCollector) isZFSAvailable() bool {
	// Check if zpool binary exists
	if _, err := os.Stat(constants.ZpoolBin); os.IsNotExist(err) {
		return false
	}

	// Try to execute zpool list to verify ZFS is functional
	_, err := lib.ExecCommandOutput(constants.ZpoolBin, "list", "-H")
	return err == nil
}

// collectPools collects information about all ZFS pools
func (c *ZFSCollector) collectPools() ([]dto.ZFSPool, error) {
	// Get list of pool names
	output, err := lib.ExecCommandOutput(constants.ZpoolBin, "list", "-H", "-o", "name")
	if err != nil {
		return nil, fmt.Errorf("failed to list pools: %w", err)
	}

	poolNames := strings.Split(strings.TrimSpace(output), "\n")
	if len(poolNames) == 0 || poolNames[0] == "" {
		return []dto.ZFSPool{}, nil
	}

	// Identify the ZFS boot pool (Unraid 7.3 internal boot) once per cycle.
	bootPool := ""
	if boot := lib.DetectBootInfo(); boot != nil {
		bootPool = boot.BootPool
	}

	pools := make([]dto.ZFSPool, 0, len(poolNames))
	for _, name := range poolNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}

		pool, err := c.collectPoolDetails(name)
		if err != nil {
			logger.Warning("Failed to collect pool details for %s: %v", name, err)
			continue
		}

		if bootPool != "" && name == bootPool {
			pool.IsBootPool = true
		}

		pools = append(pools, pool)
	}

	return pools, nil
}

// collectPoolDetails collects detailed information about a specific pool
func (c *ZFSCollector) collectPoolDetails(name string) (dto.ZFSPool, error) {
	pool := dto.ZFSPool{
		Name:      name,
		Timestamp: time.Now(),
	}

	// Get basic pool info (parseable format)
	// Fields: name, size, allocated, free, fragmentation, capacity, dedupratio, health, altroot
	output, err := lib.ExecCommandOutput(constants.ZpoolBin, "list", "-Hp", "-o",
		"name,size,allocated,free,fragmentation,capacity,dedupratio,health,altroot", name)
	if err != nil {
		return pool, fmt.Errorf("failed to get pool info: %w", err)
	}

	// Parse tab-separated values
	fields := strings.Split(strings.TrimSpace(output), "\t")
	if len(fields) < 9 {
		return pool, fmt.Errorf("unexpected pool info format: got %d fields", len(fields))
	}

	pool.SizeBytes, _ = strconv.ParseUint(fields[1], 10, 64)
	pool.AllocatedBytes, _ = strconv.ParseUint(fields[2], 10, 64)
	pool.FreeBytes, _ = strconv.ParseUint(fields[3], 10, 64)

	// Parse fragmentation and capacity (can be "-" if not available)
	if fields[4] != "-" {
		pool.FragmentationPct, _ = strconv.ParseFloat(fields[4], 64)
	}
	if fields[5] != "-" {
		pool.CapacityPct, _ = strconv.ParseFloat(fields[5], 64)
	}

	// Parse dedup ratio (format: "1.00x" or "1.00")
	dedupStr := strings.TrimSuffix(fields[6], "x")
	pool.DedupRatio, _ = strconv.ParseFloat(dedupStr, 64)

	pool.Health = fields[7]

	// Altroot (can be "-" if not set)
	if fields[8] != "-" {
		pool.Altroot = fields[8]
	}

	// Get pool properties for additional details
	if err := c.enrichPoolProperties(&pool); err != nil {
		logger.Warning("Failed to enrich pool properties for %s: %v", name, err)
	}

	// Get pool status (vdevs, errors, scrub info)
	if err := c.parsePoolStatus(&pool); err != nil {
		logger.Warning("Failed to parse pool status for %s: %v", name, err)
	}

	return pool, nil
}

// enrichPoolProperties adds additional properties from 'zpool get all'
func (c *ZFSCollector) enrichPoolProperties(pool *dto.ZFSPool) error {
	output, err := lib.ExecCommandOutput(constants.ZpoolBin, "get", "-Hp", "-o", "property,value",
		"guid,readonly,autoexpand,autotrim", pool.Name)
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}

		property := fields[0]
		value := fields[1]

		switch property {
		case "guid":
			pool.GUID = value
		case "readonly":
			pool.Readonly = value == "on"
		case "autoexpand":
			pool.Autoexpand = value == "on"
		case "autotrim":
			pool.Autotrim = value
		}
	}

	return scanner.Err()
}

// parsePoolStatus parses 'zpool status' output for vdevs, errors, and scrub info
func (c *ZFSCollector) parsePoolStatus(pool *dto.ZFSPool) error {
	output, err := c.execOutput(constants.ZpoolBin, "status", "-v", pool.Name)
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(strings.NewReader(output))
	inConfig := false
	inErrors := false
	inScan := false
	var currentVdev *dto.ZFSVdev

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Tab-indented lines directly after "scan:" carry in-progress details.
		if inScan && strings.HasPrefix(line, "\t") {
			c.parseScanDetail(pool, trimmed)
			continue
		}
		inScan = false

		// Parse state
		if state, found := strings.CutPrefix(trimmed, "state:"); found {
			pool.State = strings.TrimSpace(state)
		}

		// Parse scan/scrub info
		if strings.HasPrefix(trimmed, "scan:") {
			c.parseScanInfo(pool, trimmed)
			inScan = true
		}

		// Parse errors line. When permanent errors exist, `zpool status -v`
		// lists affected files on the following indented lines.
		if errSummary, found := strings.CutPrefix(trimmed, "errors:"); found {
			inConfig = false
			summary := strings.TrimSpace(errSummary)
			inErrors = summary != "" && !strings.Contains(summary, "No known data errors")
			continue
		}

		// Collect corrupted-file paths in the errors section.
		if inErrors {
			if trimmed == "" {
				inErrors = false
				continue
			}
			// Skip the introductory sentence ("Permanent errors have been detected...").
			if strings.HasSuffix(trimmed, ":") || strings.HasPrefix(trimmed, "Permanent errors") {
				continue
			}
			pool.CorruptedFiles = append(pool.CorruptedFiles, trimmed)
			continue
		}

		// Parse config section (vdev tree)
		if strings.HasPrefix(trimmed, "config:") {
			inConfig = true
			continue
		}

		if inConfig && trimmed != "" && !strings.HasPrefix(trimmed, "NAME") {
			// Parse vdev line
			vdev := c.parseVdevLine(line)
			if vdev != nil {
				// Determine if this is a top-level vdev or a device
				indent := len(line) - len(strings.TrimLeft(line, "\t "))

				if indent <= 1 {
					// Top-level vdev (pool itself)
					pool.ReadErrors = vdev.ReadErrors
					pool.WriteErrors = vdev.WriteErrors
					pool.ChecksumErrors = vdev.ChecksumErrors
				} else if indent <= 3 {
					// Mid-level vdev (raidz, mirror, etc.)
					if currentVdev != nil {
						pool.VDEVs = append(pool.VDEVs, *currentVdev)
					}
					currentVdev = vdev
				} else {
					// Device within a vdev
					if currentVdev != nil {
						device := dto.ZFSDevice{
							Name:           vdev.Name,
							State:          vdev.State,
							ReadErrors:     vdev.ReadErrors,
							WriteErrors:    vdev.WriteErrors,
							ChecksumErrors: vdev.ChecksumErrors,
						}
						currentVdev.Devices = append(currentVdev.Devices, device)
					}
				}
			}
		}
	}

	// Add last vdev if exists
	if currentVdev != nil {
		pool.VDEVs = append(pool.VDEVs, *currentVdev)
	}

	return scanner.Err()
}

// Scan line formats printed by print_scan_scrub_resilver_status() in OpenZFS
// zpool_main.c. Timestamps use ctime(3) format in the server's local time zone.
var (
	// "scrub repaired 0B in 2 days 00:06:12 with 0 errors on Thu Sep 17 22:06:13 2026"
	// "resilvered 1.21T in 05:12:00 with 0 errors on Thu Sep 17 22:06:13 2026"
	zfsScanDoneRe = regexp.MustCompile(`^(scrub repaired|resilvered) (\S+) in (.+) with (\d+) errors on (.+)$`)
	// "scrub canceled on Thu Sep 17 22:06:13 2026"
	zfsScanCanceledRe = regexp.MustCompile(`^(scrub|resilver) canceled on (.+)$`)
	// "scrub in progress since ...", "scrub paused since ...", "resilver in progress since ..."
	zfsScanActiveRe = regexp.MustCompile(`^(scrub|resilver) (in progress|paused) since (.+)$`)
	// Detail line: "0B repaired, 13.15% done, 01:23:45 to go" or "1.21G resilvered, 0.12% done, ..."
	zfsScanProgressRe = regexp.MustCompile(`^(\S+) (?:repaired|resilvered), ([\d.]+)% done`)
	// Scan duration: "00:00:39", "2 days 00:06:12" (ZFS < 2.0 also printed "0 days ...")
	zfsScanDurationRe = regexp.MustCompile(`^(?:(\d+) days )?(\d+):(\d{2}):(\d{2})$`)
)

// parseScanInfo parses the "scan:" line of `zpool status` output.
// Unrecognised lines ("none requested", error scrubs) leave the scan fields unset.
func (c *ZFSCollector) parseScanInfo(pool *dto.ZFSPool, line string) {
	line = strings.TrimPrefix(line, "scan:")
	line = strings.TrimSpace(line)

	if m := zfsScanDoneRe.FindStringSubmatch(line); m != nil {
		function := "scrub"
		if m[1] == "resilvered" {
			function = "resilver"
		}
		pool.ScanStatus = function + " completed"
		pool.ScanState = "finished"
		pool.ScanRepairedBytes = parseZFSNiceBytes(m[2])
		pool.ScanErrors, _ = strconv.Atoi(m[4])
		pool.ScanProgressPct = 100
		pool.ScanEndTime = parseZFSScanTime(m[5])
		// The completed line has no start time; zpool prints the duration as end - start.
		if duration, ok := parseZFSScanDuration(m[3]); ok && !pool.ScanEndTime.IsZero() {
			pool.ScanStartTime = pool.ScanEndTime.Add(-duration)
		}
		return
	}

	if m := zfsScanCanceledRe.FindStringSubmatch(line); m != nil {
		pool.ScanStatus = m[1] + " canceled"
		pool.ScanState = "canceled"
		pool.ScanEndTime = parseZFSScanTime(m[2])
		return
	}

	if m := zfsScanActiveRe.FindStringSubmatch(line); m != nil {
		pool.ScanStatus = m[1] + " " + m[2]
		if m[2] == "paused" {
			// "since" is the pause time; the start time follows on a "scrub started on" line.
			pool.ScanState = "paused"
			return
		}
		pool.ScanState = "scanning"
		pool.ScanStartTime = parseZFSScanTime(m[3])
	}
}

// parseScanDetail parses the indented lines that follow an in-progress "scan:" line:
//
//	1.23T / 4.56T scanned at 1.20G/s, 600G / 4.56T issued at 600M/s
//	0B repaired, 13.15% done, 01:23:45 to go
//	scrub started on Thu Sep 17 22:06:13 2026   (paused scrubs only)
func (c *ZFSCollector) parseScanDetail(pool *dto.ZFSPool, line string) {
	if m := zfsScanProgressRe.FindStringSubmatch(line); m != nil {
		pool.ScanRepairedBytes = parseZFSNiceBytes(m[1])
		pool.ScanProgressPct, _ = strconv.ParseFloat(m[2], 64)
		return
	}
	if started, found := strings.CutPrefix(line, "scrub started on "); found {
		pool.ScanStartTime = parseZFSScanTime(started)
	}
}

// parseZFSScanTime parses a ctime(3) timestamp such as "Sun Oct  4 05:00:03 2026"
// in the local time zone (zpool formats it with the same zone). Returns the zero
// time if the value cannot be parsed.
func parseZFSScanTime(value string) time.Time {
	t, err := time.ParseInLocation(time.ANSIC, strings.TrimSpace(value), time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseZFSScanDuration parses a zpool scan duration ("00:00:39", "2 days 00:06:12").
func parseZFSScanDuration(value string) (time.Duration, bool) {
	m := zfsScanDurationRe.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return 0, false
	}
	days, _ := strconv.Atoi(m[1]) // empty when the scan took less than a day
	hours, _ := strconv.Atoi(m[2])
	minutes, _ := strconv.Atoi(m[3])
	seconds, _ := strconv.Atoi(m[4])
	return time.Duration(days)*24*time.Hour + time.Duration(hours)*time.Hour +
		time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second, true
}

// parseZFSNiceBytes converts zfs_nicebytes() output ("0B", "512B", "1.50M", "12.3G")
// to bytes using 1024-based units. Returns 0 if the value cannot be parsed.
func parseZFSNiceBytes(value string) uint64 {
	const units = "BKMGTPE"
	exponent := 0
	if i := strings.IndexByte(units, value[len(value)-1]); i >= 0 {
		exponent = i
		value = value[:len(value)-1]
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return uint64(number * math.Pow(1024, float64(exponent)))
}

// parseVdevLine parses a single vdev line from zpool status output
// Format: "NAME        STATE     READ WRITE CKSUM"
// Example: "  sdg1      ONLINE       0     0     0"
func (c *ZFSCollector) parseVdevLine(line string) *dto.ZFSVdev {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return nil
	}

	vdev := &dto.ZFSVdev{
		Name:  fields[0],
		State: fields[1],
	}

	// Determine vdev type based on name
	if strings.Contains(vdev.Name, "raidz1") {
		vdev.Type = "raidz1"
	} else if strings.Contains(vdev.Name, "raidz2") {
		vdev.Type = "raidz2"
	} else if strings.Contains(vdev.Name, "raidz3") {
		vdev.Type = "raidz3"
	} else if strings.Contains(vdev.Name, "mirror") {
		vdev.Type = "mirror"
	} else if strings.Contains(vdev.Name, "spare") {
		vdev.Type = "spare"
	} else if strings.Contains(vdev.Name, "cache") {
		vdev.Type = "cache"
	} else if strings.Contains(vdev.Name, "log") {
		vdev.Type = "log"
	} else {
		vdev.Type = "disk"
	}

	vdev.ReadErrors, _ = strconv.ParseUint(fields[2], 10, 64)
	vdev.WriteErrors, _ = strconv.ParseUint(fields[3], 10, 64)
	vdev.ChecksumErrors, _ = strconv.ParseUint(fields[4], 10, 64)

	return vdev
}

// collectDatasets collects information about all ZFS datasets
func (c *ZFSCollector) collectDatasets() ([]dto.ZFSDataset, error) {
	// Get all datasets across all pools
	// Fields: name, type, used, available, referenced, compressratio, mountpoint, quota, reservation, compression, readonly
	output, err := lib.ExecCommandOutput(constants.ZfsBin, "list", "-Hp", "-o",
		"name,type,used,available,referenced,compressratio,mountpoint,quota,reservation,compression,readonly")
	if err != nil {
		return nil, fmt.Errorf("failed to list datasets: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	datasets := make([]dto.ZFSDataset, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		dataset := c.parseDatasetLine(line)
		if dataset != nil {
			datasets = append(datasets, *dataset)
		}
	}

	return datasets, nil
}

// parseDatasetLine parses a single dataset line from zfs list output
func (c *ZFSCollector) parseDatasetLine(line string) *dto.ZFSDataset {
	fields := strings.Split(line, "\t")
	if len(fields) < 11 {
		return nil
	}

	dataset := &dto.ZFSDataset{
		Name:      fields[0],
		Type:      fields[1],
		Timestamp: time.Now(),
	}

	dataset.UsedBytes, _ = strconv.ParseUint(fields[2], 10, 64)
	dataset.AvailableBytes, _ = strconv.ParseUint(fields[3], 10, 64)
	dataset.ReferencedBytes, _ = strconv.ParseUint(fields[4], 10, 64)

	// Parse compression ratio (format: "1.00x" or "1.00")
	compressStr := strings.TrimSuffix(fields[5], "x")
	dataset.CompressRatio, _ = strconv.ParseFloat(compressStr, 64)

	if fields[6] != "-" {
		dataset.Mountpoint = fields[6]
	}

	dataset.QuotaBytes, _ = strconv.ParseUint(fields[7], 10, 64)
	dataset.ReservationBytes, _ = strconv.ParseUint(fields[8], 10, 64)
	dataset.Compression = fields[9]
	dataset.Readonly = fields[10] == "on"

	return dataset
}

// collectSnapshots collects information about all ZFS snapshots
func (c *ZFSCollector) collectSnapshots() ([]dto.ZFSSnapshot, error) {
	// Get all snapshots
	// Fields: name, used, referenced, creation
	output, err := lib.ExecCommandOutput(constants.ZfsBin, "list", "-t", "snapshot", "-Hp", "-o",
		"name,used,referenced,creation")
	if err != nil {
		// No snapshots is not an error
		if strings.Contains(err.Error(), "no datasets available") {
			return []dto.ZFSSnapshot{}, nil
		}
		return nil, fmt.Errorf("failed to list snapshots: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	snapshots := make([]dto.ZFSSnapshot, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		snapshot := c.parseSnapshotLine(line)
		if snapshot != nil {
			snapshots = append(snapshots, *snapshot)
		}
	}

	return snapshots, nil
}

// parseSnapshotLine parses a single snapshot line from zfs list output
func (c *ZFSCollector) parseSnapshotLine(line string) *dto.ZFSSnapshot {
	fields := strings.Split(line, "\t")
	if len(fields) < 4 {
		return nil
	}

	// Parse snapshot name (format: dataset@snapshot)
	parts := strings.Split(fields[0], "@")
	if len(parts) != 2 {
		return nil
	}

	snapshot := &dto.ZFSSnapshot{
		Name:      fields[0],
		Dataset:   parts[0],
		Timestamp: time.Now(),
	}

	snapshot.UsedBytes, _ = strconv.ParseUint(fields[1], 10, 64)
	snapshot.ReferencedBytes, _ = strconv.ParseUint(fields[2], 10, 64)

	// Parse creation time (Unix timestamp)
	creationUnix, _ := strconv.ParseInt(fields[3], 10, 64)
	snapshot.CreationTime = time.Unix(creationUnix, 0)

	return snapshot
}

// collectARCStats collects ZFS ARC (Adaptive Replacement Cache) statistics
func (c *ZFSCollector) collectARCStats() (dto.ZFSARCStats, error) {
	stats := dto.ZFSARCStats{
		Timestamp: time.Now(),
	}

	// Check if ARC stats file exists
	if _, err := os.Stat(constants.ProcSPLARCStats); os.IsNotExist(err) {
		return stats, fmt.Errorf("ARC stats file not found: %w", err)
	}

	// Read ARC stats file
	file, err := os.Open(constants.ProcSPLARCStats)
	if err != nil {
		return stats, fmt.Errorf("failed to open ARC stats file: %w", err)
	}
	defer file.Close() //nolint:errcheck // Error checking not needed for defer Close

	// Parse ARC stats (format: "name type data")
	scanner := bufio.NewScanner(file)
	arcData := make(map[string]uint64)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}

		name := fields[0]
		// type is fields[1], but we don't need it
		value, _ := strconv.ParseUint(fields[2], 10, 64)
		arcData[name] = value
	}

	if err := scanner.Err(); err != nil {
		return stats, fmt.Errorf("error reading ARC stats: %w", err)
	}

	// Extract relevant stats
	stats.SizeBytes = arcData["size"]
	stats.TargetSizeBytes = arcData["c"]
	stats.MinSizeBytes = arcData["c_min"]
	stats.MaxSizeBytes = arcData["c_max"]
	stats.Hits = arcData["hits"]
	stats.Misses = arcData["misses"]

	// User-configured zfs_arc_max module parameter (0 = auto). Unraid 7.3
	// promotes this to a first-class Disk Settings tunable.
	stats.ConfiguredMaxBytes = readZFSArcMaxParam()

	// Convenience fields
	stats.SizeMB = float64(stats.SizeBytes) / (1024 * 1024)
	if stats.MaxSizeBytes > 0 {
		stats.UsagePercent = float64(stats.SizeBytes) / float64(stats.MaxSizeBytes) * 100
	}

	// Calculate hit ratio
	totalAccesses := stats.Hits + stats.Misses
	if totalAccesses > 0 {
		stats.HitRatioPct = (float64(stats.Hits) / float64(totalAccesses)) * 100.0
	}

	// MRU/MFU hit ratios (if available)
	mruHits := arcData["mru_hits"]
	mfuHits := arcData["mfu_hits"]
	if mruHits > 0 || mfuHits > 0 {
		mruTotal := mruHits + arcData["mru_ghost_hits"]
		mfuTotal := mfuHits + arcData["mfu_ghost_hits"]

		if mruTotal > 0 {
			stats.MRUHitRatioPct = (float64(mruHits) / float64(mruTotal)) * 100.0
		}
		if mfuTotal > 0 {
			stats.MFUHitRatioPct = (float64(mfuHits) / float64(mfuTotal)) * 100.0
		}
	}

	// L2ARC stats (if available)
	stats.L2SizeBytes = arcData["l2_size"]
	stats.L2Hits = arcData["l2_hits"]
	stats.L2Misses = arcData["l2_misses"]

	return stats, nil
}

// readZFSArcMaxParam reads the user-configured zfs_arc_max module parameter.
// Returns 0 when unset (auto) or unavailable.
func readZFSArcMaxParam() uint64 {
	data, err := os.ReadFile(constants.SysZFSArcMax)
	if err != nil {
		logger.Debug("ZFS: zfs_arc_max parameter not readable: %v", err)
		return 0
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		logger.Debug("ZFS: failed to parse zfs_arc_max value %q: %v", strings.TrimSpace(string(data)), err)
		return 0
	}
	return value
}
