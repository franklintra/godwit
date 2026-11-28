//go:build linux

package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// callerOf names who ran a statement: the Local API client and method,
// or "backend" for the backend's own work (sync, rules, labels).
func callerOf(c sqlCall) string {
	if c.Method == "" {
		return "backend"
	}
	return c.Client + "/" + c.Method
}

// callerSum is one caller's statements: how long they ran on the
// connection and how long they queued for it.
type callerSum struct {
	who        string
	exec, wait time.Duration
	n          int
}

// byCaller sums the statements that started inside [from, to] per caller,
// the longest running first.
func byCaller(calls []sqlCall, from, to time.Time) []callerSum {
	by := map[string]*callerSum{}
	for _, c := range calls {
		if c.Start.Before(from) || c.Start.After(to) {
			continue
		}
		k := callerOf(c)
		s := by[k]
		if s == nil {
			s = &callerSum{who: k}
			by[k] = s
		}
		s.exec += c.Exec
		s.wait += c.Wait
		s.n++
	}
	return sortedSums(by)
}

func sortedSums(by map[string]*callerSum) []callerSum {
	out := make([]callerSum, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].exec > out[j].exec })
	return out
}

// heldWhile answers "what held the connection while these statements
// waited": for each statement sel picks (inside [from, to]), it takes
// the time it queued and sums, per caller, the other statements that ran
// in that time. calls must be sorted by end (readSQL does it); their
// running intervals do not overlap, as there is one connection.
func heldWhile(calls []sqlCall, from, to time.Time, sel func(sqlCall) bool) (waited time.Duration, held []callerSum) {
	by := map[string]*callerSum{}
	for i, c := range calls {
		if !sel(c) || c.Start.Before(from) || c.Start.After(to) || c.Wait <= 0 {
			continue
		}
		waited += c.Wait
		a, b := c.Start, c.Start.Add(c.Wait)
		j := sort.Search(len(calls), func(k int) bool { return calls[k].End.After(a) })
		for ; j < len(calls); j++ {
			if j == i {
				continue
			}
			o := calls[j]
			s := o.End.Add(-o.Exec)
			if !s.Before(b) {
				break
			}
			lo, hi := s, o.End
			if lo.Before(a) {
				lo = a
			}
			if hi.After(b) {
				hi = b
			}
			if hi.After(lo) {
				k := callerOf(o)
				h := by[k]
				if h == nil {
					h = &callerSum{who: k}
					by[k] = h
				}
				h.exec += hi.Sub(lo)
				h.n++
			}
		}
	}
	return waited, sortedSums(by)
}

// profileNotes is the window part's profile: per caller, what ran on the
// connection over the rounds, and what held it while a message open or a
// view switch waited.
func profileNotes(prefix string, calls []sqlCall, from, to time.Time, top int) []string {
	var out []string
	span := to.Sub(from)
	sums := byCaller(calls, from, to)
	line := fmt.Sprintf("%s: profile: connection time per caller over the rounds (%s):", prefix, span.Round(time.Millisecond))
	for i, s := range sums {
		if i == top {
			break
		}
		line += fmt.Sprintf(" %s %.0f ms in %d (queued %.0f ms);", s.who, ms(s.exec), s.n, ms(s.wait))
	}
	out = append(out, line)
	for _, g := range []struct {
		name    string
		methods []string
	}{
		{"message open (GetThread, ThreadLabels, GetMessageLazyImages, ImagesAllowed)", []string{"GetThread", "ThreadLabels", "GetMessageLazyImages", "ImagesAllowed"}},
		{"the rest of a mail's calls (ThreadDeadline, cards, avatar, unsubscribe...)", []string{"ThreadDeadline", "PluginCards", "ThreadTags", "SetFlags", "ContactAvatar", "UnsubscribeMethod"}},
		{"view switch (ListThreads, ViewContents)", []string{"ListThreads", "ViewContents"}},
		{"new message (NewDraft, compose Load)", []string{"NewDraft", "GetDraft", "ListAccounts", "ListIdentities", "GetSettings", "GetSignature"}},
	} {
		waited, held := heldWhile(calls, from, to, func(c sqlCall) bool {
			if c.Client != clientUser {
				return false
			}
			for _, m := range g.methods {
				if c.Method == m {
					return true
				}
			}
			return false
		})
		if waited <= 0 {
			continue
		}
		var parts []string
		for i, h := range held {
			if i == top {
				break
			}
			parts = append(parts, fmt.Sprintf("%s %.0f ms (%.0f%%)", h.who, ms(h.exec), 100*float64(h.exec)/float64(waited)))
		}
		out = append(out, fmt.Sprintf("%s: profile: %s queued %.0f ms in all; the connection was held by: %s", prefix, g.name, ms(waited), strings.Join(parts, "; ")))
	}
	return out
}
