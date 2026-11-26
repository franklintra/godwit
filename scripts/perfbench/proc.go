//go:build linux

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Linux /proc readings of the backend process. Memory splits by mapping:
// anonymous pages (the Go heap and stacks, and glibc malloc's heap and
// arenas, which SQLite allocates from) and file-backed pages (the binary,
// libraries). runtime/metrics (backend.goMem) says how much is Go's.

type memSample struct {
	RSSKB       int64 `json:"rss_kb"`
	HWMKB       int64 `json:"hwm_kb"`        // peak RSS since start (VmHWM)
	AnonKB      int64 `json:"anon_kb"`       // anonymous RSS: Go heap and stacks, malloc (SQLite), libc
	FileKB      int64 `json:"file_kb"`       // file-backed RSS (binary, libraries, mmap)
	AnonDirtyKB int64 `json:"anon_dirty_kb"` // private dirty anonymous: what can't be dropped
	Threads     int   `json:"threads"`
}

type cpuSample struct {
	At       time.Time
	CPUTicks int64 // utime + stime, all threads, alive or exited
	CtxSw    int64 // voluntary + involuntary, summed over live threads
}

var clkTck = int64(100) // sysconf(_SC_CLK_TCK) is 100 on every Linux we run on

func readMem(pid int) (memSample, error) {
	var s memSample
	st, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return s, err
	}
	for _, line := range strings.Split(string(st), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, _ := strconv.ParseInt(f[0], 10, 64)
		switch k {
		case "VmRSS":
			s.RSSKB = n
		case "VmHWM":
			s.HWMKB = n
		case "Threads":
			s.Threads = int(n)
		}
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/smaps", pid))
	if err != nil {
		return s, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	anon := false
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) > 0 && isHex(line[0]) && bytes.IndexByte(line, '-') > 0 && !bytes.HasSuffix(bytes.Fields(line)[0], []byte(":")) {
			fields := bytes.Fields(line)
			path := ""
			if len(fields) >= 6 {
				path = string(fields[5])
			}
			anon = path == "" || path == "[heap]" || path == "[stack]" || strings.HasPrefix(path, "[anon")
			continue
		}
		k, v, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		fv := bytes.Fields(v)
		if len(fv) == 0 {
			continue
		}
		n, _ := strconv.ParseInt(string(fv[0]), 10, 64)
		switch string(k) {
		case "Rss":
			if anon {
				s.AnonKB += n
			} else {
				s.FileKB += n
			}
		case "Private_Dirty":
			if anon {
				s.AnonDirtyKB += n
			}
		}
	}
	return s, sc.Err()
}

func isHex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' }

func readCPU(pid int) (cpuSample, error) {
	s := cpuSample{At: time.Now()}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return s, err
	}
	// fields after the ")" of comm: state is field 3; utime 14, stime 15
	i := bytes.LastIndexByte(b, ')')
	f := strings.Fields(string(b[i+2:]))
	ut, _ := strconv.ParseInt(f[11], 10, 64)
	stt, _ := strconv.ParseInt(f[12], 10, 64)
	s.CPUTicks = ut + stt
	tasks, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/status", pid))
	for _, t := range tasks {
		tb, err := os.ReadFile(t)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(tb), "\n") {
			if strings.HasPrefix(line, "voluntary_ctxt_switches:") || strings.HasPrefix(line, "nonvoluntary_ctxt_switches:") {
				_, v, _ := strings.Cut(line, ":")
				n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				s.CtxSw += n
			}
		}
	}
	return s, nil
}

func cpuMS(ticks int64) float64 { return float64(ticks) * 1000 / float64(clkTck) }

// dropCache asks the kernel to forget the cached pages of files (fsync
// first, so they are clean): the next read comes from disk, as after a
// reboot, without root. Directories are walked.
func dropCache(paths ...string) {
	for _, root := range paths {
		filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil || !fi.Mode().IsRegular() {
				return nil
			}
			f, err := os.OpenFile(p, os.O_RDONLY, 0)
			if err != nil {
				return nil
			}
			f.Sync()
			fadviseDontNeed(f)
			f.Close()
			return nil
		})
	}
}
