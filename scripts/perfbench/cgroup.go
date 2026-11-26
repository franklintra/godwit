//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// pressureGroup is a cgroup v2 leaf with a small memory.max, page cache
// included, under the delegated PERFBENCH_CGROUP (vps-test PERF_CGROUP=1):
// a backend started in it cannot keep the database cached, as on a Mac
// whose memory is full.
type pressureGroup struct {
	dir string
	max int64
}

func newPressureGroup(name string, maxBytes int64) (*pressureGroup, error) {
	root := os.Getenv("PERFBENCH_CGROUP")
	if root == "" {
		return nil, fmt.Errorf("PERFBENCH_CGROUP is unset: run through vps-test with PERF_CGROUP=1")
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+memory"), 0); err != nil {
		return nil, fmt.Errorf("enable memory controller: %w", err)
	}
	dir := filepath.Join(root, name)
	os.Remove(dir)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, err
	}
	g := &pressureGroup{dir: dir, max: maxBytes}
	if err := g.write("memory.max", strconv.FormatInt(maxBytes, 10)); err != nil {
		return nil, err
	}
	g.write("memory.swap.max", "0") // the VPS's swap is full anyway; on the Mac it is too
	return g, nil
}

func (g *pressureGroup) write(file, v string) error {
	return os.WriteFile(filepath.Join(g.dir, file), []byte(v), 0)
}

// wrap returns argv that enters the group, then execs argv.
func (g *pressureGroup) wrap(argv []string) []string {
	return append([]string{"/bin/sh", "-c", `echo $$ > "$0/cgroup.procs" && exec "$@"`, g.dir}, argv...)
}

// stat is memory.current, peak, and the file/anon split, in bytes, and
// the OOM kills so far.
type cgStat struct {
	Current, Peak, File, Anon int64
	OOMKills                  int64
}

func (g *pressureGroup) stat() cgStat {
	var s cgStat
	read := func(f string) string {
		b, _ := os.ReadFile(filepath.Join(g.dir, f))
		return strings.TrimSpace(string(b))
	}
	s.Current, _ = strconv.ParseInt(read("memory.current"), 10, 64)
	s.Peak, _ = strconv.ParseInt(read("memory.peak"), 10, 64)
	for _, line := range strings.Split(read("memory.stat"), "\n") {
		k, v, _ := strings.Cut(line, " ")
		n, _ := strconv.ParseInt(v, 10, 64)
		switch k {
		case "file":
			s.File = n
		case "anon":
			s.Anon = n
		}
	}
	for _, line := range strings.Split(read("memory.events"), "\n") {
		if k, v, _ := strings.Cut(line, " "); k == "oom_kill" {
			s.OOMKills, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return s
}

func (g *pressureGroup) close() { os.Remove(g.dir) }

func parseBytes(s string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n * mult, err
}
