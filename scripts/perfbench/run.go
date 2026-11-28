//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

type runConfig struct {
	bin, root, out, label, commit, webDir, web      string
	db, scratch, native                             string
	scale                                           float64
	seed                                            uint64
	runs, hotRounds, viewRounds, syncMsgs, syncRuns int
	contMsgs, contBoots, pressRounds, winRounds     int
	mem                                             int64
	pressure, winViews                              string
	backendEnv                                      string
	idle, statusEvery                               time.Duration
	statusAccounts                                  int
	quick                                           bool
	only                                            map[string]bool
}

func (c runConfig) want(part string) bool { return len(c.only) == 0 || c.only[part] }

func cmdRun(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("run", flag.ExitOnError)
	var c runConfig
	fl.StringVar(&c.bin, "godwit", "", "the godwit binary to measure (built for Linux)")
	fl.StringVar(&c.root, "root", defaultRoot(), "fixture and scratch root")
	fl.StringVar(&c.out, "out", "", "write the JSON report here (default <root>/results/<label>-<time>.json)")
	fl.StringVar(&c.label, "label", "", "name of what is measured (branch)")
	fl.StringVar(&c.commit, "commit", "", "commit measured")
	fl.StringVar(&c.webDir, "web-dist", "", "web/dist of the build, for the web boot and bundle sizes (skipped when empty)")
	fl.StringVar(&c.web, "web-script", "", "scripts/perfbench/web/webboot.ts (default: in the working directory)")
	fl.Float64Var(&c.scale, "scale", 1, "fixture scale (1 = 45,000 messages)")
	fl.Uint64Var(&c.seed, "seed", 716, "fixture seed")
	quick := fl.Bool("quick", false, "fewer runs and shorter windows")
	runs := fl.Int("runs", 0, "opening runs, cold and warm each (default 5, quick 3)")
	idle := fl.Duration("idle", 0, "idle window (default 60s, quick 15s)")
	only := fl.String("only", "", "comma list of parts: open,mem,hot,idle,pressure,contention,window,sync,web (default all; contention includes window)")
	winRounds := fl.Int("window-rounds", 0, "probe rounds of the window part, while its sync writes (default 8, quick 4)")
	fl.StringVar(&c.winViews, "window-views", "franklin", "saved views the window part keeps: franklin (only the reply-based one, as his database) or fixture (all four)")
	mem := fl.String("mem", "350M", "memory.max of the pressured backend, page cache included")
	fl.StringVar(&c.pressure, "pressure", "auto", "pressured parts: auto (when PERFBENCH_CGROUP is set), on, off")
	fl.StringVar(&c.backendEnv, "backend-env", "", "extra KEY=VALUE settings for every backend, comma-separated (a diagnosis, such as MALLOC_ARENA_MAX=2)")
	fl.IntVar(&c.syncMsgs, "sync-messages", 10000, "messages in the folder the sync and contention parts sync (quick: 4000)")
	fl.StringVar(&c.db, "db", "", "measure a copy of this real database instead of the fixture (see README: run.sh -db isolates the run)")
	fl.StringVar(&c.scratch, "scratch", "", "with -db: the directory the one copy lives in (required)")
	fl.StringVar(&c.native, "native-cache", "", "with -db: mygo's native-module cache to give each backend (default: the user cache dir's mygo)")
	statusAccounts := fl.Int("status-accounts", -1, "window part: small loopback accounts whose idle sync rounds run during the probes (default 0, with -db 3)")
	fl.DurationVar(&c.statusEvery, "status-every", 5*time.Second, "window part: the time between two status rounds (SyncNow on the next status account)")
	fl.Parse(args)
	c.quick = *quick
	c.runs, c.idle, c.hotRounds, c.viewRounds, c.syncRuns, c.contMsgs, c.contBoots, c.pressRounds = 5, 60*time.Second, 20, 5, 3, 10000, 5, 3
	if c.quick {
		c.runs, c.idle, c.hotRounds, c.viewRounds, c.syncRuns, c.contMsgs, c.contBoots, c.pressRounds = 3, 15*time.Second, 8, 3, 1, 4000, 3, 2
		if c.syncMsgs == 10000 {
			c.syncMsgs = 4000
		}
	}
	c.winRounds = 8
	if c.quick {
		c.winRounds = 4
	}
	if *winRounds > 0 {
		c.winRounds = *winRounds
	}
	if !slices.Contains(windowViews, c.winViews) {
		return fmt.Errorf("-window-views: one of %v", windowViews)
	}
	if *runs > 0 {
		c.runs = *runs
	}
	if *idle > 0 {
		c.idle = *idle
	}
	var err error
	if c.mem, err = parseBytes(*mem); err != nil {
		return fmt.Errorf("-mem: %w", err)
	}
	if *only != "" {
		c.only = map[string]bool{}
		for _, p := range strings.Split(*only, ",") {
			c.only[strings.TrimSpace(p)] = true
		}
	}
	c.statusAccounts = *statusAccounts
	if c.db != "" {
		if err := realConfig(&c); err != nil {
			return err
		}
	}
	if c.statusAccounts < 0 {
		c.statusAccounts = 0
	}
	if c.bin == "" {
		return fmt.Errorf("-godwit is required")
	}
	if c.label == "" {
		c.label = "unlabeled"
	}
	switch c.pressure {
	case "auto":
		if os.Getenv("PERFBENCH_CGROUP") == "" {
			c.pressure = "off"
			log.Printf("PERFBENCH_CGROUP is unset: the pressured parts are skipped (vps-test with PERF_CGROUP=1 runs them)")
		} else {
			c.pressure = "on"
		}
	case "on", "off":
	default:
		return fmt.Errorf("-pressure: auto, on or off")
	}
	return run(ctx, c)
}

func run(ctx context.Context, c runConfig) error {
	debug.SetMemoryLimit(1536 << 20) // the harness shares vps-test's 3 GB with the backend and the IMAP server's mail
	started := time.Now()
	var (
		fxDir string
		meta  fixtureMeta
		rdb   *realDB
		err   error
	)
	if c.db != "" {
		if err := checkIsolated(); err != nil {
			return fmt.Errorf("-db: %w", err)
		}
		rdb = &realDB{src: c.db, native: c.native}
	} else if fxDir, meta, err = ensureFixture(ctx, c.root, c.scale, c.seed, c.bin); err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	rep := &report{Format: 1, Label: c.label, Commit: c.commit, Started: started.UTC().Format(time.RFC3339), Quick: c.quick, Fixture: meta, Host: hostInfo()}
	rep.Host["pressure_mem"] = fmt.Sprintf("%d MiB", c.mem>>20)
	scratch := filepath.Join(c.root, fmt.Sprintf("scratch-%d", os.Getpid()))
	if rdb != nil {
		scratch = filepath.Join(c.scratch, "copy") // c.scratch itself may be a mount point
	}
	defer os.RemoveAll(scratch)
	targets := false // the real database's targets: read once, from the first copy
	// fresh copies the fixture (or the real database: realDB.fresh) into
	// scratch (gone first): each part that writes starts from the same
	// bytes, with a fresh WAL. The copy's first open runs in g when it is
	// not nil.
	fresh := func(g *pressureGroup) (*opener, error) {
		os.RemoveAll(scratch)
		t0 := time.Now()
		if rdb != nil {
			m, err := rdb.fresh(scratch)
			if err != nil {
				return nil, fmt.Errorf("copy the database: %w", err)
			}
			if !targets {
				meta = m
			}
		} else {
			if err := copyFixture(fxDir, scratch); err != nil {
				return nil, fmt.Errorf("copy fixture: %w", err)
			}
			log.Printf("copied the fixture in %s", time.Since(t0).Round(time.Millisecond))
		}
		o := &opener{bin: c.bin, data: filepath.Join(scratch, meta.DataDir), settle: 1500 * time.Millisecond,
			env: backendEnv(scratch, filepath.Join(scratch, meta.KeyringFile), filepath.Join(scratch, meta.KeyringKey))}
		if c.backendEnv != "" {
			o.env = append(o.env, strings.Split(c.backendEnv, ",")...)
			log.Printf("backend env: %s", c.backendEnv)
		}
		// the build's first open of this copy, not an opening: what it does
		// once (a fork step, a new migration) is recorded apart, as open.first
		o.group = g
		r, _, _, err := o.open(ctx, false, false)
		o.group = nil
		if err != nil {
			return nil, fmt.Errorf("first open: %w", err)
		}
		prefix := "open.first"
		if g != nil {
			prefix = "pressured.open.first"
		}
		r.recordFirst(rep, prefix)
		if rdb != nil && !targets {
			if err := rdb.targets(ctx, filepath.Join(o.data, "godwit.db"), &meta); err != nil {
				return nil, fmt.Errorf("targets: %w", err)
			}
			rep.Fixture, targets = meta, true
		}
		return o, nil
	}
	var group *pressureGroup
	if c.pressure == "on" {
		if group, err = newPressureGroup(fmt.Sprintf("perfbench-%d", os.Getpid()), c.mem); err != nil {
			return err
		}
		defer group.close()
	}
	unpressured := c.want("open") || c.want("mem") || c.want("hot") || c.want("idle")
	if unpressured || (group != nil && c.want("pressure")) {
		var first *pressureGroup // the pressured parts alone: their copy's first open is pressured too
		if !unpressured {
			first = group
		}
		o, err := fresh(first)
		if err != nil {
			return err
		}
		if c.want("open") {
			cold, warm, order, last, err := runOpenings(ctx, o, c.runs, "open", rep)
			if err != nil {
				return err
			}
			rep.Breakdown, rep.BootRPC = breakdown(cold, warm, order), last
		}
		if c.want("mem") || c.want("hot") || c.want("idle") {
			if err := measureSession(ctx, c, o, meta, rep); err != nil {
				return err
			}
		}
		if group != nil && c.want("pressure") {
			po := *o
			po.group = group
			cold, warm, order, last, err := runOpenings(ctx, &po, c.runs, "pressured.open", rep)
			if err != nil {
				return fmt.Errorf("pressured openings: %w", err)
			}
			rep.BreakdownPressured, rep.BootRPCPressured = breakdown(cold, warm, order), last
			if err := measurePressured(ctx, c, &po, meta, rep); err != nil {
				return err
			}
		}
	}
	var mail *loopMail
	if c.want("contention") || c.want("window") || c.want("sync") {
		n := c.syncMsgs // one folder for both: the contention account syncs it too
		c.contMsgs = n
		t0 := time.Now()
		if mail, err = syncMail(n, c.seed); err != nil {
			return err
		}
		defer mail.Close()
		log.Printf("loopback IMAP folder of %d messages ready in %s", n, time.Since(t0).Round(time.Millisecond))
	}
	if c.want("contention") || c.want("window") {
		modes := []*pressureGroup{nil}
		if group != nil {
			modes = append(modes, group)
		}
		for _, g := range modes {
			prefix := "contention"
			if g != nil {
				prefix = "pressured.contention"
			}
			if c.want("contention") {
				o, err := fresh(nil)
				if err != nil {
					return err
				}
				o.group = g
				if err := runContention(ctx, o, mail, c.contMsgs, c.contBoots, prefix, rep); err != nil {
					return fmt.Errorf("%s: %w", prefix, err)
				}
			}
			// the window part on its own copy: its sync starts fresh
			o, err := fresh(nil)
			if err != nil {
				return err
			}
			o.group = g
			wo := windowOpts{rounds: c.winRounds, views: c.winViews, statusAccounts: c.statusAccounts, statusEvery: c.statusEvery, seed: c.seed}
			if rdb != nil { // the SQL trace (statement starts, no values) stays, for a later profile
				wo.keepSQL = filepath.Join(c.root, "results", fmt.Sprintf("%s-%s-%s-sql.tsv", slugify(c.label), started.UTC().Format("20060102-150405"), slugify(prefix)))
			}
			if err := runWindow(ctx, o, mail, meta, wo, prefix+".window", rep); err != nil {
				return fmt.Errorf("%s.window: %w", prefix, err)
			}
		}
	}
	os.RemoveAll(scratch)
	if c.want("sync") {
		os.MkdirAll(scratch, 0o700)
		if err := runSyncBench(ctx, c.bin, scratch, mail, c.syncRuns, rep); err != nil {
			return fmt.Errorf("sync: %w", err)
		}
	}
	if c.want("web") && c.webDir != "" {
		if err := runWeb(ctx, c, rep); err != nil {
			rep.Notes = append(rep.Notes, "web: "+err.Error())
			log.Printf("web: %v", err)
		}
	}
	rep.Notes = append(rep.Notes, fmt.Sprintf("total wall time %s", time.Since(started).Round(time.Second)))
	if c.out == "" {
		os.MkdirAll(filepath.Join(c.root, "results"), 0o755)
		c.out = filepath.Join(c.root, "results", fmt.Sprintf("%s-%s.json", slugify(c.label), started.UTC().Format("20060102-150405")))
	}
	if err := writeJSON(c.out, rep); err != nil {
		return err
	}
	printTable(os.Stdout, rep)
	fmt.Printf("\nJSON: %s\n", c.out)
	return nil
}

// copyFixture copies the fixture to dst, so each run starts from the same
// bytes with a fresh WAL, then drops the copy from the page cache: the
// copy's pages must not count as the backend's cache.
func copyFixture(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	out, err := exec.Command("cp", "-a", "--reflink=auto", src+"/.", dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	os.Remove(filepath.Join(dst, "meta.json"))
	dropCache(dst)
	return nil
}

// measureSession is one long life of a warm backend with the interface
// attached: memory after start, after the main views and hot paths, and
// over an idle window cut in five slices.
func measureSession(ctx context.Context, c runConfig, o *opener, meta fixtureMeta, rep *report) error {
	r, b, d, err := o.open(ctx, false, true)
	if err != nil {
		return err
	}
	defer func() { d.close(); b.stop(); b.cleanup() }()
	wal := walWatch(o.data)
	defer wal()
	addMem(rep, "first_list", "when the first list shows", r.Mem, r.GoMem)
	hc, _, err := b.dialUI(ctx, time.Minute, nil, "perfbench-hot") // the hot paths' own connection: the App's stays idle
	if err != nil {
		return err
	}
	defer hc.Close()
	if m, err := hc.GetMessageLazyImages(ctx, meta.BigMessage, false); err == nil && !meta.Real { // the web part's reader body: never a real one
		saveReaderBody(m.ID, m.HTML, m.Text)
	}
	if c.want("hot") || c.want("mem") {
		rounds := 0
		// the saved views take seconds each on a big mailbox: fewer rounds
		if err := runHotWith(ctx, hc, viewOps(meta), c.viewRounds, false, "hot", rep, nil); err != nil {
			return fmt.Errorf("views: %w", err)
		}
		err := runHot(ctx, hc, hotOps(meta), c.hotRounds, "hot", rep, func() {
			rounds++
			if rounds == 1 {
				m, _ := readMem(b.pid)
				addMem(rep, "after_views", "after the views and one round of every hot path (lists, the big thread, messages, searches)", m, b.goMem())
			}
		})
		if err != nil {
			return fmt.Errorf("hot paths: %w", err)
		}
	}
	if c.want("idle") || c.want("mem") {
		slice := c.idle / 5
		scale := float64(time.Minute) / float64(slice)
		prev, _ := readCPU(b.pid)
		for range 5 {
			end := time.Now().Add(slice)
			for time.Now().Before(end) { // the App's UI thread, lightly: as a window in the background
				d.drain()
				if d.tt.FramePending() {
					d.frame()
				}
				time.Sleep(50 * time.Millisecond)
			}
			cur, _ := readCPU(b.pid)
			rep.add("idle.cpu_ms_per_min", "ms", "lower", fmt.Sprintf("backend CPU per minute while idle (%s slices; accounts offline: connection refused)", slice), cpuMS(cur.CPUTicks-prev.CPUTicks)*scale)
			rep.add("idle.ctxsw_per_min", "count", "lower", "backend context switches per minute while idle (sum over live threads): wakeups", float64(cur.CtxSw-prev.CtxSw)*scale)
			prev = cur
		}
		m, _ := readMem(b.pid)
		addMem(rep, "idle_end", "after the idle window", m, b.goMem())
	}
	addWAL(rep, "session", "the session (views, hot paths, idle)", wal)
	m, _ := readMem(b.pid)
	rep.add("mem.session.peak_rss_mb", "MB", "lower", "backend VmHWM over the session (start, views, hot paths, idle)", float64(m.HWMKB)/1024)
	g := readGC(b.logPath)
	rep.add("gc.session.max_live_mb", "MB", "lower", "largest live heap after a GC (gctrace)", float64(g.MaxLiveMB))
	rep.add("gc.session.max_goal_mb", "MB", "lower", "largest heap goal (gctrace)", float64(g.MaxGoalMB))
	rep.add("gc.session.cycles", "count", "lower", "GC cycles over the session", float64(g.Cycles))
	rep.add("mem.session.threads", "count", "lower", "OS threads at the end", float64(m.Threads))
	return nil
}

// measurePressured times the calibration targets (accounts, counts, saved
// views) on a backend in the pressure group, after its opening.
func measurePressured(ctx context.Context, c runConfig, o *opener, meta fixtureMeta, rep *report) error {
	_, b, d, err := o.open(ctx, true, true)
	if err != nil {
		return err
	}
	defer func() { d.close(); b.stop(); b.cleanup() }()
	hc, _, err := b.dialUI(ctx, time.Minute, nil, "perfbench-hot")
	if err != nil {
		return err
	}
	defer hc.Close()
	ops := viewOps(meta)
	ops = append(ops, hotOps(meta)[0]) // the unified inbox's first page
	if err := runHotWith(ctx, hc, ops, c.pressRounds, false, "pressured.hot", rep, nil); err != nil {
		return err
	}
	st := o.group.stat()
	rep.add("pressured.cgroup_peak_mb", "MB", "lower", "the pressure group's memory.peak (backend anon + its page cache)", float64(st.Peak)/(1<<20))
	rep.add("pressured.cgroup_file_mb", "MB", "lower", "page cache held at the end (memory.stat file)", float64(st.File)/(1<<20))
	rep.add("pressured.cgroup_anon_mb", "MB", "lower", "anonymous memory at the end (memory.stat anon)", float64(st.Anon)/(1<<20))
	if st.OOMKills > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf("pressure: %d OOM kills in the group: the limit is below the backend's working set", st.OOMKills))
	}
	return nil
}

func addMem(rep *report, at, desc string, m memSample, gm map[string]int64) {
	rep.add("mem."+at+".rss_mb", "MB", "lower", "backend RSS, "+desc, float64(m.RSSKB)/1024)
	rep.add("mem."+at+".anon_mb", "MB", "lower", "anonymous RSS (Go heap and stacks, malloc: SQLite's page cache), "+desc, float64(m.AnonKB)/1024)
	rep.add("mem."+at+".file_mb", "MB", "lower", "file-backed RSS (binary, libraries), "+desc, float64(m.FileKB)/1024)
	if v, ok := gm["/gc/heap/live:bytes"]; ok {
		rep.add("mem."+at+".go_live_mb", "MB", "lower", "Go heap live after the last GC (runtime/metrics), "+desc, float64(v)/(1<<20))
		rep.add("mem."+at+".go_total_mb", "MB", "lower", "all memory the Go runtime mapped (runtime/metrics), "+desc, float64(gm["/memory/classes/total:bytes"])/(1<<20))
		rep.add("mem."+at+".go_in_use_mb", "MB", "lower", "Go memory in use: heap objects, unused heap spans and stacks, not released (runtime/metrics), "+desc,
			float64(gm["/memory/classes/heap/objects:bytes"]+gm["/memory/classes/heap/unused:bytes"]+gm["/memory/classes/heap/stacks:bytes"])/(1<<20))
		rep.add("mem."+at+".goroutines", "count", "lower", "goroutines, "+desc, float64(gm["/sched/goroutines:goroutines"]))
	}
}

func hostInfo() map[string]string {
	h := map[string]string{"goos": runtime.GOOS, "goarch": runtime.GOARCH, "cpus": fmt.Sprint(runtime.NumCPU()), "go": runtime.Version()}
	if n, err := os.Hostname(); err == nil {
		h["host"] = n
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "model name") {
				_, v, _ := strings.Cut(l, ":")
				h["cpu"] = strings.TrimSpace(v)
				break
			}
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		h["mem"] = strings.Fields(strings.SplitN(string(b), "\n", 2)[0])[1] + " kB"
	}
	return h
}

func defaultRoot() string {
	if r := os.Getenv("PERFBENCH_ROOT"); r != "" {
		return r
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "godwit-bench")
}
