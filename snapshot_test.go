package main

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

func TestSnapshotReadDuringReplacement(t *testing.T) {
	t.Chdir(t.TempDir())
	const first = `{"updatedAt":1,"accounts":[]}`
	const second = `{"updatedAt":2,"accounts":[]}`
	if err := os.WriteFile(statusSnapshotFile, []byte(first), 0600); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	stop := make(chan struct{})
	errors := make(chan error, 4)
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := readStatusSnapshotFile()
				if err != nil {
					errors <- err
					return
				}
				if string(data) != first && string(data) != second {
					errors <- fmt.Errorf("reader observed partial snapshot: %q", data)
					return
				}
			}
		})
	}
	defer func() {
		close(stop)
		readers.Wait()
		close(errors)
		for err := range errors {
			t.Error(err)
		}
	}()
	for i := range 1000 {
		content := first
		if i%2 == 1 {
			content = second
		}
		if err := writeStatusSnapshotFile([]byte(content)); err != nil {
			t.Fatalf("atomic replacement during reads: %v", err)
		}
	}
}
