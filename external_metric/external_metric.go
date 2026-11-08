package external_metric

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlexNortonFenn/zfspulse"
)

// Default timeouts and cache configuration for HTTP server compliance.
const (
	DefaultReadTimeout       = 5 * time.Second
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultWriteTimeout      = 10 * time.Second
	DefaultScanCacheTTL      = 5 * time.Second
)

// Config defines the runtime configuration for the external metric HTTP server.
type Config struct {
	ListenAddr               string        // Address and port to bind (default "127.0.0.1:9100")
	ProcZfsPath              string        // Custom procfs kstat path
	CommandTimeout           time.Duration // Subprocess timeout for zpool CLI
	ScrubDriftThresholdHours float64       // Hours before scrub drift penalty applies
	ScanCacheTTL             time.Duration // Cache validity duration for pool inspection results
	ReadTimeout              time.Duration // HTTP read timeout
	ReadHeaderTimeout        time.Duration // HTTP read header timeout (slowloris mitigation)
	WriteTimeout             time.Duration // HTTP write timeout
	Logger                   *slog.Logger  // Structured logger instance (defaults to slog.Default())
}

// DefaultConfig returns baseline production configuration complying with NIS2 and DORA.
func DefaultConfig() Config {
	return Config{
		ListenAddr:               "127.0.0.1:9100",
		ProcZfsPath:              zfspulse.DefaultProcZfsPath,
		CommandTimeout:           zfspulse.DefaultCommandTimeout,
		ScrubDriftThresholdHours: zfspulse.DefaultScrubDriftThresholdHours,
		ScanCacheTTL:             DefaultScanCacheTTL,
		ReadTimeout:              DefaultReadTimeout,
		ReadHeaderTimeout:        DefaultReadHeaderTimeout,
		WriteTimeout:             DefaultWriteTimeout,
		Logger:                   nil,
	}
}

// cachedSnapshot holds a timestamped snapshot of inspected pools and optional scan error.
type cachedSnapshot struct {
	pools []zfspulse.PoolHealth
	err   error
	at    time.Time
}

// Server provides an isolated HTTP endpoint exposing OpenMetrics and health probes.
type Server struct {
	cfg        Config
	inspector  *zfspulse.Inspector
	httpServer *http.Server
	logger     *slog.Logger
	baseCtx    context.Context
	baseCancel context.CancelFunc

	cacheMu sync.RWMutex
	cache   cachedSnapshot
	fetchMu sync.Mutex
}

// NewServer creates a new external metric server instance.
func NewServer(cfg Config, ins *zfspulse.Inspector) *Server {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:9100"
	}
	if cfg.ScanCacheTTL <= 0 {
		cfg.ScanCacheTTL = DefaultScanCacheTTL
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = DefaultReadTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.ReadHeaderTimeout <= 0 {
		cfg.ReadHeaderTimeout = cfg.ReadTimeout
		if cfg.ReadHeaderTimeout <= 0 {
			cfg.ReadHeaderTimeout = DefaultReadHeaderTimeout
		}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if ins == nil {
		ins = zfspulse.New(
			zfspulse.WithProcZfsPath(cfg.ProcZfsPath),
			zfspulse.WithCommandTimeout(cfg.CommandTimeout),
			zfspulse.WithScrubDriftThresholdHours(cfg.ScrubDriftThresholdHours),
		)
	}

	baseCtx, baseCancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:        cfg,
		inspector:  ins,
		logger:     logger,
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/", s.handleRoot)

	s.httpServer = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
	}

	return s
}

// Handler returns the underlying http.Handler for testing or embedding.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// inspectCached retrieves cached pool health or safely executes an upstream refresh.
// Architecture & Concurrency invariants:
// 1. cacheMu (RWMutex) guards snapshot assignment and reads in nanoseconds — NEVER held across I/O.
// 2. fetchMu (Mutex) serializes background refreshes to eliminate dogpiling/thundering herds on zpool CLI.
// 3. !snap.at.IsZero() guarantees full failure caching: broken storage or missing binaries do not trigger subprocess storms.
func (s *Server) inspectCached(ctx context.Context) ([]zfspulse.PoolHealth, error) {
	// Fast path: valid cache read under read lock
	s.cacheMu.RLock()
	snap := s.cache
	s.cacheMu.RUnlock()

	if !snap.at.IsZero() && time.Since(snap.at) < s.cfg.ScanCacheTTL {
		return snap.pools, snap.err
	}

	// Slow path: serialize background update to avoid thundering herd on zpool CLI
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()

	// Double-check cache under read lock after acquiring fetchMu
	s.cacheMu.RLock()
	snap = s.cache
	s.cacheMu.RUnlock()

	if !snap.at.IsZero() && time.Since(snap.at) < s.cfg.ScanCacheTTL {
		return snap.pools, snap.err
	}

	// Determine execution timeout with safety fallback
	cmdTimeout := s.cfg.CommandTimeout
	if cmdTimeout <= 0 {
		cmdTimeout = zfspulse.DefaultCommandTimeout
	}

	// Decouple storage inspection from individual incoming HTTP client contexts to eliminate
	// client-induced cache poisoning, but bind to server baseCtx to ensure clean shutdown.
	inspectCtx, cancel := context.WithTimeout(s.baseCtx, cmdTimeout)
	defer cancel()

	// Execute InspectAll WITHOUT holding cacheMu, so concurrent readers are never blocked
	pools, err := s.inspector.InspectAll(inspectCtx)

	// Cache successful runs, storage failures, and internal DeadlineExceeded (to mitigate subprocess storm).
	// Never cache transient context.Canceled triggered by server shutdown.
	if err == nil || !errors.Is(err, context.Canceled) {
		s.cacheMu.Lock()
		s.cache = cachedSnapshot{
			pools: pools,
			err:   err,
			at:    time.Now(),
		}
		s.cacheMu.Unlock()
	}

	return pools, err
}

// Start begins listening on the configured address. Blocks until server closes or error occurs.
func (s *Server) Start() error {
	s.logger.Info("Starting zfspulse external metric server", "address", s.cfg.ListenAddr)
	return s.httpServer.ListenAndServe()
}

// StartWithListener serves HTTP requests on an existing listener (useful for tests or socket activation).
func (s *Server) StartWithListener(ln net.Listener) error {
	s.logger.Info("Starting zfspulse external metric server on listener", "address", ln.Addr().String())
	return s.httpServer.Serve(ln)
}

// Shutdown gracefully shuts down the HTTP server and cancels in-flight inspection routines.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("Shutting down zfspulse external metric server")
	s.baseCancel()
	return s.httpServer.Shutdown(ctx)
}

// handleMetrics handles Prometheus scrape requests.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	pools, err := s.inspectCached(ctx)
	if err != nil && len(pools) == 0 {
		s.logger.Error("Failed to collect pool telemetry for metrics", "error", err)
		http.Error(w, fmt.Sprintf("Error collecting metrics: %v", err), http.StatusInternalServerError)
		return
	}

	var sb strings.Builder

	// Header and descriptions
	sb.WriteString("# HELP zfs_pool_reliability_score Deterministic reliability score (0-100%).\n")
	sb.WriteString("# TYPE zfs_pool_reliability_score gauge\n")
	for _, p := range pools {
		fmt.Fprintf(&sb, "zfs_pool_reliability_score{pool=%q} %.2f\n", p.Name, p.ReliabilityScore)
	}

	sb.WriteString("\n# HELP zfs_pool_state_health Operational state one-hot encoding (1 for active state, 0 otherwise; state=\"UNKNOWN\" indicates telemetry collection failure or unverified host state, not a native OpenZFS pool state).\n")
	sb.WriteString("# TYPE zfs_pool_state_health gauge\n")
	allStates := []string{"ONLINE", "DEGRADED", "FAULTED", "UNAVAIL", "OFFLINE", "REMOVED", "SUSPENDED", "UNKNOWN"}
	for _, p := range pools {
		for _, st := range allStates {
			val := 0
			if p.State == st {
				val = 1
			}
			fmt.Fprintf(&sb, "zfs_pool_state_health{pool=%q,state=%q} %d\n", p.Name, st, val)
		}
	}

	sb.WriteString("\n# HELP zfs_pool_errors_total Cumulative leaf device error counters (resets to 0 on zpool clear or pool import).\n")
	sb.WriteString("# TYPE zfs_pool_errors_total counter\n")
	for _, p := range pools {
		fmt.Fprintf(&sb, "zfs_pool_errors_total{pool=%q,type=\"read\"} %d\n", p.Name, p.ReadErrors)
		fmt.Fprintf(&sb, "zfs_pool_errors_total{pool=%q,type=\"write\"} %d\n", p.Name, p.WriteErrors)
		fmt.Fprintf(&sb, "zfs_pool_errors_total{pool=%q,type=\"checksum\"} %d\n", p.Name, p.ChecksumErrors)
	}

	sb.WriteString("\n# HELP zfs_pool_scrub_drift_hours Hours elapsed since last scrub completion.\n")
	sb.WriteString("# TYPE zfs_pool_scrub_drift_hours gauge\n")
	for _, p := range pools {
		fmt.Fprintf(&sb, "zfs_pool_scrub_drift_hours{pool=%q} %.2f\n", p.Name, p.ScrubDriftHours)
	}

	sb.WriteString("\n# HELP zfs_pool_scrub_in_progress Active scrub operation indicator (1=in progress, 0=idle).\n")
	sb.WriteString("# TYPE zfs_pool_scrub_in_progress gauge\n")
	for _, p := range pools {
		val := 0
		if p.ScrubInProgress {
			val = 1
		}
		fmt.Fprintf(&sb, "zfs_pool_scrub_in_progress{pool=%q} %d\n", p.Name, val)
	}

	sb.WriteString("\n# HELP zfs_hung_commands_total Total number of commands that timed out in kernel D-state.\n")
	sb.WriteString("# TYPE zfs_hung_commands_total counter\n")
	fmt.Fprintf(&sb, "zfs_hung_commands_total %d\n", s.inspector.HungCommands())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(sb.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, sb.String())
}

// evaluateHealth performs fail-closed evaluation of pool health status and HTTP status codes.
// Semantics:
// - Total failure (err != nil && len(pools) == 0): status DOWN, HTTP 503 for all probes.
// - Severe outage (FAULTED/UNAVAIL/OFFLINE/REMOVED/SUSPENDED): status CRITICAL.
//   * Liveness (/healthz): HTTP 200 (process is responsive and running).
//   * Readiness (/readyz): HTTP 503 (storage is degraded/unusable, drop from traffic rotation).
// - Redundancy loss (DEGRADED/UNKNOWN or partial errors): status DEGRADED, HTTP 200 (storage serving I/O).
// - Healthy (all ONLINE with nominal score): status UP, HTTP 200.
func evaluateHealth(pools []zfspulse.PoolHealth, err error, isReadiness bool) (statusStr string, httpCode int) {
	if err != nil && len(pools) == 0 {
		return "DOWN", http.StatusServiceUnavailable
	}

	hasCritical := false
	hasDegraded := err != nil && len(pools) > 0

	for _, p := range pools {
		switch p.State {
		case "FAULTED", "UNAVAIL", "OFFLINE", "REMOVED", "SUSPENDED":
			hasCritical = true
		case "DEGRADED":
			hasDegraded = true
		case "UNKNOWN":
			hasDegraded = true
		default:
			// For ONLINE or unclassified states, delegate to canonical Severity classification
			switch p.Severity() {
			case zfspulse.SeverityCritical:
				hasCritical = true
			case zfspulse.SeverityWarn, zfspulse.SeverityUnknown:
				hasDegraded = true
			}
		}
	}

	if hasCritical {
		if isReadiness {
			return "CRITICAL", http.StatusServiceUnavailable
		}
		return "CRITICAL", http.StatusOK
	}

	if hasDegraded {
		return "DEGRADED", http.StatusOK
	}

	return "UP", http.StatusOK
}

// handleHealthz handles liveness probes for orchestration (Kubernetes/Consul).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	pools, err := s.inspectCached(ctx)
	statusStr, httpCode := evaluateHealth(pools, err, false)

	resp := map[string]any{
		"status":        statusStr,
		"pools_count":   len(pools),
		"hung_commands": s.inspector.HungCommands(),
		"timestamp":     time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpCode)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleReadyz handles readiness probes for orchestration (Kubernetes/Consul).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	pools, err := s.inspectCached(ctx)
	statusStr, httpCode := evaluateHealth(pools, err, true)

	resp := map[string]any{
		"status":        statusStr,
		"ready":         httpCode == http.StatusOK,
		"pools_count":   len(pools),
		"hung_commands": s.inspector.HungCommands(),
		"timestamp":     time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpCode)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleRoot returns service metadata and links.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := `<!DOCTYPE html>
<html>
<head><title>zfspulse Metric Server</title></head>
<body>
<h1>zfspulse Telemetry Engine</h1>
<p>Storage reliability inspector conforming to NIS2 Art. 21, DORA, and Russian Banking/FSTEC ICT standards.</p>
<ul>
<li><a href="/metrics">/metrics</a> - Prometheus / OpenMetrics exposition</li>
<li><a href="/healthz">/healthz</a> - Health and liveness probe</li>
<li><a href="/readyz">/readyz</a> - Readiness probe</li>
</ul>
</body>
</html>`
	_, _ = io.WriteString(w, html)
}
