//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
)

// metric is one measured quantity: every sample, and its summary.
type metric struct {
	Unit    string    `json:"unit"`
	Better  string    `json:"better"` // "lower" or "higher"
	Desc    string    `json:"desc,omitempty"`
	Samples []float64 `json:"samples"`
	Median  float64   `json:"median"`
	Min     float64   `json:"min"`
	Max     float64   `json:"max"`
	P95     float64   `json:"p95"`
}

type report struct {
	Format             int                `json:"format"`
	Label              string             `json:"label"`
	Commit             string             `json:"commit,omitempty"`
	Started            string             `json:"started"`
	Quick              bool               `json:"quick"`
	Host               map[string]string  `json:"host"`
	Fixture            fixtureMeta        `json:"fixture"`
	Metrics            map[string]*metric `json:"metrics"`
	BootRPC            []rpcCall          `json:"boot_rpc"`  // the last warm opening's calls, in order
	Breakdown          []phaseRow         `json:"breakdown"` // median warm and cold phase times
	BootRPCPressured   []rpcCall          `json:"boot_rpc_pressured,omitempty"`
	BreakdownPressured []phaseRow         `json:"breakdown_pressured,omitempty"`
	Notes              []string           `json:"notes,omitempty"`
}

// phaseRow is one step of opening: from the previous mark to this one.
type phaseRow struct {
	Phase  string  `json:"phase"`
	WarmMS float64 `json:"warm_ms"`
	ColdMS float64 `json:"cold_ms"`
}

func (r *report) add(name, unit, better, desc string, v ...float64) {
	if r.Metrics == nil {
		r.Metrics = map[string]*metric{}
	}
	m := r.Metrics[name]
	if m == nil {
		m = &metric{Unit: unit, Better: better, Desc: desc}
		r.Metrics[name] = m
	}
	m.Samples = append(m.Samples, v...)
	m.summarize()
}

func (m *metric) summarize() {
	if len(m.Samples) == 0 {
		return
	}
	s := append([]float64(nil), m.Samples...)
	sort.Float64s(s)
	m.Min, m.Max = s[0], s[len(s)-1]
	m.Median = quantile(s, 0.5)
	m.P95 = quantile(s, 0.95)
}

// quantile of sorted s, linear between ranks.
func quantile(s []float64, q float64) float64 {
	if len(s) == 1 {
		return s[0]
	}
	pos := q * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := min(lo+1, len(s)-1)
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func medianOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return quantile(s, 0.5)
}

func writeJSON(path string, r *report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func loadReport(path string) (*report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r report
	return &r, json.Unmarshal(b, &r)
}

func sortedNames(m map[string]*metric) []string {
	var ns []string
	for n := range m {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

func fmtNum(v float64) string {
	a := math.Abs(v)
	switch {
	case a == 0:
		return "0"
	case a >= 1000:
		return fmt.Sprintf("%.0f", v)
	case a >= 100:
		return fmt.Sprintf("%.1f", v)
	case a >= 1:
		return fmt.Sprintf("%.2f", v)
	default:
		return fmt.Sprintf("%.3f", v)
	}
}

// printTable is the readable form: every metric, then the opening's
// breakdown and its RPC sequence.
func printTable(w io.Writer, r *report) {
	fmt.Fprintf(w, "perfbench %s  (%s, %s, fixture %s: %d msgs, db %.0f MB)\n\n", r.Label, r.Commit, r.Started, r.Fixture.Name, r.Fixture.Messages, float64(r.Fixture.DBBytes)/1e6)
	fmt.Fprintf(w, "%-44s %10s %10s %10s %10s  %-6s %s\n", "metric", "median", "min", "max", "p95", "unit", "n")
	fmt.Fprintln(w, strings.Repeat("-", 100))
	for _, n := range sortedNames(r.Metrics) {
		m := r.Metrics[n]
		fmt.Fprintf(w, "%-44s %10s %10s %10s %10s  %-6s %d\n", n, fmtNum(m.Median), fmtNum(m.Min), fmtNum(m.Max), fmtNum(m.P95), m.Unit, len(m.Samples))
	}
	printBreakdown(w, "Opening, phase by phase", r.Breakdown)
	printBreakdown(w, "Opening under memory pressure, phase by phase", r.BreakdownPressured)
	printRPCs(w, "Interface boot RPCs (last warm run", r.BootRPC)
	printRPCs(w, "Interface boot RPCs under memory pressure (last warm run", r.BootRPCPressured)
	for _, n := range r.Notes {
		fmt.Fprintln(w, "note:", n)
	}
}

// printCompare is the delta of b against a, metric by metric. A change
// inside both runs' min..max ranges is marked "~" (noise).
func printCompare(w io.Writer, a, b *report) {
	fmt.Fprintf(w, "A: %s (%s)\nB: %s (%s)\n\n", a.Label, a.Commit, b.Label, b.Commit)
	fmt.Fprintf(w, "%-44s %10s %10s %10s %8s  %s\n", "metric", "A median", "B median", "delta", "delta%", "verdict")
	fmt.Fprintln(w, strings.Repeat("-", 100))
	names := map[string]bool{}
	for n := range a.Metrics {
		names[n] = true
	}
	for n := range b.Metrics {
		names[n] = true
	}
	var ns []string
	for n := range names {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	for _, n := range ns {
		ma, mb := a.Metrics[n], b.Metrics[n]
		if ma == nil || mb == nil {
			fmt.Fprintf(w, "%-44s %10s %10s\n", n, opt(ma), opt(mb))
			continue
		}
		d := mb.Median - ma.Median
		pct := 0.0
		if ma.Median != 0 {
			pct = d / math.Abs(ma.Median) * 100
		}
		verdict := "~"
		overlap := mb.Median >= ma.Min && mb.Median <= ma.Max || ma.Median >= mb.Min && ma.Median <= mb.Max
		if !overlap {
			better := d < 0
			if ma.Better == "higher" {
				better = d > 0
			}
			verdict = "worse"
			if better {
				verdict = "better"
			}
		}
		fmt.Fprintf(w, "%-44s %10s %10s %10s %7.1f%%  %s\n", n, fmtNum(ma.Median), fmtNum(mb.Median), fmtNum(d), pct, verdict)
	}
	if len(a.Breakdown) > 0 && len(b.Breakdown) > 0 {
		fmt.Fprintf(w, "\nOpening phases, warm median ms\n%-34s %10s %10s %10s\n", "phase", "A", "B", "delta")
		bb := map[string]phaseRow{}
		for _, p := range b.Breakdown {
			bb[p.Phase] = p
		}
		for _, p := range a.Breakdown {
			q := bb[p.Phase]
			fmt.Fprintf(w, "%-34s %10s %10s %10s\n", p.Phase, fmtNum(p.WarmMS), fmtNum(q.WarmMS), fmtNum(q.WarmMS-p.WarmMS))
		}
	}
}

func opt(m *metric) string {
	if m == nil {
		return "-"
	}
	return fmtNum(m.Median)
}

func printBreakdown(w io.Writer, title string, rows []phaseRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s (median ms; each row ends at its mark)\n%-44s %10s %10s\n", title, "phase", "warm", "cold")
	fmt.Fprintln(w, strings.Repeat("-", 66))
	var tw, tc float64
	for _, p := range rows {
		fmt.Fprintf(w, "%-44s %10s %10s\n", p.Phase, fmtNum(p.WarmMS), fmtNum(p.ColdMS))
		tw += p.WarmMS
		tc += p.ColdMS
	}
	fmt.Fprintf(w, "%-44s %10s %10s\n", "(sum)", fmtNum(tw), fmtNum(tc))
}

func printRPCs(w io.Writer, title string, calls []rpcCall) {
	if len(calls) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s; start and duration in ms from the socket's accept)\n%-4s %-28s %9s %9s %9s\n", title, "#", "method", "start", "dur", "resp KB")
	for i, c := range calls {
		fmt.Fprintf(w, "%-4d %-28s %9.2f %9.2f %9.1f\n", i+1, c.Method, float64(c.Start)/1e6, float64(c.Dur)/1e6, float64(c.RespB)/1024)
	}
}
