//go:build linux

package main

import (
	"bufio"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sqlCall is one database call from the backend's SQL trace. Its kind
// names the connection that ran it and the caller's lane
// (internal/perftrace): "q@I0:i" is a query on reader I0 for an
// interactive call. Calls on one connection run one at a time, so sorted
// by end within a connection, a call runs from the later of its start
// and the connection's previous end: Wait is the time before that
// (queued, the wait for its connection included), Exec the rest. A trace
// from before the lanes (kinds q, x, t<n>) has one connection, "W".
type sqlCall struct {
	Start, End time.Time
	Kind       string
	Lane       string // the connection: W, I0, I1, B
	Class      string // the caller's lane: i, u, b ("" in an old trace)
	Client     string // the Local API client ("" for the backend's own work)
	Method     string
	Seq        string
	Stmt       string
	Wait, Exec time.Duration
}

// busyWait is a span a statement spent retrying SQLITE_BUSY on a
// connection (kind busy@<conn>), inside its call.
type busyWait struct {
	Lane string
	Dur  time.Duration
}

// laneOf is the connection and lane class of a trace kind.
func laneOf(kind string) (conn, class string) {
	_, rest, ok := strings.Cut(kind, "@")
	if !ok {
		return "W", ""
	}
	conn, class, _ = strings.Cut(rest, ":")
	return conn, class
}

func readSQL(path string) []sqlCall {
	calls, _ := readSQLBusy(path)
	return calls
}

// readSQLBusy is readSQL plus the trace's busy-handler waits.
func readSQLBusy(path string) ([]sqlCall, []busyWait) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	var out []sqlCall
	var busy []busyWait
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		p := strings.SplitN(sc.Text(), "\t", 5)
		if len(p) < 5 {
			continue
		}
		s, _ := strconv.ParseInt(p[0], 10, 64)
		e, _ := strconv.ParseInt(p[1], 10, 64)
		conn, class := laneOf(p[2])
		if strings.HasPrefix(p[2], "busy@") {
			busy = append(busy, busyWait{Lane: conn, Dur: time.Duration(e - s)})
			continue
		}
		c := sqlCall{Start: time.Unix(0, s), End: time.Unix(0, e), Kind: p[2], Lane: conn, Class: class, Stmt: p[4]}
		if tag := p[3]; tag != "" {
			client, rest, _ := strings.Cut(tag, "/")
			c.Client = client
			c.Method, c.Seq, _ = strings.Cut(rest, "#")
		}
		out = append(out, c)
	}
	splitWait(out)
	return out, busy
}

// splitWait sorts calls by end and sets each one's Wait and Exec within
// its connection.
func splitWait(calls []sqlCall) {
	sort.Slice(calls, func(i, j int) bool { return calls[i].End.Before(calls[j].End) })
	prev := map[string]time.Time{}
	for i := range calls {
		c := &calls[i]
		run := c.Start
		if p := prev[c.Lane]; p.After(run) {
			run = p
		}
		if run.After(c.End) { // clock jitter
			run = c.End
		}
		c.Wait = run.Sub(c.Start)
		c.Exec = c.End.Sub(run)
		prev[c.Lane] = c.End
	}
}

// sqlSums is wait and exec summed per Local API call (client, method, seq).
type sqlSum struct {
	Client, Method string
	Start          time.Time
	Wait, Exec     time.Duration
	N              int
}

func sumByCall(calls []sqlCall, from, to time.Time) map[string]*sqlSum {
	out := map[string]*sqlSum{}
	for _, c := range calls {
		if c.Method == "" || c.Start.Before(from) || c.Start.After(to) {
			continue
		}
		k := c.Client + "/" + c.Method + "#" + c.Seq
		s := out[k]
		if s == nil {
			s = &sqlSum{Client: c.Client, Method: c.Method, Start: c.Start}
			out[k] = s
		}
		s.Wait += c.Wait
		s.Exec += c.Exec
		s.N++
	}
	return out
}

// blockers are the backend's own calls (no client) that held the connection
// longest inside [from, to].
func blockers(calls []sqlCall, from, to time.Time, n int) []sqlCall {
	var bg []sqlCall
	for _, c := range calls {
		if c.Client == "" && c.Method == "" && c.End.After(from) && c.Start.Before(to) {
			bg = append(bg, c)
		}
	}
	sort.Slice(bg, func(i, j int) bool { return bg[i].Exec > bg[j].Exec })
	if len(bg) > n {
		bg = bg[:n]
	}
	return bg
}

// laneBusy is the share of a span one connection spent on calls with no
// client (sync, rules, labeler...), and on any call.
type laneBusy struct{ Background, All float64 }

// busyShare is laneBusy per connection over [from, to]. Each connection
// counts alone, up to 1: the writer and a reader can both be busy all
// along.
func busyShare(calls []sqlCall, from, to time.Time) map[string]laneBusy {
	span := to.Sub(from)
	out := map[string]laneBusy{}
	if span <= 0 {
		return out
	}
	for _, c := range calls {
		s, e := c.End.Add(-c.Exec), c.End
		if s.Before(from) {
			s = from
		}
		if e.After(to) {
			e = to
		}
		if e.Before(s) {
			continue
		}
		b := out[c.Lane]
		share := float64(e.Sub(s)) / float64(span)
		b.All += share
		if c.Method == "" {
			b.Background += share
		}
		out[c.Lane] = b
	}
	return out
}

// busiest is the highest background and overall share among connections
// (with one connection, that connection's).
func busiest(m map[string]laneBusy) (background, all float64) {
	for _, b := range m {
		background, all = max(background, b.Background), max(all, b.All)
	}
	return background, all
}

// lanesIn is the connections that ran calls, sorted.
func lanesIn(calls []sqlCall) []string {
	var out []string
	for _, c := range calls {
		if !slices.Contains(out, c.Lane) {
			out = append(out, c.Lane)
		}
	}
	sort.Strings(out)
	return out
}

// behindBackground answers "did an interactive statement queue behind a
// Background one": for each interactive statement (class i) on one of
// conns that started inside [from, to], the time a Background statement
// (class b) ran on its connection while it waited. n counts the
// statements that waited behind one at all.
func behindBackground(calls []sqlCall, from, to time.Time, conns ...string) (n int, d time.Duration) {
	var longest time.Duration
	for _, c := range calls {
		longest = max(longest, c.Exec)
	}
	for i, c := range calls {
		if c.Class != "i" || c.Wait <= 0 || c.Start.Before(from) || c.Start.After(to) || !slices.Contains(conns, c.Lane) {
			continue
		}
		a, b := c.Start, c.Start.Add(c.Wait)
		var held time.Duration
		j := sort.Search(len(calls), func(k int) bool { return calls[k].End.After(a) })
		for ; j < len(calls); j++ {
			o := calls[j]
			if o.End.Sub(b) > longest { // sorted by end: nothing later starts before b
				break
			}
			if j == i || o.Lane != c.Lane || o.Class != "b" {
				continue
			}
			s := o.End.Add(-o.Exec)
			lo, hi := s, o.End
			if lo.Before(a) {
				lo = a
			}
			if hi.After(b) {
				hi = b
			}
			if hi.After(lo) {
				held += hi.Sub(lo)
			}
		}
		if held > 0 {
			n++
			d += held
		}
	}
	return n, d
}

// addLaneMetrics reports, over [from, to], per connection its busy share,
// per lane class its statements' queue wait, the interactive statements
// that queued behind Background ones, and the busy-handler waits.
func addLaneMetrics(rep *report, prefix string, calls []sqlCall, busy []busyWait, from, to time.Time) {
	lanes := busyShare(calls, from, to)
	for _, l := range lanesIn(calls) {
		rep.add(prefix+".lane_busy_pct_"+l, "%", "lower", "share of the span connection "+l+" was busy (W: the writer, I0/I1: the interactive readers, B: the background reader)", lanes[l].All*100)
	}
	waits := map[string][]float64{}
	for _, c := range calls {
		if c.Class == "" || c.Start.Before(from) || c.Start.After(to) {
			continue
		}
		waits[c.Class] = append(waits[c.Class], ms(c.Wait))
	}
	for _, cl := range []struct{ k, name string }{{"i", "interactive"}, {"u", "uibackground"}, {"b", "background"}} {
		if w := waits[cl.k]; len(w) > 0 {
			rep.add(prefix+".lane_wait_ms_"+cl.name, "ms", "lower", "per "+cl.name+" statement: time queued for its connection (admission and yield included)", w...)
		}
	}
	others := 0
	for _, c := range calls {
		if (c.Lane == "I0" || c.Lane == "I1") && c.Class != "i" && !c.Start.Before(from) && !c.Start.After(to) {
			others++
		}
	}
	rep.add(prefix+".lane_noninteractive_on_i", "count", "lower", "statements of other lanes that ran on I0 or I1 (0 by design: I holds interactive statements only)", float64(others))
	n, _ := behindBackground(calls, from, to, "I0", "I1")
	rep.add(prefix+".lane_interactive_behind_background", "count", "lower", "interactive statements on I0 or I1 that queued while a Background statement ran there (0 by design)", float64(n))
	_, d := behindBackground(calls, from, to, "B")
	rep.add(prefix+".lane_borrowed_b_behind_background_ms", "ms", "lower", "interactive statements that ran on a borrowed B: time a Background statement ran there while they waited for any reader", ms(d))
	n, d = behindBackground(calls, from, to, "W")
	rep.add(prefix+".lane_writer_interactive_behind_background", "count", "lower", "interactive writes that waited for a Background transaction running on the writer", float64(n))
	rep.add(prefix+".lane_writer_interactive_behind_background_ms", "ms", "lower", "time those writes waited", ms(d))
	bw := map[string]struct {
		n int
		d time.Duration
	}{}
	for _, b := range busy {
		x := bw[b.Lane]
		x.n++
		x.d += b.Dur
		bw[b.Lane] = x
	}
	var total int
	for _, x := range bw {
		total += x.n
	}
	rep.add(prefix+".busy_waits", "count", "lower", "statements that retried SQLITE_BUSY (a lock held by another connection)", float64(total))
	for _, l := range slices.Sorted(maps.Keys(bw)) {
		rep.add(prefix+".busy_wait_ms_"+l, "ms", "lower", "time statements on "+l+" retried SQLITE_BUSY", ms(bw[l].d))
	}
}
