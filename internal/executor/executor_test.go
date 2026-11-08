package executor

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestSafeBufferCap(t *testing.T) {
	// Case 1: Full overflow write (1.2 MB data against 1 MB cap)
	var buf SafeBuffer
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

	if len(buf.String()) != MaxBufferCap {
		t.Errorf("Expected capped size %d, got %d", MaxBufferCap, len(buf.String()))
	}
	if !buf.Truncated() {
		t.Errorf("Expected buffer to be marked as Truncated")
	}

	// Case 2: Partial overflow write (1000 KB existing + 100 KB write -> consumes 24 KB, capped at 1024 KB)
	var buf2 SafeBuffer
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
	if len(buf2.String()) != MaxBufferCap {
		t.Errorf("Expected capped size %d, got %d", MaxBufferCap, len(buf2.String()))
	}
	if !buf2.Truncated() {
		t.Errorf("Expected buf2 to be marked as Truncated on partial overflow")
	}

	// Case 3: Empty write on exact full buffer does not trigger false truncation
	var buf3 SafeBuffer
	if _, err := buf3.Write(make([]byte, MaxBufferCap)); err != nil {
		t.Fatalf("Exact fill write failed: %v", err)
	}
	if buf3.Truncated() {
		t.Errorf("Expected buf3 NOT truncated after exact write")
	}
	n3, err3 := buf3.Write([]byte{})
	if err3 != nil || n3 != 0 {
		t.Fatalf("Empty write failed: n=%d, err=%v", n3, err3)
	}
	if buf3.Truncated() {
		t.Errorf("Empty write falsely marked buffer as Truncated")
	}
}

func TestRunCommandParentCancel(t *testing.T) {
	cmdName := "ping"
	if _, err := exec.LookPath(cmdName); err != nil {
		t.Skip("ping not found on host, skipping test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before command runs

	res := runCommand(ctx, 5*time.Second, cmdName, "127.0.0.1")

	if res.TimedOut {
		t.Errorf("Expected TimedOut=false for parent context cancellation, got true")
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Expected context.Canceled, got: %v", res.Err)
	}
}

func TestRunCommandTimeoutWrapsSentinel(t *testing.T) {
	// LookPath for a long-running command or execute go tool test
	// Using timeout that expires immediately (1 nanosecond or 10ms with sleep)
	ctx := context.Background()

	// If zpool is not installed, it will return exec.ErrNotFound fast without timeout.
	// We test with an executable that blocks long enough to hit timeout.
	cmdName := "ping"
	var args []string
	if runtime.GOOS == "windows" {
		args = []string{"127.0.0.1", "-n", "5"}
	} else {
		args = []string{"127.0.0.1", "-c", "5"}
	}

	if _, err := exec.LookPath(cmdName); err != nil {
		t.Skip("ping not found on host, skipping timeout test")
	}

	res := runCommand(ctx, 50*time.Millisecond, cmdName, args...)
	if !res.TimedOut {
		t.Errorf("Expected TimedOut=true, got false (res.Err=%v)", res.Err)
	}
	if !errors.Is(res.Err, ErrCommandTimeout) {
		t.Errorf("Expected res.Err to wrap ErrCommandTimeout, got %v", res.Err)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Errorf("Expected res.Err to wrap context.DeadlineExceeded, got %v", res.Err)
	}
}
