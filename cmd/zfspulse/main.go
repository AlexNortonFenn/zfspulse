package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/AlexNortonFenn/zfspulse"
	"github.com/AlexNortonFenn/zfspulse/external_metric"
)

var (
	version = "v0.1.0"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		runStatus(nil)
		return
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "status":
		runStatus(args)
	case "check":
		runCheck(args)
	case "serve":
		runServe(args)
	case "version", "-v", "--version":
		runVersion()
	case "help", "-h", "--help":
		printUsage()
	default:
		if !strings.HasPrefix(cmd, "-") {
			fmt.Fprintf(os.Stderr, "unknown command %q, treating arguments as target pool names\n", cmd)
		}
		runStatus(os.Args[1:])
	}
}

func printUsage() {
	fmt.Println("zfspulse - Zero-CGO, High-Resilience ZFS Health & Reliability Inspector")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  zfspulse [command] [options] [pool...]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  status     Show formatted table or JSON of pool health and error counters")
	fmt.Println("  check      Monitoring check with exit codes (0=OK, 1=WARN, 2=CRIT, 3=UNKNOWN)")
	fmt.Println("  serve      Expose Prometheus /metrics, /healthz and /readyz HTTP endpoints")
	fmt.Println("  version    Display version, commit hash, and build date")
	fmt.Println("  help       Show this help message")
	fmt.Println()
	fmt.Println("Run 'zfspulse [command] -h' for command-specific options.")
}

func runVersion() {
	fmt.Printf("zfspulse %s (commit: %s, built: %s, go: %s)\n", version, commit, date, runtime.Version())
}

// truncateNote truncates long notes to maxRunes cleanly with ellipsis suffix.
func truncateNote(note string, maxRunes int) string {
	if r := []rune(note); len(r) > maxRunes {
		if maxRunes > 3 {
			return string(r[:maxRunes-3]) + "..."
		}
		return string(r[:maxRunes])
	}
	return note
}

// runStatus renders pool health status as an aligned table or structured JSON.
// Adheres to fail-closed principles: returns exit code 1 on partial discovery/read failures,
// exit code 3 if all requested pools failed inspection, and exit code 0 only on total success.
func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOutput := fs.Bool("json", false, "Output results in JSON format")
	procPath := fs.String("proc-path", zfspulse.DefaultProcZfsPath, "Custom Linux procfs kstat path")
	timeoutSec := fs.Int("timeout", 15, "Command execution timeout in seconds")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(3)
	}
	poolNames := fs.Args()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	results, err := inspectTargetPools(ctx, poolNames, *procPath, time.Duration(*timeoutSec)*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		if len(results) == 0 {
			os.Exit(3)
		}
	}

	if *jsonOutput {
		if results == nil {
			results = []zfspulse.PoolHealth{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(results); encErr != nil {
			fmt.Fprintf(os.Stderr, "JSON encoding error: %v\n", encErr)
			os.Exit(3)
		}
		if err != nil {
			os.Exit(1)
		}
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "POOL\tSTATE\tSCORE\tREAD\tWRITE\tCKSUM\tSCRUB DRIFT\tSTATUS")
	for _, p := range results {
		driftStr := fmt.Sprintf("%.1fh", p.ScrubDriftHours)
		if p.ScrubDriftHours >= 24 {
			driftStr = fmt.Sprintf("%.1fd", p.ScrubDriftHours/24.0)
		}
		scoreStr := fmt.Sprintf("%.1f%%", p.ReliabilityScore)

		note := p.StatusNote
		if note == "" {
			if p.State == "ONLINE" {
				note = "nominal"
			} else {
				note = strings.ToLower(p.State)
			}
		}
		// Truncate overly long notes for clean table output
		note = truncateNote(note, 50)

		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n",
			p.Name,
			p.State,
			scoreStr,
			p.ReadErrors,
			p.WriteErrors,
			p.ChecksumErrors,
			driftStr,
			note,
		)
	}
	if flushErr := w.Flush(); flushErr != nil {
		fmt.Fprintf(os.Stderr, "Table flush error: %v\n", flushErr)
	}
	if err != nil {
		os.Exit(1)
	}
}

// runCheck evaluates storage health against warning/critical reliability score thresholds,
// mapping results directly to Nagios/Icinga exit codes (0=OK, 1=WARN, 2=CRIT, 3=UNKNOWN).
// Fail-closed invariant: partial inspection errors or unknown pool states reliably trigger exit code 3
// and populate error telemetry across both plain-text and structured JSON formats without early-exit truncation.
func runCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOutput := fs.Bool("json", false, "Output check results in JSON format")
	procPath := fs.String("proc-path", zfspulse.DefaultProcZfsPath, "Custom Linux procfs kstat path")
	timeoutSec := fs.Int("timeout", 15, "Command execution timeout in seconds")
	warnScore := fs.Float64("warn", zfspulse.ScoreWarnExit, "Score warning threshold (default 75.0)")
	critScore := fs.Float64("crit", zfspulse.ScoreCriticalEnter, "Score critical threshold (default 50.0)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(3)
	}

	if *critScore >= *warnScore {
		fmt.Fprintln(os.Stderr, "error: -crit must be lower than -warn")
		os.Exit(3)
	}

	poolNames := fs.Args()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	results, err := inspectTargetPools(ctx, poolNames, *procPath, time.Duration(*timeoutSec)*time.Second)

	var hasCrit, hasWarn, hasUnknown bool
	var details []string

	if len(results) == 0 {
		hasUnknown = true
		if err != nil {
			details = append(details, fmt.Sprintf("inspection failed: %v", err))
		} else {
			details = append(details, "no pools found")
		}
	} else if err != nil {
		hasUnknown = true
		details = append(details, fmt.Sprintf("partial inspection error: %v", err))
	}

	for _, p := range results {
		sev := p.Severity()
		switch {
		case p.State == "UNKNOWN" || sev == zfspulse.SeverityUnknown:
			hasUnknown = true
			note := p.StatusNote
			if note == "" {
				note = "state unknown or unverified"
			}
			details = append(details, fmt.Sprintf("%s UNKNOWN (%s)", p.Name, note))
		case p.ReliabilityScore < *critScore || sev == zfspulse.SeverityCritical:
			hasCrit = true
			details = append(details, fmt.Sprintf("%s CRITICAL (state=%s, score=%.1f%%, read=%d, write=%d, cksum=%d)",
				p.Name, p.State, p.ReliabilityScore, p.ReadErrors, p.WriteErrors, p.ChecksumErrors))
		case p.ReliabilityScore < *warnScore || sev == zfspulse.SeverityWarn:
			hasWarn = true
			details = append(details, fmt.Sprintf("%s WARN (state=%s, score=%.1f%%, drift=%.1fh)",
				p.Name, p.State, p.ReliabilityScore, p.ScrubDriftHours))
		default:
			details = append(details, fmt.Sprintf("%s OK (score=%.1f%%)", p.Name, p.ReliabilityScore))
		}
	}

	exitCode := 0
	prefix := "OK"
	if hasCrit {
		exitCode = 2
		prefix = "CRITICAL"
	} else if hasUnknown {
		exitCode = 3
		prefix = "UNKNOWN"
	} else if hasWarn {
		exitCode = 1
		prefix = "WARNING"
	}

	if *jsonOutput {
		if results == nil {
			results = []zfspulse.PoolHealth{}
		}
		payload := map[string]any{
			"exit_code": exitCode,
			"pools":     results,
			"summary":   strings.Join(details, "; "),
		}
		if err != nil {
			payload["error"] = err.Error()
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(payload); encErr != nil {
			fmt.Fprintf(os.Stderr, "JSON encoding error: %v\n", encErr)
			os.Exit(3)
		}
	} else {
		fmt.Printf("%s - %s\n", prefix, strings.Join(details, "; "))
	}

	os.Exit(exitCode)
}

// inspectTargetPools inspects explicitly requested pools or auto-discovers all pools on the host.
// Propagates context cancellation down to child zpool CLI subprocesses to prevent orphan processes on SIGINT/SIGTERM.
func inspectTargetPools(ctx context.Context, targetPools []string, procPath string, timeout time.Duration) ([]zfspulse.PoolHealth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ins := zfspulse.New(
		zfspulse.WithProcZfsPath(procPath),
		zfspulse.WithCommandTimeout(timeout),
	)

	if len(targetPools) > 0 {
		var results []zfspulse.PoolHealth
		var errs []error
		for _, name := range targetPools {
			h, err := ins.InspectPool(ctx, name)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				if h.Name == "" {
					h.Name = name
				}
				if h.State == "" {
					h.State = "UNKNOWN"
				}
				if h.StatusNote == "" {
					h.StatusNote = err.Error()
				} else if !strings.Contains(h.StatusNote, err.Error()) {
					h.StatusNote = h.StatusNote + "; " + err.Error()
				}
				results = append(results, h)
				continue
			}
			results = append(results, h)
		}
		if len(errs) > 0 {
			return results, errors.Join(errs...)
		}
		return results, nil
	}

	return ins.InspectAll(ctx)
}

// runServe spins up the HTTP telemetry server exposing Prometheus /metrics, liveness /healthz, and readiness /readyz.
// Pre-binds the TCP listener prior to announcement to eliminate misleading "Starting..." log entries if port is busy.
func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	listenAddr := fs.String("listen", "127.0.0.1:9100", "Address and port to bind metric server")
	procPath := fs.String("proc-path", zfspulse.DefaultProcZfsPath, "Custom Linux procfs kstat path")
	timeoutSec := fs.Int("timeout", 15, "Command execution timeout in seconds")
	driftHours := fs.Float64("scrub-drift-hours", zfspulse.DefaultScrubDriftThresholdHours, "Hours before scrub drift penalty applies")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(3)
	}

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Server bind error: %v\n", err)
		os.Exit(1)
	}
	defer ln.Close()

	cfg := external_metric.Config{
		ListenAddr:               *listenAddr,
		ProcZfsPath:              *procPath,
		CommandTimeout:           time.Duration(*timeoutSec) * time.Second,
		ScrubDriftThresholdHours: *driftHours,
		ScanCacheTTL:             external_metric.DefaultScanCacheTTL,
		ReadTimeout:              external_metric.DefaultReadTimeout,
		ReadHeaderTimeout:        external_metric.DefaultReadHeaderTimeout,
		WriteTimeout:             external_metric.DefaultWriteTimeout,
	}

	ins := zfspulse.New(
		zfspulse.WithProcZfsPath(*procPath),
		zfspulse.WithCommandTimeout(time.Duration(*timeoutSec)*time.Second),
		zfspulse.WithScrubDriftThresholdHours(*driftHours),
	)

	server := external_metric.NewServer(cfg, ins)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	serverErr := make(chan error, 1)
	fmt.Printf("Starting zfspulse metrics server on %s (metrics at /metrics, health at /healthz, ready at /readyz)...\n", ln.Addr().String())
	go func() {
		serverErr <- server.StartWithListener(ln)
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
			os.Exit(1)
		}
	case <-sigCh:
		fmt.Println("\nReceived termination signal, shutting down gracefully...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				fmt.Fprintln(os.Stderr, "Warning: shutdown grace period exceeded, killing remaining connections")
			} else {
				fmt.Fprintf(os.Stderr, "Server shutdown error: %v\n", err)
			}
		} else {
			fmt.Println("Server stopped gracefully")
		}
	}
}
