//go:build unix

package perftrace

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/metrics"
	"syscall"
)

// With the startup trace on, SIGUSR1 writes the runtime's memory metrics
// to "<startup trace>.mem", replacing the last ones: the Go heap as the
// runtime sees it, read from outside the process.
func init() {
	if startupPath == "" {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			writeMem(startupPath + ".mem")
		}
	}()
}

var memMetrics = []string{
	"/gc/heap/live:bytes",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/unused:bytes",
	"/memory/classes/heap/free:bytes",
	"/memory/classes/heap/released:bytes",
	"/memory/classes/heap/stacks:bytes",
	"/memory/classes/total:bytes",
	"/gc/cycles/total:gc-cycles",
	"/sched/goroutines:goroutines",
}

func writeMem(path string) {
	s := make([]metrics.Sample, len(memMetrics))
	for i, n := range memMetrics {
		s[i].Name = n
	}
	metrics.Read(s)
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	for _, m := range s {
		if m.Value.Kind() == metrics.KindUint64 {
			fmt.Fprintf(f, "%s %d\n", m.Name, m.Value.Uint64())
		}
	}
	f.Close()
	os.Rename(tmp, path)
}
