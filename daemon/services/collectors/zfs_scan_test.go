package collectors

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ruaan-deysel/unraid-management-agent/daemon/domain"
	"github.com/ruaan-deysel/unraid-management-agent/daemon/dto"
)

// localTime builds the expected value for a ctime timestamp printed by zpool,
// which is always in the server's local time zone.
func localTime(year int, month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(year, month, day, hour, minute, second, 0, time.Local)
}

// TestParsePoolStatusScanFixtures runs parsePoolStatus over `zpool status -v`
// output. The "scrub-days" and "scrub-mirror" fixtures were captured from an
// Unraid 7.4.0-beta.3 server running ZFS 2.4.4; the remaining fixtures follow the
// print_scan_scrub_resilver_status() formats in OpenZFS zpool_main.c.
func TestParsePoolStatusScanFixtures(t *testing.T) {
	tests := []struct {
		fixture      string
		wantStatus   string
		wantState    string
		wantErrors   int
		wantRepaired uint64
		wantProgress float64
		wantStart    time.Time
		wantEnd      time.Time
		wantVdevs    int
	}{
		{
			fixture:      "status-v-scrub-days.txt",
			wantStatus:   "scrub completed",
			wantState:    "finished",
			wantProgress: 100,
			// "repaired 0B in 2 days 00:06:12 with 0 errors on Thu Sep 17 22:06:13 2026"
			wantStart: localTime(2026, time.September, 15, 22, 0, 1),
			wantEnd:   localTime(2026, time.September, 17, 22, 6, 13),
			wantVdevs: 2,
		},
		{
			fixture:      "status-v-scrub-mirror.txt",
			wantStatus:   "scrub completed",
			wantState:    "finished",
			wantProgress: 100,
			// "repaired 0B in 00:00:02 with 0 errors on Sun Oct  4 05:00:03 2026"
			wantStart: localTime(2026, time.October, 4, 5, 0, 1),
			wantEnd:   localTime(2026, time.October, 4, 5, 0, 3),
			wantVdevs: 1,
		},
		{
			fixture:      "status-v-scrub-errors.txt",
			wantStatus:   "scrub completed",
			wantState:    "finished",
			wantErrors:   2,
			wantRepaired: 128 * 1024,
			wantProgress: 100,
			wantStart:    localTime(2026, time.September, 30, 2, 0, 1),
			wantEnd:      localTime(2026, time.October, 1, 4, 3, 5),
			wantVdevs:    1,
		},
		{
			fixture:      "status-v-scrub-in-progress.txt",
			wantStatus:   "scrub in progress",
			wantState:    "scanning",
			wantRepaired: 1572864, // 1.50M
			wantProgress: 48.06,
			wantStart:    localTime(2026, time.October, 7, 9, 0, 1),
			wantVdevs:    1,
		},
		{
			fixture:      "status-v-scrub-paused.txt",
			wantStatus:   "scrub paused",
			wantState:    "paused",
			wantProgress: 48.06,
			wantStart:    localTime(2026, time.October, 7, 9, 0, 1),
			wantVdevs:    1,
		},
		{
			fixture:    "status-v-scrub-canceled.txt",
			wantStatus: "scrub canceled",
			wantState:  "canceled",
			wantEnd:    localTime(2026, time.October, 7, 10, 15, 0),
			wantVdevs:  1,
		},
		{
			fixture:      "status-v-resilver-in-progress.txt",
			wantStatus:   "resilver in progress",
			wantState:    "scanning",
			wantRepaired: 511 * 1024 * 1024 * 1024,
			wantProgress: 12.14,
			wantStart:    localTime(2026, time.October, 7, 9, 0, 1),
			wantVdevs:    1,
		},
		{
			fixture:      "status-v-resilver-completed.txt",
			wantStatus:   "resilver completed",
			wantState:    "finished",
			wantRepaired: 4529987906437, // 4.12T
			wantProgress: 100,
			wantStart:    localTime(2026, time.October, 7, 9, 0, 1),
			wantEnd:      localTime(2026, time.October, 7, 12, 12, 46),
			wantVdevs:    1,
		},
		{
			fixture:   "status-v-never-scrubbed.txt",
			wantVdevs: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "zpool", tt.fixture))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			c := NewZFSCollector(&domain.Context{Hub: domain.NewEventBus(10)})
			c.execOutput = func(command string, args ...string) (string, error) {
				return string(data), nil
			}

			pool := &dto.ZFSPool{Name: "test"}
			if err := c.parsePoolStatus(pool); err != nil {
				t.Fatalf("parsePoolStatus: %v", err)
			}

			if pool.ScanStatus != tt.wantStatus {
				t.Errorf("ScanStatus = %q, want %q", pool.ScanStatus, tt.wantStatus)
			}
			if pool.ScanState != tt.wantState {
				t.Errorf("ScanState = %q, want %q", pool.ScanState, tt.wantState)
			}
			if pool.ScanErrors != tt.wantErrors {
				t.Errorf("ScanErrors = %d, want %d", pool.ScanErrors, tt.wantErrors)
			}
			if pool.ScanRepairedBytes != tt.wantRepaired {
				t.Errorf("ScanRepairedBytes = %d, want %d", pool.ScanRepairedBytes, tt.wantRepaired)
			}
			if pool.ScanProgressPct != tt.wantProgress {
				t.Errorf("ScanProgressPct = %v, want %v", pool.ScanProgressPct, tt.wantProgress)
			}
			if !pool.ScanStartTime.Equal(tt.wantStart) {
				t.Errorf("ScanStartTime = %v, want %v", pool.ScanStartTime, tt.wantStart)
			}
			if !pool.ScanEndTime.Equal(tt.wantEnd) {
				t.Errorf("ScanEndTime = %v, want %v", pool.ScanEndTime, tt.wantEnd)
			}
			// The scan detail lines must not disturb vdev parsing.
			if len(pool.VDEVs) != tt.wantVdevs {
				t.Errorf("len(VDEVs) = %d, want %d", len(pool.VDEVs), tt.wantVdevs)
			}
		})
	}
}

func TestParsePoolStatusCommandError(t *testing.T) {
	c := NewZFSCollector(&domain.Context{Hub: domain.NewEventBus(10)})
	wantErr := errors.New("zpool failed")
	c.execOutput = func(command string, args ...string) (string, error) {
		return "", wantErr
	}
	if err := c.parsePoolStatus(&dto.ZFSPool{Name: "test"}); !errors.Is(err, wantErr) {
		t.Errorf("parsePoolStatus error = %v, want %v", err, wantErr)
	}
}

func TestParseScanInfoUnparseableTimes(t *testing.T) {
	c := NewZFSCollector(&domain.Context{Hub: domain.NewEventBus(10)})

	tests := []struct {
		name      string
		line      string
		wantStart time.Time
		wantEnd   time.Time
	}{
		{
			// Pre-0.8 duration format: end time is kept, start time is unknown.
			name:    "legacy duration",
			line:    "scan: scrub repaired 0B in 0h1m with 0 errors on Sun Oct  4 05:00:03 2026",
			wantEnd: localTime(2026, time.October, 4, 5, 0, 3),
		},
		{
			name: "bad end time",
			line: "scan: scrub repaired 0B in 00:00:02 with 0 errors on yesterday",
		},
		{
			name: "bad start time",
			line: "scan: scrub in progress since yesterday",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := &dto.ZFSPool{}
			c.parseScanInfo(pool, tt.line)
			if !pool.ScanStartTime.Equal(tt.wantStart) {
				t.Errorf("ScanStartTime = %v, want %v", pool.ScanStartTime, tt.wantStart)
			}
			if !pool.ScanEndTime.Equal(tt.wantEnd) {
				t.Errorf("ScanEndTime = %v, want %v", pool.ScanEndTime, tt.wantEnd)
			}
		})
	}
}

func TestParseZFSScanDuration(t *testing.T) {
	tests := []struct {
		value  string
		want   time.Duration
		wantOK bool
	}{
		{"00:00:39", 39 * time.Second, true},
		{"04:27:00", 4*time.Hour + 27*time.Minute, true},
		{"2 days 00:06:12", 48*time.Hour + 6*time.Minute + 12*time.Second, true},
		{"0 days 00:00:01", time.Second, true},
		{"0h1m", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, ok := parseZFSScanDuration(tt.value)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseZFSScanDuration(%q) = %v, %v; want %v, %v", tt.value, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseZFSNiceBytes(t *testing.T) {
	tests := []struct {
		value string
		want  uint64
	}{
		{"0B", 0},
		{"512B", 512},
		{"4K", 4096},
		{"1.50M", 1572864},
		{"12.3G", 13207024435},
		{"1E", 1 << 60},
		{"42", 42},
		{"abc", 0},
		{"B", 0},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := parseZFSNiceBytes(tt.value); got != tt.want {
				t.Errorf("parseZFSNiceBytes(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}
