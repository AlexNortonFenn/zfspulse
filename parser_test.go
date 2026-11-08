package zfspulse

import (
	"strings"
	"testing"
	"time"

	"github.com/AlexNortonFenn/zfspulse/internal/parser"
)

var parseVdevErrorCount = parser.ParseVdevErrorCount
var parseScrubDate = parser.ParseScrubDate
var addSaturatingUint64 = parser.AddSaturatingUint64
var isVirtualContainer = parser.IsVirtualContainer
var mapVdevState = parser.MapVdevState

func TestZpoolStatusReaderParsing(t *testing.T) {
	col := New()
	mockOutput := `
  pool: tank
 state: ONLINE
  scan: scrub repaired 0B in 00:00:02 with 0 errors on Wed Nov  4 08:43:42 2026
config:

	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  mirror-0  ONLINE       0     0     0
	    sdb     ONLINE       0     0     0
	    sdc     ONLINE       1     2     5

errors: No known data errors
`
	health := PoolHealth{
		Name:             "tank",
		State:            "UNKNOWN",
		ReliabilityScore: 0.0,
	}

	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse zpool status: %v", err)
	}

	if health.State != "ONLINE" {
		t.Errorf("Expected State=ONLINE, got %s", health.State)
	}
	if health.ReadErrors != 1 {
		t.Errorf("Expected ReadErrors=1, got %d", health.ReadErrors)
	}
	if health.WriteErrors != 2 {
		t.Errorf("Expected WriteErrors=2, got %d", health.WriteErrors)
	}
	if health.ChecksumErrors != 5 {
		t.Errorf("Expected ChecksumErrors=5, got %d", health.ChecksumErrors)
	}

	if _, ok := col.lastScrub["tank"]; !ok {
		t.Error("Expected lastScrub to be populated from scan line")
	}

	// Degraded leaf disk test
	mockDegraded := `
  pool: tank
 state: ONLINE
config:

	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  mirror-0  DEGRADED     0     0     0
	    sdb     ONLINE       0     0     0
	    sdc     FAULTED      3     0     0

errors: No known data errors
`
	healthDegraded := PoolHealth{
		Name:             "tank",
		State:            "UNKNOWN",
		ReliabilityScore: 0.0,
	}
	if err := col.parseZpoolStatusReader(strings.NewReader(mockDegraded), "tank", &healthDegraded); err != nil {
		t.Fatalf("Failed to parse degraded zpool status: %v", err)
	}

	if healthDegraded.State != "DEGRADED" {
		t.Errorf("Expected State=DEGRADED due to faulted leaf device, got %s", healthDegraded.State)
	}
	if healthDegraded.ReadErrors != 3 {
		t.Errorf("Expected ReadErrors=3, got %d", healthDegraded.ReadErrors)
	}
}

func TestScrubInProgressDoesNotResetDrift(t *testing.T) {
	col := New()
	oldScrub := time.Now().Add(-168 * time.Hour)
	col.lastScrub["tank"] = oldScrub

	mockOutput := `
  pool: tank
 state: ONLINE
  scan: scrub in progress since Wed Nov  4 08:00:00 2026
	100M scanned at 10M/s, 50M issued at 5M/s, 200M total
	0B repaired, 25.00% done, 00:00:10 to go
config:
	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
`
	var health PoolHealth
	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse status: %v", err)
	}

	if col.lastScrub["tank"] != oldScrub {
		t.Errorf("lastScrub must NOT be updated during active scrub, expected %v, got %v", oldScrub, col.lastScrub["tank"])
	}
}

func TestParserLogsAndCacheSubsections(t *testing.T) {
	col := New()
	mockOutput := `
  pool: tank
 state: ONLINE
config:
	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  mirror-0  ONLINE       0     0     0
	    sda     ONLINE       0     0     0
	    sdb     ONLINE       0     0     0
	logs
	  nvme0n1   DEGRADED     1     2     3
	cache
	  nvme1n1   FAULTED      4     5     6
errors: No known data errors
`
	var health PoolHealth
	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse zpool status: %v", err)
	}

	if health.State != "DEGRADED" {
		t.Errorf("Expected pool state DEGRADED from degraded logs/cache, got %s", health.State)
	}
	if health.ReadErrors != 5 {
		t.Errorf("Expected 5 read errors (1+4), got %d", health.ReadErrors)
	}
	if health.WriteErrors != 7 {
		t.Errorf("Expected 7 write errors (2+5), got %d", health.WriteErrors)
	}
	if health.ChecksumErrors != 9 {
		t.Errorf("Expected 9 checksum errors (3+6), got %d", health.ChecksumErrors)
	}
}

func TestParserSparesExclusion(t *testing.T) {
	col := New()
	mockOutput := `
  pool: tank
 state: ONLINE
config:
	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  mirror-0  ONLINE       0     0     0
	    sda     ONLINE       0     0     0
	    sdb     ONLINE       0     0     0
	spares
	  sdc       FAULTED      9     9     9
errors: No known data errors
`
	var health PoolHealth
	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse status: %v", err)
	}

	// Spares do not participate in active I/O -> pool must NOT be degraded by spare disk faults
	if health.State != "ONLINE" {
		t.Errorf("Pool with faulted inactive spare should remain ONLINE, got %s", health.State)
	}
	if health.ReadErrors != 0 || health.WriteErrors != 0 || health.ChecksumErrors != 0 {
		t.Errorf("Errors from spare disks must not be tallied to active pool, got read=%d write=%d cksum=%d",
			health.ReadErrors, health.WriteErrors, health.ChecksumErrors)
	}
}

func TestErrorsPermanentFileDamageFaulted(t *testing.T) {
	col := New()
	mockOutput := `
  pool: tank
 state: ONLINE
config:
	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  mirror-0  ONLINE       0     0     0
	    sda     ONLINE       0     0     0
errors: Permanent errors have been detected in the following files:
	/tank/data/corrupted.db
`
	var health PoolHealth
	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse zpool status: %v", err)
	}

	if health.State != "FAULTED" {
		t.Errorf("Pool with permanent data errors must be raised to FAULTED, got %s", health.State)
	}
}

func TestParseVdevErrorCount(t *testing.T) {
	cases := []struct {
		in      string
		want    uint64
		wantErr bool
	}{
		{"0", 0, false},
		{"5", 5, false},
		{"1K", 1024, false},
		{"1k", 1024, false},
		{"1.5K", 1536, false},
		{"50M", 50 * 1024 * 1024, false},
		{"10m", 10 * 1024 * 1024, false},
		{"1G", 1024 * 1024 * 1024, false},
		{"1g", 1024 * 1024 * 1024, false},
		{"1T", 1024 * 1024 * 1024 * 1024, false},
		{"1t", 1024 * 1024 * 1024 * 1024, false},
		{"1P", 1024 * 1024 * 1024 * 1024 * 1024, false},
		{"1p", 1024 * 1024 * 1024 * 1024 * 1024, false},
		{"1E", 1024 * 1024 * 1024 * 1024 * 1024 * 1024, false},
		{"1e", 1024 * 1024 * 1024 * 1024 * 1024 * 1024, false},
		{"+Inf", 0, true},
		{"NaN", 0, true},
		{"1e300", 0, true},     // overflow
		{"16777216T", 0, true}, // Fix #2: exactly 2^64, must overflow and return error
		{"16384P", 0, true},    // exactly 2^64 (2^14 * 2^50), must overflow
		{"16E", 0, true},       // exactly 2^64 (2^4 * 2^60), must overflow
		{"18446744073709551616", 0, true}, // 2^64 integer string, must overflow
		{"-5", 0, true},
		{"junk", 0, true},
		{"", 0, true},
	}

	for _, tc := range cases {
		got, err := parseVdevErrorCount(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseVdevErrorCount(%q): expected error, got nil (val=%d)", tc.in, got)
			}
		} else {
			if err != nil {
				t.Errorf("parseVdevErrorCount(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseVdevErrorCount(%q): expected %d, got %d", tc.in, tc.want, got)
			}
		}
	}
}

func TestParseScrubDateVariants(t *testing.T) {
	cases := []struct {
		line    string
		wantErr bool
	}{
		{"scan: scrub repaired 0B in 00:00:02 with 0 errors on Wed Nov  4 08:43:42 2026", false},
		{"scan: resilvered 10M in 00:01:00 with 0 errors on Sun Jan 18 14:23:01 2026", false},
		{"scan: scrub repaired 0B in 01:23:45 with 0 errors on 2026-11-04 12:00:00", false},
		{"scan: scrub in progress", true}, // no ' on ' delimiter
		{"scan: scrub repaired 0B on invalid-timestamp-here", true},
	}

	for _, tc := range cases {
		ts, err := parseScrubDate(tc.line)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseScrubDate(%q): expected error, got %v", tc.line, ts)
			}
		} else {
			if err != nil {
				t.Errorf("parseScrubDate(%q): unexpected error: %v", tc.line, err)
			}
			if ts.IsZero() {
				t.Errorf("parseScrubDate(%q): parsed time is zero", tc.line)
			}
		}
	}
}

func TestAddSaturatingUint64(t *testing.T) {
	if got := addSaturatingUint64(10, 20); got != 30 {
		t.Errorf("Expected 30, got %d", got)
	}
	// MaxUint64 overflow check
	if got := addSaturatingUint64(^uint64(0)-5, 10); got != ^uint64(0) {
		t.Errorf("Expected MaxUint64 on saturation, got %d", got)
	}
}

func TestVirtualContainerAndVdevMapping(t *testing.T) {
	// Virtual containers
	containers := []string{"tank", "mirror-0", "mirror:0", "mirror", "raidz1-0", "draid2-0", "special-0", "cache-0", "replacing-0", "replacing:0", "spare-0"}
	for _, c := range containers {
		if !isVirtualContainer(c, "tank") {
			t.Errorf("Expected %q to be recognized as virtual container", c)
		}
	}
	leaves := []string{"sda", "sdb1", "/dev/disk/by-id/nvme-0"}
	for _, l := range leaves {
		if isVirtualContainer(l, "tank") {
			t.Errorf("Expected %q to NOT be recognized as virtual container", l)
		}
	}

	// State mapping
	if mapVdevState("ONLINE") != "ONLINE" {
		t.Errorf("Expected ONLINE")
	}
	if mapVdevState("FAULTED") != "DEGRADED" {
		t.Errorf("Expected FAULTED to map to DEGRADED at pool level")
	}
	if mapVdevState("REMOVED") != "DEGRADED" {
		t.Errorf("Expected REMOVED to map to DEGRADED")
	}
	if mapVdevState("UNKNOWN_STATE") != "" {
		t.Errorf("Expected empty string for unknown vdev state")
	}
}

func TestTruncatedVdevFieldsFailClosed(t *testing.T) {
	// Fix #6: Lines with len(fields) < 5 (e.g. "sda UNAVAIL") must degrade the pool and record note
	col := New()
	mockOutput := `
  pool: tank
 state: ONLINE
config:
	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  sda       UNAVAIL
	  sdb       ONLINE
	    was /dev/sdb1
errors: No known data errors
`
	var health PoolHealth
	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse status: %v", err)
	}

	if health.State != "DEGRADED" {
		t.Errorf("Expected pool state DEGRADED from sda UNAVAIL (len=2), got %s", health.State)
	}
	if !strings.Contains(health.StatusNote, "vdev sda is UNAVAIL") {
		t.Errorf("Expected StatusNote to record sda UNAVAIL, got: %s", health.StatusNote)
	}
}

func TestStatusAndActionCollectedInStatusNote(t *testing.T) {
	// Fix #7: Multi-line status: and action: lines are captured into StatusNote
	col := New()
	mockOutput := `
  pool: tank
 state: DEGRADED
status: One or more devices could not be opened.  Sufficient replicas exist for
	the pool to continue functioning in a degraded state.
action: Attach the missing device and online it using 'zpool online'.
   see: https://openzfs.github.io/openzfs-docs/msg/ZFS-8000-2Q
  scan: scrub repaired 0B in 00:00:02 with 0 errors on Wed Nov  4 08:43:42 2026
config:
	NAME        STATE     READ WRITE CKSUM
	tank        DEGRADED     0     0     0
	  sda       UNAVAIL      0     0     0
errors: No known data errors
`
	var health PoolHealth
	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse status: %v", err)
	}

	if health.State != "DEGRADED" {
		t.Errorf("Expected DEGRADED, got %s", health.State)
	}
	if !strings.Contains(health.StatusNote, "status: One or more devices could not be opened. Sufficient replicas exist for the pool to continue functioning in a degraded state.") {
		t.Errorf("Expected multi-line status to be concatenated into StatusNote, got: %q", health.StatusNote)
	}
	if !strings.Contains(health.StatusNote, "action: Attach the missing device and online it using 'zpool online'.") {
		t.Errorf("Expected action to be included in StatusNote, got: %q", health.StatusNote)
	}
}

func TestErrorsHandlerPreservesExistingStatusNote(t *testing.T) {
	// Fix #4: Do not overwrite existing StatusNote, and do not noise-log "No known data errors"
	col := New()

	// Case 1: Healthy pool with existing StatusNote (e.g. from truncation)
	mockClean := `
  pool: tank
 state: ONLINE
config:
	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
errors: No known data errors
`
	healthClean := PoolHealth{
		State:      "ONLINE",
		StatusNote: "zpool output truncated at 1MB (buffer cap reached)",
	}
	if err := col.parseZpoolStatusReader(strings.NewReader(mockClean), "tank", &healthClean); err != nil {
		t.Fatalf("Failed to parse clean: %v", err)
	}
	if healthClean.StatusNote != "zpool output truncated at 1MB (buffer cap reached)" {
		t.Errorf("Expected existing StatusNote preserved, got: %q", healthClean.StatusNote)
	}

	// Case 2: Pool with real data errors preserves note and raises FAULTED
	mockErrors := `
  pool: tank
 state: ONLINE
config:
	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
errors: Permanent errors have been detected in the following files:
`
	healthErrors := PoolHealth{
		State:      "ONLINE",
		StatusNote: "initial diagnostic note",
	}
	if err := col.parseZpoolStatusReader(strings.NewReader(mockErrors), "tank", &healthErrors); err != nil {
		t.Fatalf("Failed to parse errors: %v", err)
	}
	if healthErrors.State != "FAULTED" {
		t.Errorf("Expected FAULTED on permanent data errors, got %s", healthErrors.State)
	}
	if !strings.Contains(healthErrors.StatusNote, "initial diagnostic note") || !strings.Contains(healthErrors.StatusNote, "errors: Permanent errors") {
		t.Errorf("Expected concatenated StatusNote preserving earlier note, got: %q", healthErrors.StatusNote)
	}
}

func TestTruncatedHeaderTwoFields(t *testing.T) {
	col := New()
	// Output where config header only has 2 fields: NAME STATE (e.g. older ZFS or custom view)
	mockOutput := `
  pool: tank
 state: ONLINE
config:
	NAME        STATE
	tank        ONLINE
	  sda       UNAVAIL
	  sdb       ONLINE
errors: No known data errors
`
	var health PoolHealth
	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}
	if health.State != "DEGRADED" {
		t.Errorf("Expected DEGRADED from 2-field header with unavail disk, got %s", health.State)
	}
	if !strings.Contains(health.StatusNote, "vdev sda is UNAVAIL") {
		t.Errorf("Expected note about sda UNAVAIL, got: %q", health.StatusNote)
	}
}

func TestPoolSelfRowStateElevatesCorrectly(t *testing.T) {
	col := New()
	// Device tree where state: header is missing, but pool root row shows FAULTED
	mockOutput := `
config:
	NAME        STATE     READ WRITE CKSUM
	tank        FAULTED      0     0     0
	  sda       FAULTED      0     0     0
`
	health := PoolHealth{State: "UNKNOWN"}
	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}
	if health.State != "FAULTED" {
		t.Errorf("Expected pool state FAULTED from root row, got %s", health.State)
	}
}

func TestUnexpectedStateFromZpoolStatusFailClosed(t *testing.T) {
	col := New()
	mockOutput := `
  pool: tank
 state: CORRUPTED_STATE
config:
	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
`
	health := PoolHealth{State: "ONLINE"}
	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}
	if health.State != "FAULTED" {
		t.Errorf("Expected fail-closed FAULTED on unexpected state, got %s", health.State)
	}
	if !strings.Contains(health.StatusNote, "unexpected pool state: CORRUPTED_STATE") {
		t.Errorf("Expected StatusNote to explain unexpected state, got: %q", health.StatusNote)
	}
}

func TestSuspendedPoolStateParsing(t *testing.T) {
	col := New()
	mockOutput := `
  pool: tank
 state: SUSPENDED
status: One or more devices has experienced an unrecoverable error.
        An attempt was made to correct the error. Applications are blocked.
action: Wait for the hardware to recover and run 'zpool clear'.
config:
	NAME        STATE     READ WRITE CKSUM
	tank        SUSPENDED    0     0     0
	  sda       ONLINE       0     0     0
	  sdb       FAULTED     10    20     0
`
	health := PoolHealth{State: "ONLINE"}
	if err := col.parseZpoolStatusReader(strings.NewReader(mockOutput), "tank", &health); err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}
	if health.State != "SUSPENDED" {
		t.Errorf("Expected pool state SUSPENDED, got %s", health.State)
	}
}
