package main

import (
	"os"
	"sync"
)

// Windows cannot reliably replace a snapshot while this process is reading it.
// Keep this lock limited to file I/O; callers may already hold accountMu.
var statusSnapshotFileMu sync.RWMutex

func readStatusSnapshotFile() ([]byte, error) {
	statusSnapshotFileMu.RLock()
	defer statusSnapshotFileMu.RUnlock()
	return os.ReadFile(statusSnapshotFile)
}

func writeStatusSnapshotFile(data []byte) error {
	statusSnapshotFileMu.Lock()
	defer statusSnapshotFileMu.Unlock()
	tmp := statusSnapshotFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, statusSnapshotFile)
}
