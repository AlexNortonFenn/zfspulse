package procfs

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ValidPoolStates defines the whitelist of known OpenZFS pool health states.
var ValidPoolStates = map[string]struct{}{
	"ONLINE":    {},
	"DEGRADED":  {},
	"FAULTED":   {},
	"OFFLINE":   {},
	"UNAVAIL":   {},
	"REMOVED":   {},
	"SUSPENDED": {},
}

// ReadPoolState reads and validates the pool state from Linux procfs kstat.
// Returns normalized pool state and true if successfully read and validated, or ("", false) otherwise.
func ReadPoolState(basePath, poolName string) (string, bool) {
	if basePath == "" {
		return "", false
	}
	path := filepath.Join(basePath, poolName, "state")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Debug("ZFS procfs kstat state not found", "pool", poolName, "path", path)
			return "", false
		}
		slog.Warn("Failed to read ZFS procfs kstat state",
			"pool", poolName,
			"path", path,
			"error", err,
			"permission_denied", errors.Is(err, os.ErrPermission),
		)
		return "", false
	}

	st := strings.ToUpper(strings.TrimSpace(string(data)))
	if st == "" {
		slog.Warn("ZFS procfs kstat state file is empty", "pool", poolName, "path", path)
		return "", false
	}

	if _, ok := ValidPoolStates[st]; !ok {
		slog.Warn("Unexpected ZFS pool state from procfs",
			"pool", poolName,
			"path", path,
			"value", st,
			"raw_len", len(data),
		)
		return "", false
	}

	return st, true
}

// DiscoverPools attempts to discover available ZFS pool names from Linux procfs kstat directory.
func DiscoverPools(basePath string) ([]string, error) {
	if basePath == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return nil, err
	}
	var pools []string
	for _, entry := range entries {
		if entry.IsDir() {
			name := entry.Name()
			statePath := filepath.Join(basePath, name, "state")
			if fi, err := os.Stat(statePath); err == nil && !fi.IsDir() {
				pools = append(pools, name)
			}
		}
	}
	sort.Strings(pools)
	return pools, nil
}

