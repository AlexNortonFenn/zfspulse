package zfspulse

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	"github.com/AlexNortonFenn/zfspulse/internal/executor"
)

type safeBuffer = executor.SafeBuffer

const maxBufferCap = executor.MaxBufferCap

func TestSafeBufferCap(t *testing.T) {
	// Case 1: Full overflow write (1.2 MB data against 1 MB cap)
	var buf safeBuffer
	data := make([]byte, 1200*1024)
	for i := range data {
		data[i] = 'A'
	}

	n, err := buf.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected n=%d, got %d", len(data), n)
	}

	if len(buf.String()) != maxBufferCap {
		t.Errorf("Expected capped size %d, got %d", maxBufferCap, len(buf.String()))
	}
	if !buf.Truncated() {
		t.Errorf("Expected buffer to be marked as Truncated")
	}

	// Case 2: Partial overflow write (1000 KB existing + 100 KB write -> consumes 24 KB, capped at 1024 KB)
	var buf2 safeBuffer
	if _, err := buf2.Write(make([]byte, 1000*1024)); err != nil {
		t.Fatalf("Initial write failed: %v", err)
	}
	n2, err2 := buf2.Write(make([]byte, 100*1024))
	if err2 != nil {
		t.Fatalf("Partial write failed: %v", err2)
	}
	if n2 != 100*1024 {
		t.Errorf("Expected n2=%d, got %d", 100*1024, n2)
	}
	if len(buf2.String()) != maxBufferCap {
		t.Errorf("Expected capped size %d, got %d", maxBufferCap, len(buf2.String()))
	}
	if !buf2.Truncated() {
		t.Errorf("Expected buf2 to be marked as Truncated on partial overflow")
	}
}

func TestGracefulShutdownNoHungCommands(t *testing.T) {
	ins := New()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var health PoolHealth
	err := ins.parseZpoolStatus(ctx, "tank", &health)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Expected context.Canceled on shutdown, got: %v", err)
	}

	if ins.HungCommands() != 0 {
		t.Errorf("Expected hungCommands=0 on graceful shutdown, got %d", ins.HungCommands())
	}

	if health.State == "FAULTED" {
		t.Errorf("Pool must NOT be marked FAULTED on parent shutdown cancellation")
	}
}

func TestNoSuchPoolConfigMismatch(t *testing.T) {
	ins := New()
	ins.cmdRunner = func(ctx context.Context, poolName string) zpoolCmdResult {
		return zpoolCmdResult{
			Stderr: "cannot open 'nonexistent_pool_123': no such pool",
			Err:    errors.New("exit status 1"),
		}
	}

	ctx := context.Background()
	health, err := ins.InspectPool(ctx, "nonexistent_pool_123")
	if health.State == "ONLINE" {
		t.Errorf("Expected non-ONLINE state for nonexistent pool, got: %s", health.State)
	}
	if err == nil {
		t.Errorf("Expected error for nonexistent pool when CLI/procfs fails")
	}
}

func TestNoSuchPoolElevatesOnlineStateToUnavail(t *testing.T) {
	ins := New()
	ins.cmdRunner = func(ctx context.Context, poolName string) zpoolCmdResult {
		return zpoolCmdResult{
			Stderr: "cannot open 'tank': no such pool",
			Err:    errors.New("exit status 1"),
		}
	}

	health := PoolHealth{
		Name:  "tank",
		State: "ONLINE", // Previously deemed ONLINE by procfs
	}

	err := ins.parseZpoolStatus(context.Background(), "tank", &health)
	if err == nil {
		t.Fatal("Expected error for missing pool")
	}

	if health.State != "UNAVAIL" {
		t.Errorf("Expected State to be elevated to UNAVAIL from ONLINE, got %s", health.State)
	}
	if !strings.Contains(health.StatusNote, "not found on host") {
		t.Errorf("Expected StatusNote to mention config mismatch, got: %s", health.StatusNote)
	}
}

func TestMissingZpoolBinaryElevatesOnlineToUnavail(t *testing.T) {
	ins := New()
	ins.cmdRunner = func(ctx context.Context, poolName string) zpoolCmdResult {
		return zpoolCmdResult{
			Err: &exec.Error{Name: "zpool", Err: exec.ErrNotFound},
		}
	}

	health := PoolHealth{
		Name:  "tank",
		State: "ONLINE", // Previously deemed ONLINE by procfs
	}

	err := ins.parseZpoolStatus(context.Background(), "tank", &health)
	if err == nil {
		t.Fatal("Expected error when zpool binary missing")
	}

	if health.State != "UNAVAIL" {
		t.Errorf("Expected State to be elevated to UNAVAIL from ONLINE, got %s", health.State)
	}
	if !strings.Contains(health.StatusNote, "zpool CLI binary not found") {
		t.Errorf("Expected StatusNote to mention binary not found, got: %s", health.StatusNote)
	}
}

func TestStderrTruncatedLogged(t *testing.T) {
	var logBuf bytes.Buffer
	handler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(oldLogger)
	})

	ins := New()
	ins.cmdRunner = func(ctx context.Context, poolName string) zpoolCmdResult {
		return zpoolCmdResult{
			Stdout:          "pool: tank\nstate: ONLINE\n",
			StderrTruncated: true,
		}
	}

	health := PoolHealth{
		Name:  "tank",
		State: "ONLINE",
	}

	err := ins.parseZpoolStatus(context.Background(), "tank", &health)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !strings.Contains(logBuf.String(), "zpool status stderr exceeded 1MB memory limit and was truncated") {
		t.Errorf("Expected Warn log for stderr truncation, got: %s", logBuf.String())
	}
	if !strings.Contains(health.StatusNote, "zpool stderr truncated at 1MB") {
		t.Errorf("Expected StatusNote to note stderr truncation, got: %s", health.StatusNote)
	}
}

func TestCommandHungTimeout(t *testing.T) {
	ins := New()
	ins.cmdRunner = func(ctx context.Context, poolName string) zpoolCmdResult {
		return zpoolCmdResult{
			TimedOut:            true,
			SuspectedKernelHang: true,
			Err:                 context.DeadlineExceeded,
			Stderr:              "kernel hung",
		}
	}

	health := PoolHealth{Name: "tank", State: "ONLINE"}
	err := ins.parseZpoolStatus(context.Background(), "tank", &health)
	if err == nil {
		t.Fatal("Expected error on timeout")
	}

	if health.State != "FAULTED" {
		t.Errorf("Expected State=FAULTED on hang, got %s", health.State)
	}
	if ins.HungCommands() != 1 {
		t.Errorf("Expected HungCommands=1, got %d", ins.HungCommands())
	}
	if !strings.Contains(health.StatusNote, "wedged in kernel D-state") {
		t.Errorf("Expected StatusNote to mention wedged in kernel D-state, got: %s", health.StatusNote)
	}
}

func TestCommandSoftwareTimeout(t *testing.T) {
	ins := New()
	ins.cmdRunner = func(ctx context.Context, poolName string) zpoolCmdResult {
		return zpoolCmdResult{
			TimedOut:            true,
			SuspectedKernelHang: false,
			Err:                 context.DeadlineExceeded,
		}
	}

	health := PoolHealth{Name: "tank", State: "ONLINE"}
	err := ins.parseZpoolStatus(context.Background(), "tank", &health)
	if err == nil {
		t.Fatal("Expected error on timeout")
	}

	if health.State != "UNKNOWN" {
		t.Errorf("Expected State=UNKNOWN on software timeout, got %s", health.State)
	}
	if ins.HungCommands() != 0 {
		t.Errorf("Expected HungCommands=0 on software timeout, got %d", ins.HungCommands())
	}
	if !strings.Contains(health.StatusNote, "state unverified") {
		t.Errorf("Expected StatusNote to mention state unverified, got: %s", health.StatusNote)
	}
}

func TestWaitDelayExceeded(t *testing.T) {
	ins := New()
	ins.cmdRunner = func(ctx context.Context, poolName string) zpoolCmdResult {
		return zpoolCmdResult{
			Err:    exec.ErrWaitDelay,
			Stderr: "leaked pipes",
		}
	}

	health := PoolHealth{Name: "tank", State: "ONLINE"}
	err := ins.parseZpoolStatus(context.Background(), "tank", &health)
	if err == nil {
		t.Fatal("Expected error on wait delay")
	}

	if health.State != "DEGRADED" {
		t.Errorf("Expected State=DEGRADED on leaked descriptor, got %s", health.State)
	}
	if !strings.Contains(health.StatusNote, "WaitDelay exceeded") {
		t.Errorf("Expected StatusNote to mention WaitDelay, got: %s", health.StatusNote)
	}
}
