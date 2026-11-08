package parser

import (
	"strings"
	"testing"
)

func TestParseVdevErrorCount(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"0", 0},
		{"42", 42},
		{"1K", 1024},
		{"2k", 2048},
		{"1M", 1024 * 1024},
		{"1G", 1024 * 1024 * 1024},
	}

	for _, tc := range cases {
		got, err := ParseVdevErrorCount(tc.in)
		if err != nil {
			t.Errorf("Unexpected error for %s: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseVdevErrorCount(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseScrubDateVariants(t *testing.T) {
	lines := []string{
		"scan: scrub repaired 0B in 00:00:05 with 0 errors on Wed Nov  4 08:43:42 2026",
		"scan: scrub repaired 0B in 00:00:05 with 0 errors on 2026-11-04 08:43:42",
		"scan: scrub repaired 0B in 00:00:05 with 0 errors on Wed Nov 04 08:43:42 2026",
	}

	for _, line := range lines {
		dt, err := ParseScrubDate(line)
		if err != nil {
			t.Errorf("Failed to parse date from line %q: %v", line, err)
		}
		if dt.Year() != 2026 {
			t.Errorf("Expected year 2026, got %d", dt.Year())
		}
	}
}

func TestAddSaturatingUint64(t *testing.T) {
	if got := AddSaturatingUint64(10, 20); got != 30 {
		t.Errorf("Expected 30, got %d", got)
	}
	if got := AddSaturatingUint64(^uint64(0), 1); got != ^uint64(0) {
		t.Errorf("Expected saturating max uint64, got %d", got)
	}
}

func TestParseZpoolStatusReader(t *testing.T) {
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
	res, err := ParseZpoolStatusReader(strings.NewReader(mockOutput), "tank")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if res.PoolState != "ONLINE" {
		t.Errorf("Expected ONLINE, got %s", res.PoolState)
	}
	if res.ReadErrors != 1 || res.WriteErrors != 2 || res.ChecksumErrors != 5 {
		t.Errorf("Expected errors 1, 2, 5, got %d, %d, %d", res.ReadErrors, res.WriteErrors, res.ChecksumErrors)
	}
	if !res.HasLastScrub {
		t.Error("Expected HasLastScrub to be true")
	}
}

func TestScrubInProgressIndicator(t *testing.T) {
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
	res, err := ParseZpoolStatusReader(strings.NewReader(mockOutput), "tank")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if !res.ScrubInProgress {
		t.Error("Expected ScrubInProgress=true")
	}
	if !strings.Contains(res.StatusNote, "actively in progress") {
		t.Errorf("Expected status note to mention active scrub, got: %s", res.StatusNote)
	}
}

func TestStackParserWithCustomDiskNames(t *testing.T) {
	// Verifies that disks with names matching former prefix heuristics (e.g. mirror_disk_1)
	// are accurately identified as leaf nodes via indentation stack geometry and their errors are counted.
	mockOutput := `
  pool: tank
 state: ONLINE
config:

	NAME                 STATE     READ WRITE CKSUM
	tank                 ONLINE       0     0     0
	  mirror-0           ONLINE       0     0     0
	    mirror_disk_1    ONLINE       3     0     0
	    spare_disk_1     ONLINE       0     4     0
`
	res, err := ParseZpoolStatusReader(strings.NewReader(mockOutput), "tank")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if res.PoolState != "ONLINE" {
		t.Errorf("Expected ONLINE, got %s", res.PoolState)
	}
	if res.ReadErrors != 3 {
		t.Errorf("Expected 3 ReadErrors on mirror_disk_1, got %d", res.ReadErrors)
	}
	if res.WriteErrors != 4 {
		t.Errorf("Expected 4 WriteErrors on spare_disk_1, got %d", res.WriteErrors)
	}
}

func TestStackParserNestedReplacing(t *testing.T) {
	mockOutput := `
  pool: tank
 state: DEGRADED
config:

	NAME                  STATE     READ WRITE CKSUM
	tank                  DEGRADED     0     0     0
	  mirror-0            DEGRADED     0     0     0
	    replacing-0       DEGRADED     0     0     0
	      sda             FAULTED      7     0     0
	      spare-0         ONLINE       0     0     0
	    sdb               ONLINE       0     0     0
`
	res, err := ParseZpoolStatusReader(strings.NewReader(mockOutput), "tank")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if res.PoolState != "DEGRADED" {
		t.Errorf("Expected DEGRADED, got %s", res.PoolState)
	}
	// Only leaf disk errors must be counted, replacing-0 and mirror-0 must not duplicate them
	if res.ReadErrors != 7 {
		t.Errorf("Expected 7 ReadErrors from leaf sda, got %d", res.ReadErrors)
	}
}

func TestStackParserSparesAndCacheSections(t *testing.T) {
	mockOutput := `
  pool: tank
 state: ONLINE
config:

	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
	  sda       ONLINE       0     0     0
	cache
	  sdb       ONLINE       1     0     0
	spares
	  sdc       AVAIL
`
	res, err := ParseZpoolStatusReader(strings.NewReader(mockOutput), "tank")
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if res.PoolState != "ONLINE" {
		t.Errorf("Expected ONLINE, got %s", res.PoolState)
	}
	if res.ReadErrors != 1 {
		t.Errorf("Expected 1 ReadError from cache leaf sdb, got %d", res.ReadErrors)
	}
}
