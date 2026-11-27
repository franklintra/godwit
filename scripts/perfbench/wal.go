//go:build linux

package main

import (
	"path/filepath"
	"sync"
	"time"
)

// walWatch samples the size of dataDir's WAL file every 200 ms until the
// returned function is called. That function gives the largest size seen
// and the size at the call, in MB. Under WAL, a reader's snapshot keeps
// the writer from restarting the log, so the file grows while long reads
// run beside a sync, and without journal_size_limit it keeps its largest
// size. Calls after the first give the first one's sizes.
func walWatch(dataDir string) func() (maxMB, endMB float64) {
	path := filepath.Join(dataDir, "godwit.db-wal")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var largest int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			largest = max(largest, fileSize(path))
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	var once sync.Once
	var maxMB, endMB float64
	return func() (float64, float64) {
		once.Do(func() {
			close(stop)
			wg.Wait()
			end := fileSize(path)
			maxMB, endMB = float64(max(largest, end))/1e6, float64(end)/1e6
		})
		return maxMB, endMB
	}
}

// addWAL reports walWatch's sizes under prefix.
func addWAL(rep *report, prefix, during string, stop func() (float64, float64)) {
	maxMB, endMB := stop()
	rep.add(prefix+".wal_max_mb", "MB", "lower", "largest godwit.db-wal "+during+" (sampled every 200 ms)", maxMB)
	rep.add(prefix+".wal_end_mb", "MB", "lower", "godwit.db-wal at the end of "+during, endMB)
}
