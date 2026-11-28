//go:build linux

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/godwit-email/godwit/rpc"
	"github.com/godwit-email/godwit/rpcbind"
	"github.com/godwit-email/godwit/uiclient"
)

// backend is one `godwit --headless` process on a scratch data directory,
// started as the interface starts it (--ui-started), with the startup
// trace and the GC trace on.
type backend struct {
	cmd     *exec.Cmd
	sqlPath string // GODWIT_SQL_TRACE's file; "" when off
	pid     int
	dataDir string
	trace   string // GODWIT_STARTUP_TRACE's file
	logPath string // stderr: the backend's log and gctrace lines
	start   time.Time
	exited  chan error
}

// backendEnv is the environment every backend and the generator run in:
// credentials from the fixture's file keyring, no desktop, the network
// privacy switches off by default, and a HOME of its own.
func backendEnv(scratch, keyring, keyFile string) []string {
	home := filepath.Join(scratch, "home")
	os.MkdirAll(home, 0o700)
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "DISPLAY", "WAYLAND_DISPLAY", "GODEBUG", "GOMAXPROCS", "GOGC", "GOMEMLIMIT":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+home, "GODWIT_HEADLESS_DESKTOP=0", "GODWIT_OFFLINE_DEFAULTS=1",
		"GODWIT_KEYRING_FILE="+keyring, "GODWIT_KEYRING_KEY_FILE="+keyFile, "GODEBUG=gctrace=1")
}

// startBackend starts bin on dataDir, inside g when g is not nil, with the
// SQL trace on when sqlTrace.
func startBackend(bin, dataDir string, env []string, g *pressureGroup, sqlTrace bool) (*backend, error) {
	stamp := time.Now().UnixNano()
	dir := filepath.Dir(dataDir)
	b := &backend{dataDir: dataDir, trace: filepath.Join(dir, fmt.Sprintf("trace-%d.log", stamp)), logPath: filepath.Join(dir, fmt.Sprintf("backend-%d.log", stamp))}
	extraEnv := []string{}
	if sqlTrace {
		b.sqlPath = filepath.Join(dir, fmt.Sprintf("sql-%d.log", stamp))
		extraEnv = append(extraEnv, "GODWIT_SQL_TRACE="+b.sqlPath)
	}
	logf, err := os.Create(b.logPath)
	if err != nil {
		return nil, err
	}
	argv := []string{bin, "--headless", "--data-dir", dataDir, "--ui-started"}
	if g != nil {
		argv = g.wrap(argv)
	}
	b.cmd = exec.Command(argv[0], argv[1:]...)
	b.cmd.Env = append(append(slices.Clone(env), "GODWIT_STARTUP_TRACE="+b.trace), extraEnv...)
	b.cmd.Stdout, b.cmd.Stderr = logf, logf
	b.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	b.start = time.Now()
	if err := b.cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	b.pid = b.cmd.Process.Pid
	b.exited = make(chan error, 1)
	go func() { b.exited <- b.cmd.Wait(); logf.Close() }()
	return b, nil
}

// dialUI polls for the Local API to accept a ui connection, as the
// interface's connectBackend does (every 50 ms there, 2 ms here), and
// returns the connection and when the socket first accepted.
func (b *backend) dialUI(ctx context.Context, timeout time.Duration, wrap func(io.ReadWriteCloser) io.ReadWriteCloser, client string) (*uiclient.Client, time.Time, error) {
	deadline := time.Now().Add(timeout)
	for {
		c, err := dialRole(b.dataDir, rpc.RoleUI, wrap, client)
		if err == nil {
			return uiclient.New(c), time.Now(), nil
		}
		select {
		case err := <-b.exited:
			b.exited <- err
			return nil, time.Time{}, fmt.Errorf("backend exited before it answered (%v); log %s:\n%s", err, b.logPath, tail(b.logPath, 20))
		default:
		}
		if time.Now().After(deadline) {
			return nil, time.Time{}, fmt.Errorf("backend did not answer in %s: %v; log %s", timeout, err, b.logPath)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// dialRole is rpc.Dial with the connection wrapped (the RPC trace).
func dialRole(dataDir, role string, wrap func(io.ReadWriteCloser) io.ReadWriteCloser, client string) (*rpc.Client, error) {
	tokenFile := rpc.TokenName
	if role == rpc.RoleUI {
		tokenFile = rpc.UITokenName
	}
	tok, err := os.ReadFile(filepath.Join(dataDir, tokenFile))
	if err != nil {
		return nil, err
	}
	conn, err := dialUnix(filepath.Join(dataDir, rpc.SocketName))
	if err != nil {
		return nil, err
	}
	var rw io.ReadWriteCloser = conn
	if wrap != nil {
		rw = wrap(conn)
	}
	c := rpc.NewClient(rw)
	c.DecodeError = rpcbind.NewCodec().Decode
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var hello rpc.Hello
	if err := c.Call(ctx, "hello", &hello, string(tok), role, client); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// stop sends SIGINT, as the interface's quit, and waits; SIGKILL after 20 s.
func (b *backend) stop() (time.Duration, error) {
	t0 := time.Now()
	b.cmd.Process.Signal(os.Interrupt)
	select {
	case err := <-b.exited:
		b.exited <- err
		return time.Since(t0), nil
	case <-time.After(20 * time.Second):
		syscall.Kill(-b.pid, syscall.SIGKILL)
		err := <-b.exited
		b.exited <- err
		return time.Since(t0), errors.New("backend did not stop in 20 s; killed")
	}
}

type traceMark struct {
	Step string
	At   time.Time
}

// readTrace is the startup trace's marks, in order.
func readTrace(path string) []traceMark {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []traceMark
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		step, ns, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(ns, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, traceMark{step, time.Unix(0, n)})
	}
	return out
}

var gcLine = regexp.MustCompile(`^gc (\d+) @[0-9.]+s [0-9]+%: .* (\d+)->(\d+)->(\d+) MB, (\d+) MB goal`)

// gcStats is the GC trace so far: how many cycles, and the heap live after
// the last one (MB, as gctrace rounds it), and the largest goal.
type gcStats struct {
	Cycles    int `json:"cycles"`
	LiveMB    int `json:"live_mb"`
	MaxLiveMB int `json:"max_live_mb"`
	MaxGoalMB int `json:"max_goal_mb"`
	MaxHeapMB int `json:"max_heap_mb"` // heap at a GC's start: the peak the GC saw
}

func readGC(path string) gcStats {
	var s gcStats
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		m := gcLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		start, _ := strconv.Atoi(m[2])
		live, _ := strconv.Atoi(m[4])
		goal, _ := strconv.Atoi(m[5])
		s.Cycles = n
		s.LiveMB = live
		s.MaxLiveMB = max(s.MaxLiveMB, live)
		s.MaxGoalMB = max(s.MaxGoalMB, goal)
		s.MaxHeapMB = max(s.MaxHeapMB, start)
	}
	return s
}

func tail(path string, n int) string {
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var keep []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "gc ") {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return redact(strings.Join(keep, "\n"))
}

// openOnce starts the build on the fixture's own data dir and stops it once
// it answers: what a first open of this build changes is then done.
func openOnce(ctx context.Context, bin, fixtureDir string, m fixtureMeta) error {
	env := backendEnv(fixtureDir, filepath.Join(fixtureDir, m.KeyringFile), filepath.Join(fixtureDir, m.KeyringKey))
	b, err := startBackend(bin, filepath.Join(fixtureDir, m.DataDir), env, nil, false)
	if err != nil {
		return err
	}
	c, _, err := b.dialUI(ctx, 5*time.Minute, nil, "perfbench-open")
	if err != nil {
		b.stop()
		return err
	}
	c.Close()
	_, err = b.stop()
	os.RemoveAll(filepath.Join(fixtureDir, "home"))
	os.Remove(b.trace)
	os.Remove(b.logPath)
	return err
}

// goMem asks the backend for its runtime/metrics (SIGUSR1, perftrace) and
// returns them by name; empty if it did not answer within a second.
func (b *backend) goMem() map[string]int64 {
	path := b.trace + ".mem"
	os.Remove(path)
	b.cmd.Process.Signal(syscall.SIGUSR1)
	out := map[string]int64{}
	for range 200 {
		time.Sleep(5 * time.Millisecond)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			k, v, ok := strings.Cut(line, " ")
			if ok {
				out[k], _ = strconv.ParseInt(v, 10, 64)
			}
		}
		return out
	}
	return out
}

// cleanup removes the backend's log files.
func (b *backend) cleanup() {
	os.Remove(b.trace)
	os.Remove(b.trace + ".mem")
	os.Remove(b.logPath)
	if b.sqlPath != "" {
		os.Remove(b.sqlPath)
	}
}
