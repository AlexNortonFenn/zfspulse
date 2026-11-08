package external_metric

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexNortonFenn/zfspulse"
)

// setupFakeZpool creates a mock zpool binary in an isolated directory with recent scrub.
func setupFakeZpool(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()

	recentScrub := time.Now().Add(-2 * time.Hour).Format("Mon Jan _2 15:04:05 2006")

	shScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "list" ]; then
    echo "tank"
    exit 0
fi
cat << 'EOF'
  pool: tank
 state: ONLINE
  scan: scrub repaired 0B in 00:00:05 with 0 errors on %s
config:

	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0

errors: No known data errors
EOF
exit 0
`, recentScrub)

	batScript := fmt.Sprintf(`@echo off
if "%%1"=="list" (
    echo tank
    exit /b 0
)
echo   pool: tank
echo  state: ONLINE
echo   scan: scrub repaired 0B in 00:00:05 with 0 errors on %s
echo config:
echo 	NAME        STATE     READ WRITE CKSUM
echo 	tank        ONLINE       0     0     0
echo errors: No known data errors
exit /b 0
`, recentScrub)

	if err := os.WriteFile(filepath.Join(tmpDir, "zpool"), []byte(shScript), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "zpool.bat"), []byte(batScript), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func setupMockProcfs(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	poolDir := filepath.Join(tmpDir, "tank")
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poolDir, "state"), []byte("ONLINE\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return tmpDir
}

func TestMetricsHandler(t *testing.T) {
	setupFakeZpool(t)
	procDir := setupMockProcfs(t)
	ins := zfspulse.New(
		zfspulse.WithProcZfsPath(procDir),
		zfspulse.WithCommandTimeout(100*time.Millisecond),
	)
	cfg := DefaultConfig()
	cfg.ProcZfsPath = procDir
	server := NewServer(cfg, ins)

	// Disallow POST
	postReq := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	postRr := httptest.NewRecorder()
	server.Handler().ServeHTTP(postRr, postReq)
	if postRr.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected 405 for POST, got %d", postRr.Code)
	}

	// Disallow HEAD to prevent waste
	headReq := httptest.NewRequest(http.MethodHead, "/metrics", nil)
	headRr := httptest.NewRecorder()
	server.Handler().ServeHTTP(headRr, headReq)
	if headRr.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected 405 for HEAD, got %d", headRr.Code)
	}

	// GET request
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d, body: %s", rr.Code, rr.Body.String())
	}

	if cl := rr.Header().Get("Content-Length"); cl == "" {
		t.Error("Expected Content-Length header to be set")
	}

	body := rr.Body.String()
	expectedSubstrings := []string{
		"# HELP zfs_pool_reliability_score",
		"# TYPE zfs_pool_reliability_score gauge",
		"# HELP zfs_pool_state_health",
		"# HELP zfs_pool_errors_total",
		"# HELP zfs_pool_scrub_drift_hours",
		"# HELP zfs_hung_commands_total",
		`zfs_pool_reliability_score{pool="tank"}`,
		"zfs_hung_commands_total 0",
	}

	for _, sub := range expectedSubstrings {
		if !strings.Contains(body, sub) {
			t.Errorf("Metrics output missing expected line: %s", sub)
		}
	}
}

func TestHealthzAndReadyzHandlers(t *testing.T) {
	setupFakeZpool(t)
	procDir := setupMockProcfs(t)
	ins := zfspulse.New(
		zfspulse.WithProcZfsPath(procDir),
		zfspulse.WithCommandTimeout(100*time.Millisecond),
	)
	cfg := DefaultConfig()
	cfg.ProcZfsPath = procDir
	server := NewServer(cfg, ins)

	// Test healthz (liveness)
	reqHealth := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rrHealth := httptest.NewRecorder()
	server.Handler().ServeHTTP(rrHealth, reqHealth)

	if rrHealth.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d, body: %s", rrHealth.Code, rrHealth.Body.String())
	}

	bodyHealth := rrHealth.Body.String()
	if !strings.Contains(bodyHealth, `"status":"UP"`) {
		t.Errorf("Expected health status UP, got: %s", bodyHealth)
	}

	// Test readyz (readiness)
	reqReady := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rrReady := httptest.NewRecorder()
	server.Handler().ServeHTTP(rrReady, reqReady)

	if rrReady.Code != http.StatusOK {
		t.Fatalf("Expected status 200 for readyz, got %d, body: %s", rrReady.Code, rrReady.Body.String())
	}

	bodyReady := rrReady.Body.String()
	if !strings.Contains(bodyReady, `"ready":true`) {
		t.Errorf("Expected ready:true in readyz response, got: %s", bodyReady)
	}

	// Disallow POST on healthz
	postReq := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	postRr := httptest.NewRecorder()
	server.Handler().ServeHTTP(postRr, postReq)
	if postRr.Code != http.StatusMethodNotAllowed {
		t.Errorf("Expected 405 for POST /healthz, got %d", postRr.Code)
	}
}

func TestServerStartShutdown(t *testing.T) {
	setupFakeZpool(t)
	procDir := setupMockProcfs(t)
	cfg := DefaultConfig()
	cfg.ProcZfsPath = procDir
	ins := zfspulse.New(zfspulse.WithProcZfsPath(procDir))
	server := NewServer(cfg, ins)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to bind test listener: %v", err)
	}
	defer ln.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.StartWithListener(ln)
	}()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Server exited with unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Server did not stop within timeout")
	}
}

func TestEvaluateHealthLogic(t *testing.T) {
	tests := []struct {
		name         string
		pools        []zfspulse.PoolHealth
		err          error
		isReadiness  bool
		wantStatus   string
		wantHttpCode int
	}{
		{
			name:         "All healthy ONLINE",
			pools:        []zfspulse.PoolHealth{{Name: "tank", State: "ONLINE", ReliabilityScore: 100.0}},
			err:          nil,
			isReadiness:  false,
			wantStatus:   "UP",
			wantHttpCode: http.StatusOK,
		},
		{
			name:         "One pool DEGRADED",
			pools:        []zfspulse.PoolHealth{{Name: "tank", State: "DEGRADED", ReliabilityScore: 70.0}},
			err:          nil,
			isReadiness:  false,
			wantStatus:   "DEGRADED",
			wantHttpCode: http.StatusOK,
		},
		{
			name:         "Partial inspection failure",
			pools:        []zfspulse.PoolHealth{{Name: "tank", State: "ONLINE", ReliabilityScore: 100.0}},
			err:          fmt.Errorf("failed inspecting other pool"),
			isReadiness:  false,
			wantStatus:   "DEGRADED",
			wantHttpCode: http.StatusOK,
		},
		{
			name:         "One pool FAULTED on liveness",
			pools:        []zfspulse.PoolHealth{{Name: "tank", State: "FAULTED", ReliabilityScore: 0.0}},
			err:          nil,
			isReadiness:  false,
			wantStatus:   "CRITICAL",
			wantHttpCode: http.StatusOK, // Process is still responsive
		},
		{
			name:         "One pool FAULTED on readiness",
			pools:        []zfspulse.PoolHealth{{Name: "tank", State: "FAULTED", ReliabilityScore: 0.0}},
			err:          nil,
			isReadiness:  true,
			wantStatus:   "CRITICAL",
			wantHttpCode: http.StatusServiceUnavailable, // Storage unavailable
		},
		{
			name:         "One pool SUSPENDED on liveness",
			pools:        []zfspulse.PoolHealth{{Name: "tank", State: "SUSPENDED", ReliabilityScore: 0.0}},
			err:          nil,
			isReadiness:  false,
			wantStatus:   "CRITICAL",
			wantHttpCode: http.StatusOK,
		},
		{
			name:         "One pool SUSPENDED on readiness",
			pools:        []zfspulse.PoolHealth{{Name: "tank", State: "SUSPENDED", ReliabilityScore: 0.0}},
			err:          nil,
			isReadiness:  true,
			wantStatus:   "CRITICAL",
			wantHttpCode: http.StatusServiceUnavailable,
		},
		{
			name:         "Complete failure no pools found",
			pools:        nil,
			err:          fmt.Errorf("procfs unavailable"),
			isReadiness:  false,
			wantStatus:   "DOWN",
			wantHttpCode: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, code := evaluateHealth(tc.pools, tc.err, tc.isReadiness)
			if st != tc.wantStatus {
				t.Errorf("evaluateHealth status: got %s, want %s", st, tc.wantStatus)
			}
			if code != tc.wantHttpCode {
				t.Errorf("evaluateHealth code: got %d, want %d", code, tc.wantHttpCode)
			}
		})
	}
}

func TestCacheEffectiveness(t *testing.T) {
	setupFakeZpool(t)
	procDir := setupMockProcfs(t)
	ins := zfspulse.New(
		zfspulse.WithProcZfsPath(procDir),
		zfspulse.WithCommandTimeout(100*time.Millisecond),
	)
	cfg := DefaultConfig()
	cfg.ProcZfsPath = procDir
	cfg.ScanCacheTTL = 500 * time.Millisecond
	server := NewServer(cfg, ins)

	ctx := context.Background()

	// 1st call: populates cache
	t0 := time.Now()
	p1, err1 := server.inspectCached(ctx)
	if err1 != nil && len(p1) == 0 {
		t.Fatalf("Initial inspect failed: %v", err1)
	}

	// 2nd call immediately: must hit fast cache path
	p2, err2 := server.inspectCached(ctx)
	if err2 != err1 {
		t.Errorf("Expected identical cached error")
	}
	if len(p2) != len(p1) {
		t.Errorf("Expected identical cached pools length")
	}

	// Ensure fast path was practically instantaneous
	if dur := time.Since(t0); dur > 200*time.Millisecond {
		t.Logf("Cached retrieval took %v", dur)
	}
}

func TestCacheOnFailure(t *testing.T) {
	// Fake zpool that logs invocations and always exits 1 (failure)
	tmpDir := t.TempDir()
	counterFile := filepath.Join(tmpDir, "counter.txt")
	t.Setenv("COUNTER_FILE", counterFile)

	shScript := `#!/bin/sh
echo "call" >> "$COUNTER_FILE"
exit 1
`
	batScript := `@echo off
echo call >> "%COUNTER_FILE%"
exit /b 1
`
	if err := os.WriteFile(filepath.Join(tmpDir, "zpool"), []byte(shScript), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "zpool.bat"), []byte(batScript), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := DefaultConfig()
	cfg.ProcZfsPath = t.TempDir() // empty procfs directory
	cfg.ScanCacheTTL = 5 * time.Second
	ins := zfspulse.New(
		zfspulse.WithProcZfsPath(cfg.ProcZfsPath),
		zfspulse.WithCommandTimeout(100*time.Millisecond),
	)
	server := NewServer(cfg, ins)

	ctx := context.Background()

	// 1st call: fails and should be cached
	_, err1 := server.inspectCached(ctx)
	if err1 == nil {
		t.Fatal("Expected inspectCached to return error on failing zpool, got nil")
	}

	// 2nd call: must hit cache and NOT invoke zpool CLI again
	_, err2 := server.inspectCached(ctx)
	if err2 == nil {
		t.Fatal("Expected second inspectCached call to return cached error, got nil")
	}

	data, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatalf("Failed to read counter file: %v", err)
	}
	calls := strings.Count(string(data), "call")
	if calls != 1 {
		t.Errorf("Expected exactly 1 zpool CLI invocation due to error caching, got %d", calls)
	}
}

func TestClientContextCancellationDoesNotPoisonCache(t *testing.T) {
	setupFakeZpool(t)
	procDir := setupMockProcfs(t)
	ins := zfspulse.New(
		zfspulse.WithProcZfsPath(procDir),
		zfspulse.WithCommandTimeout(1*time.Second),
	)
	cfg := DefaultConfig()
	cfg.ProcZfsPath = procDir
	cfg.ScanCacheTTL = 5 * time.Second
	server := NewServer(cfg, ins)

	// Client 1 with an immediately canceled context
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// inspectCached executes under server's baseCtx, detached from canceledCtx
	pools1, err1 := server.inspectCached(canceledCtx)
	if err1 != nil && len(pools1) == 0 {
		t.Fatalf("Unexpected inspection failure: %v", err1)
	}

	// Client 2 with normal context must receive valid data, NOT a poisoned context.Canceled error
	validCtx := context.Background()
	pools2, err2 := server.inspectCached(validCtx)
	if errors.Is(err2, context.Canceled) {
		t.Fatalf("Cache poisoned: received context.Canceled on valid client request!")
	}
	if len(pools2) == 0 {
		t.Fatalf("Expected valid cached pools for client 2, got 0")
	}
}

func TestServerShutdownCancelsInFlight(t *testing.T) {
	cfg := DefaultConfig()
	server := NewServer(cfg, nil)

	if server.baseCtx.Err() != nil {
		t.Fatalf("baseCtx must be alive before shutdown")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	if !errors.Is(server.baseCtx.Err(), context.Canceled) {
		t.Errorf("Expected baseCtx to be canceled after shutdown, got: %v", server.baseCtx.Err())
	}
}
