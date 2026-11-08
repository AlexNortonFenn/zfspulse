package zfspulse

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectPoolProcfs(t *testing.T) {
	var logBuf bytes.Buffer
	handler := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		slog.SetDefault(oldLogger)
	})

	tmpDir := t.TempDir()
	poolName := "testpool"
	poolDir := filepath.Join(tmpDir, poolName)
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		t.Fatalf("Failed to create mock procfs dir: %v", err)
	}

	stateFile := filepath.Join(poolDir, "state")
	if err := os.WriteFile(stateFile, []byte("DEGRADED\n"), 0644); err != nil {
		t.Fatalf("Failed to write mock state: %v", err)
	}

	ins := New(
		WithProcZfsPath(tmpDir),
		WithScrubDriftThresholdHours(168),
	)
	ins.cmdRunner = func(ctx context.Context, pool string) zpoolCmdResult {
		return zpoolCmdResult{Stdout: "", Err: nil}
	}

	ctx := context.Background()

	// 1. Valid state read
	health, _ := ins.InspectPool(ctx, poolName)
	if health.State != "DEGRADED" {
		t.Errorf("Expected State=DEGRADED from procfs kstat, got %s", health.State)
	}

	// 2. Empty state file -> Warn log
	logBuf.Reset()
	if err := os.WriteFile(stateFile, []byte("   \n"), 0644); err != nil {
		t.Fatalf("Failed to truncate mock state: %v", err)
	}
	healthEmpty, _ := ins.InspectPool(ctx, poolName)
	if !strings.Contains(logBuf.String(), "ZFS procfs kstat state file is empty") {
		t.Errorf("Expected Warn log for empty procfs state file, got: %s", logBuf.String())
	}
	if healthEmpty.Name != poolName {
		t.Errorf("Expected pool name %s, got %s", poolName, healthEmpty.Name)
	}

	// 3. Unexpected / garbage state -> Warn log
	logBuf.Reset()
	if err := os.WriteFile(stateFile, []byte("EXPLODED\n"), 0644); err != nil {
		t.Fatalf("Failed to write invalid mock state: %v", err)
	}
	healthInvalid, _ := ins.InspectPool(ctx, poolName)
	if !strings.Contains(logBuf.String(), "Unexpected ZFS pool state from procfs") {
		t.Errorf("Expected Warn log for unexpected pool state, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "\"raw_len\":9") {
		t.Errorf("Expected Warn log to contain raw_len=9, got: %s", logBuf.String())
	}
	if healthInvalid.Name != poolName {
		t.Errorf("Expected pool name %s, got %s", poolName, healthInvalid.Name)
	}

	// 4. Non-existent pool in procfs falls back cleanly with Debug log and asserts State
	logBuf.Reset()
	ins.opts.ProcZfsPath = filepath.Join(tmpDir, "nonexistent")
	healthFallback, _ := ins.InspectPool(ctx, poolName)
	if healthFallback.Name != poolName {
		t.Errorf("Expected pool name %s, got %s", poolName, healthFallback.Name)
	}
	if healthFallback.State == "ONLINE" {
		t.Errorf("State must not be ONLINE when procfs kstat missing")
	}
	if !strings.Contains(logBuf.String(), "ZFS procfs kstat state not found") {
		t.Errorf("Expected Debug log for missing procfs state, got: %s", logBuf.String())
	}
}

func TestProcfsReadRemovedState(t *testing.T) {
	tmpDir := t.TempDir()
	poolName := "removedpool"
	poolDir := filepath.Join(tmpDir, poolName)
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		t.Fatalf("Failed to create mock dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(poolDir, "state"), []byte("REMOVED\n"), 0644); err != nil {
		t.Fatalf("Failed to write mock state: %v", err)
	}

	ins := New(WithProcZfsPath(tmpDir))

	st, ok := ins.readProcfsPoolState(poolName)
	if !ok {
		t.Fatal("Expected readProcfsPoolState to succeed for REMOVED state")
	}
	if st != "REMOVED" {
		t.Errorf("Expected state REMOVED, got %s", st)
	}
}

func TestProcfsReadSuspendedState(t *testing.T) {
	tmpDir := t.TempDir()
	poolName := "suspendedpool"
	poolDir := filepath.Join(tmpDir, poolName)
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		t.Fatalf("Failed to create mock dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(poolDir, "state"), []byte("SUSPENDED\n"), 0644); err != nil {
		t.Fatalf("Failed to write mock state: %v", err)
	}

	ins := New(WithProcZfsPath(tmpDir))

	st, ok := ins.readProcfsPoolState(poolName)
	if !ok {
		t.Fatal("Expected readProcfsPoolState to succeed for SUSPENDED state")
	}
	if st != "SUSPENDED" {
		t.Errorf("Expected state SUSPENDED, got %s", st)
	}
}
