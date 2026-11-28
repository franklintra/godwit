//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestReadSQLLanes(t *testing.T) {
	ms := func(n int64) int64 { return 1_000_000_000_000 + n*1_000_000 }
	line := func(s, e int64, kind, tag, stmt string) string {
		return strconv.FormatInt(ms(s), 10) + "\t" + strconv.FormatInt(ms(e), 10) + "\t" + kind + "\t" + tag + "\t" + stmt + "\n"
	}
	trace := line(0, 10, "q@B:b", "", "SELECT bg") + // background on B, 0-10
		line(2, 12, "q@B:i", "godwit-ui/GetThread#1", "SELECT borrowed") + // waits 2-10 on B
		line(1, 3, "q@I0:i", "godwit-ui/GetThread#2", "SELECT quick") +
		line(0, 5, "x@W:b", "", "UPDATE sync") +
		line(1, 6, "x@W:i", "godwit-ui/SetFlags#3", "UPDATE flag") + // waits 1-5 on W
		line(4, 5, "busy@W", "", "UPDATE sync") +
		line(0, 1, "q", "", "SELECT old")
	path := filepath.Join(t.TempDir(), "sql.tsv")
	if err := os.WriteFile(path, []byte(trace), 0o600); err != nil {
		t.Fatal(err)
	}
	calls, busy := readSQLBusy(path)
	if len(calls) != 6 || len(busy) != 1 || busy[0].Lane != "W" || busy[0].Dur != time.Millisecond {
		t.Fatalf("calls %d, busy %+v", len(calls), busy)
	}
	by := map[string]sqlCall{}
	for _, c := range calls {
		by[c.Stmt] = c
	}
	if c := by["SELECT borrowed"]; c.Lane != "B" || c.Class != "i" || c.Wait != 8*time.Millisecond {
		t.Errorf("borrowed: %+v", c)
	}
	if c := by["SELECT quick"]; c.Lane != "I0" || c.Wait != 0 {
		t.Errorf("quick: %+v", c)
	}
	if c := by["SELECT old"]; c.Lane != "W" || c.Class != "" {
		t.Errorf("an old trace's kind: %+v", c)
	}
	from, to := time.Unix(0, ms(0)), time.Unix(0, ms(20))
	if n, _ := behindBackground(calls, from, to, "I0", "I1"); n != 0 {
		t.Errorf("I behind background = %d", n)
	}
	if n, d := behindBackground(calls, from, to, "B"); n != 1 || d != 8*time.Millisecond {
		t.Errorf("B behind background = %d, %s", n, d)
	}
	if n, d := behindBackground(calls, from, to, "W"); n != 1 || d != 4*time.Millisecond {
		t.Errorf("W behind background = %d, %s", n, d)
	}
	lanes := busyShare(calls, from, to)
	if b := lanes["B"]; b.All < 0.59 || b.All > 0.61 || b.Background < 0.49 || b.Background > 0.51 { // B ran 0-12 of 20 ms, 0-10 for the backend
		t.Errorf("B busy %+v", b)
	}
	if got := lanesIn(calls); len(got) != 3 || got[0] != "B" || got[1] != "I0" || got[2] != "W" {
		t.Errorf("lanes %v", got)
	}
}
