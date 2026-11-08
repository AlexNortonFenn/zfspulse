package procfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPoolState(t *testing.T) {
	tmpDir := t.TempDir()
	poolDir := filepath.Join(tmpDir, "tank")
	if err := os.MkdirAll(poolDir, 0755); err != nil {
		t.Fatal(err)
	}

	stateFile := filepath.Join(poolDir, "state")
	if err := os.WriteFile(stateFile, []byte("ONLINE\n"), 0644); err != nil {
		t.Fatal(err)
	}

	st, ok := ReadPoolState(tmpDir, "tank")
	if !ok || st != "ONLINE" {
		t.Errorf("Expected ONLINE and true, got %s, %v", st, ok)
	}

	// Nonexistent pool
	_, ok = ReadPoolState(tmpDir, "nonexistent")
	if ok {
		t.Error("Expected false for nonexistent pool")
	}

	// Empty path
	_, ok = ReadPoolState("", "tank")
	if ok {
		t.Error("Expected false for empty procfs path")
	}

	// Corrupted state
	if err := os.WriteFile(stateFile, []byte("BOGUS_STATE\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, ok = ReadPoolState(tmpDir, "tank")
	if ok {
		t.Error("Expected false for invalid pool state")
	}
}

func TestDiscoverPools(t *testing.T) {
	tmpDir := t.TempDir()

	// Pool 1
	p1 := filepath.Join(tmpDir, "tank")
	if err := os.MkdirAll(p1, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p1, "state"), []byte("ONLINE\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Pool 2
	p2 := filepath.Join(tmpDir, "rpool")
	if err := os.MkdirAll(p2, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p2, "state"), []byte("ONLINE\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Non-pool dir (no state file)
	p3 := filepath.Join(tmpDir, "other")
	if err := os.MkdirAll(p3, 0755); err != nil {
		t.Fatal(err)
	}

	pools, err := DiscoverPools(tmpDir)
	if err != nil {
		t.Fatalf("DiscoverPools failed: %v", err)
	}

	if len(pools) != 2 {
		t.Fatalf("Expected 2 pools, got %d: %v", len(pools), pools)
	}
	if pools[0] != "rpool" || pools[1] != "tank" {
		t.Errorf("Expected [rpool, tank], got %v", pools)
	}
}

