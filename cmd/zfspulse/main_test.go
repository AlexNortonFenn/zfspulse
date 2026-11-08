package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupFakeZpool creates a mock zpool binary in an isolated directory and adds it to PATH.
func setupFakeZpool(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()

	shScript := `#!/bin/sh
if [ "$1" = "list" ]; then
    echo "tank"
    exit 0
fi
cat << 'EOF'
  pool: tank
 state: ONLINE
config:

	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0

errors: No known data errors
EOF
exit 0
`

	batScript := `@echo off
if "%1"=="list" (
    echo tank
    exit /b 0
)
echo   pool: tank
echo  state: ONLINE
echo config:
echo 	NAME        STATE     READ WRITE CKSUM
echo 	tank        ONLINE       0     0     0
echo errors: No known data errors
exit /b 0
`

	if err := os.WriteFile(filepath.Join(tmpDir, "zpool"), []byte(shScript), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "zpool.bat"), []byte(batScript), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestInspectTargetPoolsExplicit(t *testing.T) {
	setupFakeZpool(t)
	tmpDir := t.TempDir()
	poolDir := filepath.Join(tmpDir, "tank")
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poolDir, "state"), []byte("ONLINE\n"), 0644); err != nil {
		t.Fatal(err)
	}

	results, err := inspectTargetPools(context.Background(), []string{"tank"}, tmpDir, 2*time.Second)
	if err != nil {
		t.Fatalf("Unexpected error with mock zpool: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}
	if results[0].Name != "tank" {
		t.Errorf("Expected pool tank, got %s", results[0].Name)
	}
	if results[0].State != "ONLINE" {
		t.Errorf("Expected pool state ONLINE, got %s", results[0].State)
	}
}

func TestInspectTargetPoolsWithProcfsDiscovery(t *testing.T) {
	setupFakeZpool(t)
	tmpDir := t.TempDir()
	poolDir := filepath.Join(tmpDir, "tank")
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poolDir, "state"), []byte("ONLINE\n"), 0644); err != nil {
		t.Fatal(err)
	}

	results, err := inspectTargetPools(context.Background(), nil, tmpDir, 2*time.Second)
	if err != nil {
		t.Fatalf("Unexpected error with mock zpool: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Expected 1 discovered pool, got %d", len(results))
	}
	if results[0].Name != "tank" {
		t.Errorf("Expected pool tank, got %s", results[0].Name)
	}
	if results[0].State != "ONLINE" {
		t.Errorf("Expected pool state ONLINE, got %s", results[0].State)
	}
}

func TestInspectTargetPoolsMissingZpool(t *testing.T) {
	// Point PATH to empty directory to ensure zpool binary cannot be resolved
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)

	tmpDir := t.TempDir() // empty procfs directory
	_, err := inspectTargetPools(context.Background(), nil, tmpDir, 2*time.Second)
	if err == nil {
		t.Fatal("Expected error when zpool CLI binary is missing, got nil")
	}

	var execErr *exec.Error
	if !errors.As(err, &execErr) && !strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("Expected binary missing error, got: %v", err)
	}
}

func TestInspectTargetPoolsInvalidName(t *testing.T) {
	results, _ := inspectTargetPools(context.Background(), []string{"../badpool"}, "", 2*time.Second)
	if len(results) != 1 {
		t.Fatalf("Expected 1 result for invalid pool, got %d", len(results))
	}
	if results[0].State != "UNKNOWN" {
		t.Errorf("Expected UNKNOWN state for invalid pool, got %s", results[0].State)
	}
	if !strings.Contains(results[0].StatusNote, "path traversal") {
		t.Errorf("Expected StatusNote to mention path traversal, got %q", results[0].StatusNote)
	}
}

func TestRuneTruncation(t *testing.T) {
	// Multi-byte UTF-8 characters testing rune-boundary preservation
	longNote := "Unicode test sample for verifying accurate rune truncation — Prüfungsnotiz äöü!"
	truncated := truncateNote(longNote, 50)
	if len([]rune(truncated)) != 50 {
		t.Errorf("Expected 50 runes, got %d", len([]rune(truncated)))
	}
	if !strings.HasSuffix(truncated, "...") {
		t.Errorf("Expected ellipsis suffix, got %s", truncated)
	}

	shortNote := "Short UTF-8 sample äöü"
	if truncateNote(shortNote, 50) != shortNote {
		t.Errorf("Expected short string to remain unchanged, got %s", truncateNote(shortNote, 50))
	}
}

func TestInspectTargetPoolsErrorPreserved(t *testing.T) {
	// Querying an invalid pool name must return error and preserve the pool with UNKNOWN state
	results, err := inspectTargetPools(context.Background(), []string{"bad/name"}, "", 2*time.Second)
	if err == nil {
		t.Fatal("Expected error for invalid pool name, got nil")
	}
	if len(results) != 1 {
		t.Fatalf("Expected 1 result for failed pool, got %d", len(results))
	}
	if results[0].Name != "bad/name" {
		t.Errorf("Expected pool name bad/name, got %s", results[0].Name)
	}
	if results[0].State != "UNKNOWN" {
		t.Errorf("Expected UNKNOWN state, got %s", results[0].State)
	}
	if results[0].StatusNote == "" {
		t.Errorf("Expected status note to be populated, got empty")
	}
}
