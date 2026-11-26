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

// sqlCall is one database call from the backend's SQL trace. Every call
// waits for the one SQLite connection, so sorted by end, a call runs from
// the later of its start and the previous call's end: Wait is the time
// before that (queued behind others), Exec the rest.
type sqlCall struct {
	Start, End time.Time
	Kind       string
	Client     string // the Local API client ("" for the backend's own work)
	Method     string
	Seq        string
	Stmt       string
	Wait, Exec time.Duration
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
		c := sqlCall{Start: time.Unix(0, s), End: time.Unix(0, e), Kind: p[2], Stmt: p[4]}
		if tag := p[3]; tag != "" {
			client, rest, _ := strings.Cut(tag, "/")
			c.Client = client
			c.Method, c.Seq, _ = strings.Cut(rest, "#")
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].End.Before(out[j].End) })
	var prev time.Time
	for i := range out {
		run := out[i].Start
		if prev.After(run) {
			run = prev
		}
		if run.After(out[i].End) { // clock jitter
			run = out[i].End
		}
		out[i].Wait = run.Sub(out[i].Start)
		out[i].Exec = out[i].End.Sub(run)
		prev = out[i].End
	}
	return out
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

// busyShare is the share of [from, to] the connection spent on calls
// with no client (sync, rules, labeler...), and on any call.
func busyShare(calls []sqlCall, from, to time.Time) (background, all float64) {
	span := to.Sub(from)
	if span <= 0 {
		return 0, 0
	}
	var bg, any time.Duration
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
		any += e.Sub(s)
		if c.Method == "" {
			bg += e.Sub(s)
		}
	}
	return float64(bg) / float64(span), float64(any) / float64(span)
}
