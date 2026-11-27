//go:build linux

package main

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sqlCall is one database call from the backend's SQL trace. The kind
// names the connection that ran it (internal/perftrace): q, x and t<n> ran
// on the writer, lane "w"; q@r<i> on read-only connection i, lane "r<i>".
// Calls on one connection run one at a time, so sorted by end within a
// lane, a call runs from the later of its start and the lane's previous
// end: Wait is the time before that, Exec the rest. A reader call's Wait
// includes its wait for a free reader, charged to the reader it got. A
// trace from before the readers has the writer lane only, so its numbers
// are those of the one connection; with readers, Wait and Exec are per
// lane and do not compare one to one with such a trace.
type sqlCall struct {
	Start, End time.Time
	Kind       string
	Lane       string // "w" for the writer, "r0", "r1"... for the readers
	Client     string // the Local API client ("" for the backend's own work)
	Method     string
	Seq        string
	Stmt       string
	Wait, Exec time.Duration
}

// laneOf is the connection a trace kind ran on.
func laneOf(kind string) string {
	if _, lane, ok := strings.Cut(kind, "@"); ok {
		return lane
	}
	return "w"
}

func readSQL(path string) []sqlCall {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []sqlCall
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		p := strings.SplitN(sc.Text(), "\t", 5)
		if len(p) < 5 {
			continue
		}
		s, _ := strconv.ParseInt(p[0], 10, 64)
		e, _ := strconv.ParseInt(p[1], 10, 64)
		c := sqlCall{Start: time.Unix(0, s), End: time.Unix(0, e), Kind: p[2], Lane: laneOf(p[2]), Stmt: p[4]}
		if tag := p[3]; tag != "" {
			client, rest, _ := strings.Cut(tag, "/")
			c.Client = client
			c.Method, c.Seq, _ = strings.Cut(rest, "#")
		}
		out = append(out, c)
	}
	splitWait(out)
	return out
}

// splitWait sorts calls by end and sets each one's Wait and Exec within
// its lane.
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

// blockers are the backend's own calls (no client) that held a
// connection longest inside [from, to]; Kind says which.
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

// busyShare is laneBusy per connection over [from, to]. Each lane counts
// alone, up to 1: the writer and a reader can both be busy all along.
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

// busiest is the highest background and overall share among lanes (with
// one connection, that connection's).
func busiest(m map[string]laneBusy) (background, all float64) {
	for _, b := range m {
		background, all = max(background, b.Background), max(all, b.All)
	}
	return background, all
}
