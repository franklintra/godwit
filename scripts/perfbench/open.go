//go:build linux

package main

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"
)

// openResult is one opening: backend start to the first thread list.
type openResult struct {
	ProcessReady time.Duration // exec to the Local API accepting a ui connection
	FirstList    time.Duration // exec to the App's MailReady (accounts, settings, first inbox page loaded)
	UIBoot       time.Duration // accept to MailReady: the interface's boot RPCs
	Calls        []rpcCall     // the boot's RPCs until MailReady
	After        []rpcCall     // and those in the settle that follows
	Phases       []phase
	Mem          memSample
	GoMem        map[string]int64
	SQLWait      time.Duration // ui client's statements: time queued
	SQLExec      time.Duration // and running
	Frames       int
	FrameTime    time.Duration
}

type phase struct {
	Name string
	D    time.Duration
}

// opener runs openings on one scratch data directory.
type opener struct {
	bin, data string
	env       []string
	group     *pressureGroup // nil: no memory limit beyond the job's
	settle    time.Duration
}

// open starts a backend (after dropping the data's and binary's cached
// pages when cold) and the interface, and measures until the first list.
// keep leaves both running for the caller, which must stop them.
func (o *opener) open(ctx context.Context, cold, keep bool) (openResult, *backend, *uiDriver, error) {
	var r openResult
	if cold {
		dropCache(o.data, o.bin)
	}
	b, err := startBackend(o.bin, o.data, o.env, o.group, true)
	if err != nil {
		return r, nil, nil, err
	}
	fail := func(err error) (openResult, *backend, *uiDriver, error) {
		b.stop()
		log.Printf("backend log %s:\n%s", b.logPath, tail(b.logPath, 30))
		return r, nil, nil, err
	}
	tr := newRPCTrace()
	c, accept, err := b.dialUI(ctx, 10*time.Minute, tr.wrap, "perfbench-ui")
	if err != nil {
		return fail(err)
	}
	r.ProcessReady = accept.Sub(b.start)
	d, err := startUI(ctx, c, o.data)
	if err != nil {
		c.Close()
		return fail(err)
	}
	if err := d.until(10*time.Minute, d.a.MailReady); err != nil {
		d.close()
		c.Close()
		return fail(fmt.Errorf("first list: %w", err))
	}
	ready := time.Now()
	r.FirstList, r.UIBoot = ready.Sub(b.start), ready.Sub(accept)
	r.Frames, r.FrameTime = d.frames, d.frameDur
	r.Mem, _ = readMem(b.pid)
	r.Calls = tr.snapshot(accept)
	d.settle(o.settle)
	all := tr.snapshot(accept)
	r.After = all[len(r.Calls):]
	r.GoMem = b.goMem()
	marks := readTrace(b.trace)
	prev := b.start
	for _, m := range marks {
		r.Phases = append(r.Phases, phase{"backend: " + m.Step, m.At.Sub(prev)})
		prev = m.At
	}
	r.Phases = append(r.Phases, phase{"backend: listen to first accept", accept.Sub(prev)}, phase{"interface: boot RPCs to first list", r.UIBoot})
	time.Sleep(150 * time.Millisecond) // the SQL trace flushes every 100 ms
	for _, s := range sumByCall(readSQL(b.sqlPath), b.start, ready) {
		if s.Client == "perfbench-ui" {
			r.SQLWait += s.Wait
			r.SQLExec += s.Exec
		}
	}
	if keep {
		return r, b, d, nil
	}
	d.close()
	c.Close()
	b.stop()
	b.cleanup()
	return r, nil, nil, nil
}

// record adds one opening's numbers under prefix ("open.cold", ...).
func (r openResult) record(rep *report, prefix string) {
	rep.add(prefix+".process_ready_ms", "ms", "lower", "exec of godwit --headless to its Local API accepting a ui connection", ms(r.ProcessReady))
	rep.add(prefix+".first_list_ms", "ms", "lower", "exec to the interface's first thread list (app.App.MailReady): the opening time", ms(r.FirstList))
	rep.add(prefix+".ui_boot_ms", "ms", "lower", "accept to first list: the interface's boot RPCs and frames", ms(r.UIBoot))
	rep.add(prefix+".boot_rpcs", "calls", "lower", "Local API calls the interface made before its first list", float64(len(r.Calls)))
	kb := 0.0
	for _, c := range r.Calls {
		kb += float64(c.RespB) / 1024
	}
	rep.add(prefix+".boot_rpc_kb", "KB", "lower", "response bytes of those calls", kb)
	rep.add(prefix+".boot_sql_wait_ms", "ms", "lower", "their SQL statements' time queued for the one SQLite connection", ms(r.SQLWait))
	rep.add(prefix+".boot_sql_exec_ms", "ms", "lower", "their SQL statements' time running", ms(r.SQLExec))
	rep.add(prefix+".rss_at_first_list_mb", "MB", "lower", "backend RSS when the list shows", float64(r.Mem.RSSKB)/1024)
	if sv := slowest(r.Calls, "SavedViews"); sv > 0 {
		rep.add(prefix+".rpc_saved_views_ms", "ms", "lower", "the boot's SavedViews call (every view's unread count)", sv)
	}
	if la := slowest(r.Calls, "ListAccounts"); la > 0 {
		rep.add(prefix+".rpc_list_accounts_ms", "ms", "lower", "the boot's slowest ListAccounts call", la)
	}
}

func slowest(calls []rpcCall, method string) float64 {
	best := 0.0
	for _, c := range calls {
		if c.Method == method && c.Done {
			best = max(best, ms(c.Dur))
		}
	}
	return best
}

// runOpenings does runs pairs of cold then warm openings and records them
// under prefix; it returns the median phases and the last warm boot's
// calls.
func runOpenings(ctx context.Context, o *opener, runs int, prefix string, rep *report) (cold, warm map[string][]float64, order []string, last []rpcCall, err error) {
	cold, warm = map[string][]float64{}, map[string][]float64{}
	for i := range runs {
		for _, isCold := range []bool{true, false} {
			r, _, _, err := o.open(ctx, isCold, false)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			kind, ph := "warm", warm
			if isCold {
				kind, ph = "cold", cold
			} else {
				last = slices.Concat(r.Calls, r.After)
			}
			r.record(rep, prefix+"."+kind)
			for _, p := range r.Phases {
				if !slices.Contains(order, p.Name) {
					order = append(order, p.Name)
				}
				ph[p.Name] = append(ph[p.Name], ms(p.D))
			}
			log.Printf("%s.%s run %d: ready %.0f ms, first list %.0f ms (%d RPCs, sql wait %.0f exec %.0f ms)", prefix, kind, i+1,
				ms(r.ProcessReady), ms(r.FirstList), len(r.Calls), ms(r.SQLWait), ms(r.SQLExec))
		}
	}
	return cold, warm, order, last, nil
}

// breakdown is the phase table, in the order of the first run's phases.
func breakdown(cold, warm map[string][]float64, order []string) []phaseRow {
	var out []phaseRow
	for _, n := range order {
		out = append(out, phaseRow{Phase: n, WarmMS: medianOf(warm[n]), ColdMS: medianOf(cold[n])})
	}
	return out
}
