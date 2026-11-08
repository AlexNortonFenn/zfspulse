package zfspulse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestInspectorOptions(t *testing.T) {
	ins := New(
		WithProcZfsPath("/custom/proc/zfs"),
		WithScrubDriftThresholdHours(336.0),
		WithCommandTimeout(30*time.Second),
	)

	opts := ins.Options()
	if opts.ProcZfsPath != "/custom/proc/zfs" {
		t.Errorf("Expected ProcZfsPath=/custom/proc/zfs, got %s", opts.ProcZfsPath)
	}
	if opts.ScrubDriftThresholdHours != 336.0 {
		t.Errorf("Expected ScrubDriftThresholdHours=336.0, got %f", opts.ScrubDriftThresholdHours)
	}
	if opts.CommandTimeout != 30*time.Second {
		t.Errorf("Expected CommandTimeout=30s, got %v", opts.CommandTimeout)
	}
}

func TestInspectPoolValidation(t *testing.T) {
	ins := New()

	ctx := context.Background()

	// Empty pool name
	_, err := ins.InspectPool(ctx, "")
	if err == nil {
		t.Error("Expected error for empty pool name, got nil")
	}

	// Path traversal attempts
	traversalCases := []string{
		"../tank",
		"tank/sub",
		"tank\\sub",
		".",
		"..",
		"/tank",
	}

	for _, tc := range traversalCases {
		h, err := ins.InspectPool(ctx, tc)
		if err == nil {
			t.Errorf("Expected path traversal error for %q, got nil", tc)
		}
		if h.State != "UNKNOWN" {
			t.Errorf("Fail-Closed: State must be UNKNOWN on error, got %s", h.State)
		}
	}
}

func TestInspectPoolMocked(t *testing.T) {
	recentScrub := time.Now().Add(-2 * time.Hour).Format("Mon Jan _2 15:04:05 2006")
	mockOutput := `
  pool: rpool
 state: ONLINE
  scan: scrub repaired 0B in 00:00:05 with 0 errors on ` + recentScrub + `
config:

	NAME        STATE     READ WRITE CKSUM
	rpool       ONLINE       0     0     0
	  mirror-0  ONLINE       0     0     0
	    sda3    ONLINE       0     0     0
	    sdb3    ONLINE       0     0     0

errors: No known data errors
`
	ins := New(WithProcZfsPath("")) // procfs disabled
	ins.cmdRunner = func(ctx context.Context, poolName string) zpoolCmdResult {
		return zpoolCmdResult{
			Stdout: mockOutput,
		}
	}

	ctx := context.Background()
	h, err := ins.InspectPool(ctx, "rpool")
	if err != nil {
		t.Fatalf("InspectPool returned unexpected error: %v", err)
	}

	if h.Name != "rpool" {
		t.Errorf("Expected name=rpool, got %s", h.Name)
	}
	if h.State != "ONLINE" {
		t.Errorf("Expected state=ONLINE, got %s", h.State)
	}
	if h.ReadErrors != 0 || h.WriteErrors != 0 || h.ChecksumErrors != 0 {
		t.Errorf("Expected 0 errors, got read=%d write=%d cksum=%d", h.ReadErrors, h.WriteErrors, h.ChecksumErrors)
	}
	if h.ReliabilityScore < 90.0 {
		t.Errorf("Expected high reliability score, got %.1f", h.ReliabilityScore)
	}
	if h.Severity() != SeverityOK {
		t.Errorf("Expected SeverityOK, got %s", h.Severity())
	}
}

func TestInspectAllWithProcfs(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mock pools: poolA and poolB
	for _, p := range []string{"poolA", "poolB"} {
		pDir := filepath.Join(tmpDir, p)
		if err := os.MkdirAll(pDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pDir, "state"), []byte("ONLINE\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	ins := New(WithProcZfsPath(tmpDir))
	ins.cmdRunner = func(ctx context.Context, poolName string) zpoolCmdResult {
		return zpoolCmdResult{
			Stdout: `
  pool: ` + poolName + `
 state: ONLINE
config:
	NAME        STATE     READ WRITE CKSUM
	` + poolName + `       ONLINE       0     0     0
errors: No known data errors
`,
		}
	}

	ctx := context.Background()
	results, err := ins.InspectAll(ctx)
	if err != nil {
		t.Fatalf("InspectAll failed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("Expected 2 pools, got %d", len(results))
	}

	if results[0].Name != "poolA" || results[1].Name != "poolB" {
		t.Errorf("Expected [poolA, poolB], got [%s, %s]", results[0].Name, results[1].Name)
	}
}

func TestSeverityClassification(t *testing.T) {
	cases := []struct {
		health PoolHealth
		want   Severity
	}{
		{PoolHealth{State: "ONLINE", ReliabilityScore: 100.0}, SeverityOK},
		{PoolHealth{State: "ONLINE", ReliabilityScore: 75.0}, SeverityOK},
		{PoolHealth{State: "ONLINE", ReliabilityScore: 74.9}, SeverityWarn},
		{PoolHealth{State: "ONLINE", ReliabilityScore: 50.0}, SeverityWarn},
		{PoolHealth{State: "ONLINE", ReliabilityScore: 49.9}, SeverityCritical},
		{PoolHealth{State: "ONLINE", ReliabilityScore: 0.0}, SeverityCritical},
		{PoolHealth{State: "DEGRADED", ReliabilityScore: 40.0}, SeverityCritical},
		{PoolHealth{State: "FAULTED", ReliabilityScore: 0.0}, SeverityCritical},
		{PoolHealth{State: "UNAVAIL", ReliabilityScore: 0.0}, SeverityCritical},
		{PoolHealth{State: "OFFLINE", ReliabilityScore: 0.0}, SeverityCritical},
		{PoolHealth{State: "REMOVED", ReliabilityScore: 0.0}, SeverityCritical},
		{PoolHealth{State: "UNKNOWN", ReliabilityScore: 0.0}, SeverityUnknown},
	}

	for _, tc := range cases {
		got := tc.health.Severity()
		if got != tc.want {
			t.Errorf("Severity() for %+v: got %s, want %s", tc.health, got, tc.want)
		}
	}
}

func TestDiscoverPoolsFallbackNoPoolsAvailable(t *testing.T) {
	ins := New(WithProcZfsPath(""))
	ins.cmdListRunner = func(ctx context.Context) (string, string, error) {
		return "", "no pools available\n", errors.New("exit status 1")
	}

	pools, err := ins.DiscoverPools(context.Background())
	if err != nil {
		t.Fatalf("Expected nil error for 'no pools available', got: %v", err)
	}
	if len(pools) != 0 {
		t.Fatalf("Expected 0 pools, got %v", pools)
	}
}

func TestDiscoverPoolsFallbackList(t *testing.T) {
	ins := New(WithProcZfsPath(""))
	ins.cmdListRunner = func(ctx context.Context) (string, string, error) {
		return "tank\nrpool\ntank\n", "", nil
	}

	pools, err := ins.DiscoverPools(context.Background())
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if len(pools) != 2 || pools[0] != "rpool" || pools[1] != "tank" {
		t.Fatalf("Expected [rpool, tank], got %v", pools)
	}
}

func TestDiscoverPoolsHungTimeout(t *testing.T) {
	ins := New(WithProcZfsPath(""))
	ins.cmdListRunner = func(ctx context.Context) (string, string, error) {
		ins.hungCommands.Add(1)
		return "", "", context.DeadlineExceeded
	}

	_, err := ins.DiscoverPools(context.Background())
	if err == nil {
		t.Fatal("Expected error on hung timeout")
	}
	if ins.HungCommands() != 1 {
		t.Fatalf("Expected HungCommands=1, got %d", ins.HungCommands())
	}
}

func TestDiscoverPoolsParentCancel(t *testing.T) {
	ins := New(WithProcZfsPath(""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ins.cmdListRunner = func(ctx context.Context) (string, string, error) {
		return "", "", ctx.Err()
	}

	_, err := ins.DiscoverPools(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Expected context.Canceled, got %v", err)
	}
	if ins.HungCommands() != 0 {
		t.Fatalf("Expected HungCommands=0 on parent cancel, got %d", ins.HungCommands())
	}
}

func TestConcurrentInspect(t *testing.T) {
	ins := New(WithProcZfsPath(""))
	ins.cmdRunner = func(ctx context.Context, poolName string) zpoolCmdResult {
		return zpoolCmdResult{
			Stdout: `
  pool: concurrent_pool
 state: ONLINE
  scan: scrub repaired 0B in 00:00:01 with 0 errors on Sun Jan 18 14:23:01 2026
config:
	NAME             STATE     READ WRITE CKSUM
	concurrent_pool  ONLINE       0     0     0
errors: No known data errors
`,
		}
	}

	var wg sync.WaitGroup
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				h, err := ins.InspectPool(ctx, "concurrent_pool")
				if err != nil {
					t.Errorf("Concurrent inspect error: %v", err)
				}
				if h.State != "ONLINE" {
					t.Errorf("Expected ONLINE, got %s", h.State)
				}
			}
		}()
	}

	wg.Wait()
}
