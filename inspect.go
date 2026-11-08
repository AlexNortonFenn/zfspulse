package zfspulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlexNortonFenn/zfspulse/internal/executor"
	"github.com/AlexNortonFenn/zfspulse/internal/parser"
	"github.com/AlexNortonFenn/zfspulse/internal/procfs"
)

// zpoolCmdResult encapsulates isolated command execution outputs.
type zpoolCmdResult = executor.CmdResult

// zpoolCmdRunner defines the pluggable command runner signature for production and testing.
type zpoolCmdRunner func(ctx context.Context, poolName string) zpoolCmdResult

// zpoolListRunner defines the pluggable command runner signature for pool discovery.
type zpoolListRunner func(ctx context.Context) (stdout, stderr string, err error)

// appendStatusNote safely concatenates diagnostic messages without duplicating.
func appendStatusNote(h *PoolHealth, note string) {
	if h == nil {
		return
	}
	h.StatusNote = parser.AppendStatusNote(h.StatusNote, note)
}

// Inspector provides standalone ZFS pool health, wear, and reliability inspection.
// Conforms to NIS2 and DORA standards: 0 CGO, zero external dependencies, fail-closed telemetry.
type Inspector struct {
	opts          Options
	mu            sync.RWMutex
	lastScrub     map[string]time.Time
	hungCommands  atomic.Uint64
	cmdRunner     zpoolCmdRunner
	cmdListRunner zpoolListRunner
}

// New creates and initializes a new ZFS Inspector with the provided options.
func New(opts ...Option) *Inspector {
	cfg := DefaultOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	ins := &Inspector{
		opts:      cfg,
		lastScrub: make(map[string]time.Time),
	}
	ins.cmdRunner = ins.defaultExecZpoolStatus
	ins.cmdListRunner = ins.defaultExecZpoolList
	return ins
}

// Options returns a copy of the Inspector's current options.
func (ins *Inspector) Options() Options {
	return ins.opts
}

// HungCommands returns the total number of commands that timed out in kernel D-state.
func (ins *Inspector) HungCommands() uint64 {
	return ins.hungCommands.Load()
}

// setLastScrub safely records the scrub completion time under inspector lock.
func (ins *Inspector) setLastScrub(poolName string, t time.Time) {
	ins.mu.Lock()
	defer ins.mu.Unlock()
	ins.lastScrub[poolName] = t
}

// defaultExecZpoolStatus runs `zpool status <pool>` via internal executor.
func (ins *Inspector) defaultExecZpoolStatus(ctx context.Context, poolName string) zpoolCmdResult {
	return executor.ExecZpoolStatus(ctx, poolName, ins.opts.CommandTimeout)
}

// defaultExecZpoolList runs `zpool list -H -o name` via internal executor.
func (ins *Inspector) defaultExecZpoolList(ctx context.Context) (string, string, error) {
	res := executor.ExecZpoolList(ctx, ins.opts.CommandTimeout)
	if res.TimedOut {
		ins.hungCommands.Add(1)
	}
	return res.Stdout, res.Stderr, res.Err
}

// readProcfsPoolState delegates to internal procfs package.
func (ins *Inspector) readProcfsPoolState(poolName string) (string, bool) {
	return procfs.ReadPoolState(ins.opts.ProcZfsPath, poolName)
}

// parseZpoolStatus executes command runner and parses the status output.
func (ins *Inspector) parseZpoolStatus(ctx context.Context, poolName string, health *PoolHealth) error {
	res := ins.cmdRunner(ctx, poolName)

	if ctx.Err() != nil || (errors.Is(res.Err, context.Canceled) && !res.TimedOut) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return res.Err
	}

	if res.TimedOut {
		timeout := ins.opts.CommandTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}

		// 1. Genuine kernel D-state hang: process did not exit even after SIGKILL + WaitDelay + grace window.
		if res.SuspectedKernelHang {
			ins.hungCommands.Add(1)
			stderrStr := res.Stderr
			slog.Error("zpool status execution wedged in kernel D-state (uninterruptible sleep)",
				"pool", poolName,
				"timeout", timeout.String(),
				"stderr", stderrStr,
				"hung_commands_total", ins.hungCommands.Load(),
			)
			health.RaiseState("FAULTED")
			note := fmt.Sprintf("zpool CLI wedged in kernel D-state after %v", timeout)
			if stderrStr != "" {
				note += "; stderr: " + stderrStr
			}
			appendStatusNote(health, note)
			return fmt.Errorf("zpool status hung in kernel D-state for %s: %w", poolName, res.Err)
		}

		// 2. Open descriptor leak: process was killed, but child processes held stdout/stderr pipes open.
		if errors.Is(res.Err, exec.ErrWaitDelay) {
			slog.Warn("zpool process exited but leaked open I/O descriptors (WaitDelay expired)",
				"pool", poolName,
				"stderr", res.Stderr,
			)
			health.RaiseState("DEGRADED")
			appendStatusNote(health, "zpool leaked descriptors (WaitDelay exceeded)")
			return fmt.Errorf("zpool wait delay exceeded for %s: %w", poolName, res.Err)
		}

		// 3. Software command timeout: process was killed cleanly via SIGKILL, real state unverified (Fail-Closed).
		slog.Warn("zpool status command timed out and was killed",
			"pool", poolName,
			"timeout", timeout.String(),
		)
		health.RaiseState("UNKNOWN")
		appendStatusNote(health, fmt.Sprintf("zpool CLI timed out after %v (state unverified)", timeout))
		return fmt.Errorf("zpool status timed out for %s: %w", poolName, res.Err)
	}

	if res.Err != nil {
		if errors.Is(res.Err, exec.ErrWaitDelay) {
			slog.Warn("zpool process exited but leaked open I/O descriptors (WaitDelay expired)",
				"pool", poolName,
				"stderr", res.Stderr,
			)
			health.RaiseState("DEGRADED")
			appendStatusNote(health, "zpool leaked descriptors (WaitDelay exceeded)")
			return fmt.Errorf("zpool wait delay exceeded for %s: %w", poolName, res.Err)
		}

		var execErr *exec.Error
		if errors.As(res.Err, &execErr) {
			slog.Error("zpool CLI binary unavailable on host", "pool", poolName, "error", execErr)
			health.RaiseState("UNAVAIL")
			appendStatusNote(health, "zpool CLI binary not found: "+execErr.Error())
			return fmt.Errorf("zpool binary missing: %w", execErr)
		}

		if strings.Contains(strings.ToLower(res.Stderr), "no such pool") {
			slog.Warn("ZFS pool does not exist on host (configuration error)", "pool", poolName, "stderr", res.Stderr)
			health.RaiseState("UNAVAIL")
			appendStatusNote(health, fmt.Sprintf("pool %q not found on host (config mismatch)", poolName))
			return fmt.Errorf("pool %s not found: %w", poolName, res.Err)
		}

		slog.Warn("zpool status execution failed", "pool", poolName, "error", res.Err, "stderr", res.Stderr)
		health.RaiseState("FAULTED")
		note := fmt.Sprintf("zpool CLI failed: %v", res.Err)
		if res.Stderr != "" {
			note += "; stderr: " + res.Stderr
		}
		appendStatusNote(health, note)
		return fmt.Errorf("zpool status failed for %s: %w (stderr: %s)", poolName, res.Err, res.Stderr)
	}

	if res.StdoutTruncated {
		slog.Warn("zpool status stdout exceeded 1MB memory limit and was truncated", "pool", poolName)
		health.RaiseState("DEGRADED")
		appendStatusNote(health, "zpool output truncated at 1MB (buffer cap reached)")
	}

	if res.StderrTruncated {
		slog.Warn("zpool status stderr exceeded 1MB memory limit and was truncated", "pool", poolName)
		appendStatusNote(health, "zpool stderr truncated at 1MB")
	}

	return ins.parseZpoolStatusReader(strings.NewReader(res.Stdout), poolName, health)
}

// parseZpoolStatusReader delegates streaming parsing to internal parser.
func (ins *Inspector) parseZpoolStatusReader(r io.Reader, poolName string, health *PoolHealth) error {
	res, err := parser.ParseZpoolStatusReader(r, poolName)
	if res.PoolState != "" && res.PoolState != "UNKNOWN" {
		health.RaiseState(res.PoolState)
	}
	health.ReadErrors = parser.AddSaturatingUint64(health.ReadErrors, res.ReadErrors)
	health.WriteErrors = parser.AddSaturatingUint64(health.WriteErrors, res.WriteErrors)
	health.ChecksumErrors = parser.AddSaturatingUint64(health.ChecksumErrors, res.ChecksumErrors)

	if res.ScrubInProgress {
		health.ScrubInProgress = true
	}
	if res.HasLastScrub {
		ins.setLastScrub(poolName, res.LastScrubTime)
	} else if res.NoScrubRequested {
		ins.setLastScrub(poolName, time.Unix(0, 0))
	}

	if res.StatusNote != "" {
		appendStatusNote(health, res.StatusNote)
	}

	return err
}

// InspectPool reads pool state from Linux procfs or zpool utility without holding locks during I/O.
// Conforms strictly to fail-closed principles: if state cannot be confirmed ONLINE, it defaults to UNKNOWN.
func (ins *Inspector) InspectPool(ctx context.Context, poolName string) (PoolHealth, error) {
	poolName = strings.TrimSpace(poolName)
	if poolName == "" {
		return PoolHealth{State: "UNKNOWN", StatusNote: "zfspulse: pool name cannot be empty"}, errors.New("zfspulse: pool name cannot be empty")
	}
	if strings.ContainsAny(poolName, `/\`) || poolName == "." || poolName == ".." {
		errMsg := fmt.Sprintf("zfspulse: invalid pool name %q (path traversal forbidden)", poolName)
		return PoolHealth{Name: poolName, State: "UNKNOWN", StatusNote: errMsg}, errors.New(errMsg)
	}

	health := PoolHealth{
		Name:             poolName,
		State:            "UNKNOWN",
		ReliabilityScore: 0.0,
	}

	var errs []error

	// 1. Try reading Linux OpenZFS procfs kstat first
	if ins.opts.ProcZfsPath != "" {
		if st, ok := ins.readProcfsPoolState(poolName); ok {
			health.RaiseState(st)
		}
	}

	// 2. Query zpool status for scrub timestamp, device tree state, and error counters
	if err := ins.parseZpoolStatus(ctx, poolName, &health); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return health, err
		}
		errs = append(errs, err)
		if health.StatusNote == "" {
			health.StatusNote = "zpool inspection error: " + err.Error()
		}
	}

	// Calculate scrub drift
	ins.mu.RLock()
	lastScrub, ok := ins.lastScrub[poolName]
	ins.mu.RUnlock()
	if !ok {
		ins.mu.Lock()
		lastScrub, ok = ins.lastScrub[poolName]
		if !ok {
			lastScrub = time.Unix(0, 0)
			ins.lastScrub[poolName] = lastScrub
		}
		ins.mu.Unlock()
	}

	drift := time.Since(lastScrub).Hours()
	if drift < 0 || health.ScrubInProgress {
		drift = 0
	}
	health.ScrubDriftHours = drift

	// Compute Pool Reliability Index (0-100%)
	health.ReliabilityScore = calculateReliabilityScore(&health, float64(ins.opts.ScrubDriftThresholdHours))

	return health, errors.Join(errs...)
}

// DiscoverPools attempts to discover available ZFS pool names via procfs or 'zpool list'.
func (ins *Inspector) DiscoverPools(ctx context.Context) ([]string, error) {
	if ins.opts.ProcZfsPath != "" {
		pools, err := procfs.DiscoverPools(ins.opts.ProcZfsPath)
		if err == nil && len(pools) > 0 {
			return pools, nil
		}
	}

	stdout, stderr, err := ins.cmdListRunner(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if strings.Contains(strings.ToLower(stderr), "no pools available") {
			return nil, nil
		}
		return nil, fmt.Errorf("zpool list failed: %w (stderr: %s)", err, stderr)
	}

	lines := strings.Split(stdout, "\n")
	var pools []string
	seen := make(map[string]bool)
	for _, l := range lines {
		p := strings.TrimSpace(l)
		if p != "" && !seen[p] {
			pools = append(pools, p)
			seen[p] = true
		}
	}
	sort.Strings(pools)
	return pools, nil
}

// InspectAll discovers all active pools on the system and inspects their health.
func (ins *Inspector) InspectAll(ctx context.Context) ([]PoolHealth, error) {
	pools, err := ins.DiscoverPools(ctx)
	if err != nil {
		return nil, err
	}
	if len(pools) == 0 {
		return nil, nil
	}

	results := make([]PoolHealth, 0, len(pools))
	var errs []error
	for _, p := range pools {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		h, err := ins.InspectPool(ctx, p)
		if err != nil {
			errs = append(errs, err)
		}
		results = append(results, h)
	}

	return results, errors.Join(errs...)
}
