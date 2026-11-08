// Package executor provides safe execution of ZFS-related commands with
// strict timeouts, memory limits, and suspected-hang detection.
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// MaxBufferCap is the 1 MB memory limit per command output stream.
	// Prevents OOM on commands producing unbounded output (e.g. large
	// SAN/JBOD setups with thousands of disks or pools).
	MaxBufferCap = 1024 * 1024

	// defaultTimeout is used when a caller passes a non-positive timeout.
	defaultTimeout = 5 * time.Second

	// processWaitDelay is the maximum time os/exec waits for I/O pipes
	// to close after the process has been killed via CommandContext.
	processWaitDelay = 2 * time.Second

	// processGraceDelay is the extra time granted to cmd.Run to return
	// after WaitDelay has elapsed, before declaring a suspected hang.
	processGraceDelay = 100 * time.Millisecond
)

// ErrCommandTimeout is wrapped into CmdResult.Err when our internal execution
// deadline expires before the command completes.
var ErrCommandTimeout = errors.New("executor: command timeout")

// errInternalTimeout is a private sentinel passed to context.WithTimeoutCause.
// Being private, external callers cannot inject it into the parent context,
// completely eliminating sentinel collision.
var errInternalTimeout = errors.New("executor: internal timer fired")

// SafeBuffer is a concurrency-safe buffer with a strict memory cap and
// truncation detection.
//
// It is designed for use as the Stdout/Stderr sink of an os/exec.Cmd. When
// the cap is reached, further writes are silently discarded and Truncated()
// returns true. This intentionally deviates from standard io.Writer semantics
// (returning len(p) on partial/discarded writes) to prevent child processes from
// blocking on a full pipe while strictly bounding memory consumption.
type SafeBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

// Write implements io.Writer. Excess bytes beyond MaxBufferCap are discarded;
// the returned n always equals len(p) so that callers do not encounter short-write
// errors or retry loops. Use Truncated() to detect data loss.
func (s *SafeBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	if origLen == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	remaining := MaxBufferCap - s.buf.Len()
	if remaining <= 0 {
		s.truncated = true
		return origLen, nil
	}

	if origLen > remaining {
		s.truncated = true
		p = p[:remaining]
	}

	// bytes.Buffer.Write only errors on allocation failure (OOM panic).
	_, _ = s.buf.Write(p)
	return origLen, nil
}

// String returns the buffered content captured so far. Safe for concurrent access.
func (s *SafeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Truncated reports whether any writes were discarded due to MaxBufferCap.
func (s *SafeBuffer) Truncated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.truncated
}

// CmdResult contains command output and execution telemetry.
type CmdResult struct {
	// Stdout is the raw standard output, possibly truncated (see StdoutTruncated).
	Stdout string

	// Stderr is the trimmed standard error, possibly truncated (see StderrTruncated).
	Stderr string

	// Err is the error returned by cmd.Run, the context, or exec sentinels.
	// When TimedOut is true, Err wraps ErrCommandTimeout.
	Err error

	// TimedOut is true when our internal deadline expired before command completion.
	// False for parent-context cancellations.
	TimedOut bool

	// SuspectedKernelHang indicates that cmd.Run did not return within
	// WaitDelay + processGraceDelay after context cancellation.
	// This may indicate a process stuck in kernel D-state, an orphaned child
	// process holding inherited I/O pipes open, or another OS-level issue.
	SuspectedKernelHang bool

	// StdoutTruncated indicates Stdout exceeded MaxBufferCap and was capped.
	StdoutTruncated bool

	// StderrTruncated indicates Stderr exceeded MaxBufferCap and was capped.
	StderrTruncated bool
}

// runCommand executes name with args under the specified timeout and returns a
// fully populated CmdResult. It is the single core executor for all Exec* helpers.
func runCommand(
	ctx context.Context,
	timeout time.Duration,
	name string,
	args ...string,
) CmdResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	cmdCtx, cancel := context.WithTimeoutCause(ctx, timeout, errInternalTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, name, args...)
	cmd.WaitDelay = processWaitDelay
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")

	var stdoutBuf, stderrBuf SafeBuffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// Buffered channel ensures the goroutine exits cleanly even if runCommand returns early.
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- cmd.Run()
	}()

	build := func(err error, timedOut, suspectedHang bool) CmdResult {
		// If our deadline fired and the command failed or timed out, wrap our exported sentinel
		// and guarantee context.DeadlineExceeded is present in the error chain across all OS platforms.
		if timedOut && err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				err = fmt.Errorf("%w: %w: %w", ErrCommandTimeout, context.DeadlineExceeded, err)
			} else {
				err = fmt.Errorf("%w: %w", ErrCommandTimeout, err)
			}
		}

		return CmdResult{
			Stdout:              stdoutBuf.String(),
			Stderr:              strings.TrimSpace(stderrBuf.String()),
			Err:                 err,
			TimedOut:            timedOut,
			SuspectedKernelHang: suspectedHang,
			StdoutTruncated:     stdoutBuf.Truncated(),
			StderrTruncated:     stderrBuf.Truncated(),
		}
	}

	// 1. Non-blocking drain fast-path: if the command already finished, take it immediately.
	// Guarantees an already-completed command never loses the select race against cmdCtx.Done().
	select {
	case err := <-resultCh:
		return build(err, false, false)
	default:
	}

	// 2. Block until either command finishes or context cancels/times out.
	select {
	case err := <-resultCh:
		return build(err, false, false)
	case <-cmdCtx.Done():
	}

	// Cancellation or timeout occurred. Determine if our internal deadline was the cause.
	ourDeadline := errors.Is(context.Cause(cmdCtx), errInternalTimeout)

	timer := time.NewTimer(cmd.WaitDelay + processGraceDelay)
	defer timer.Stop()

	select {
	case err := <-resultCh:
		// Process exited within the grace window.
		// If err == nil, it completed cleanly right at the wire without being killed.
		if err == nil {
			return build(nil, false, false)
		}
		return build(err, ourDeadline, false)

	case <-timer.C:
		// Process failed to exit after SIGKILL + WaitDelay + grace window.
		// Wedged in kernel D-state or orphaned pipe leak.
		return build(cmdCtx.Err(), ourDeadline, true)
	}
}

// ExecZpoolStatus runs `zpool status <poolName>` and returns its output with telemetry.
func ExecZpoolStatus(
	ctx context.Context,
	poolName string,
	timeout time.Duration,
) CmdResult {
	return runCommand(ctx, timeout, "zpool", "status", poolName)
}

// ExecZpoolList runs `zpool list -H -o name` and returns its output with telemetry.
func ExecZpoolList(
	ctx context.Context,
	timeout time.Duration,
) CmdResult {
	return runCommand(ctx, timeout, "zpool", "list", "-H", "-o", "name")
}
