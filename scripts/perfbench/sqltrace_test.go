//go:build linux

package main

import (
	"strconv"
	"testing"
	"time"
)

func TestSplitWaitPerLane(t *testing.T) {
	at := func(ms int) time.Time { return time.Unix(0, 0).Add(time.Duration(ms) * time.Millisecond) }
	calls := []sqlCall{
		{Start: at(0), End: at(100), Kind: "q@r0"},  // a long reader call
		{Start: at(10), End: at(20), Kind: "q@r1"},  // beside it on another reader: no wait
		{Start: at(10), End: at(30), Kind: "x"},     // the writer: no wait either
		{Start: at(15), End: at(40), Kind: "t2"},    // queued behind the writer's x
		{Start: at(50), End: at(120), Kind: "q@r0"}, // queued behind r0's first call
	}
	for i := range calls {
		calls[i].Lane = laneOf(calls[i].Kind)
	}
	splitWait(calls)
	want := map[string][2]time.Duration{ // kind@end: wait, exec
		"q@r1@20":  {0, 10 * time.Millisecond},
		"x@30":     {0, 20 * time.Millisecond},
		"t2@40":    {15 * time.Millisecond, 10 * time.Millisecond},
		"q@r0@100": {0, 100 * time.Millisecond},
		"q@r0@120": {50 * time.Millisecond, 20 * time.Millisecond},
	}
	for _, c := range calls {
		k := c.Kind + "@" + strconv.FormatInt(c.End.Sub(at(0)).Milliseconds(), 10)
		w, ok := want[k]
		if !ok {
			t.Fatalf("unexpected call %s", k)
		}
		if c.Wait != w[0] || c.Exec != w[1] {
			t.Errorf("%s: wait %v exec %v, want %v %v", k, c.Wait, c.Exec, w[0], w[1])
		}
	}

	busy := busyShare(calls, at(0), at(100))
	if b := busy["r0"]; b.All < 0.999 || b.All > 1.001 {
		t.Errorf("r0 busy %v, want 1", b.All)
	}
	if b := busy["w"]; b.All < 0.299 || b.All > 0.301 {
		t.Errorf("writer busy %v, want 0.3", b.All)
	}
	if _, all := busiest(busy); all < 0.999 {
		t.Errorf("busiest %v, want r0's 1", all)
	}
}

func TestLaneOf(t *testing.T) {
	for kind, want := range map[string]string{"q": "w", "x": "w", "t3": "w", "q@r0": "r0", "q@r2": "r2"} {
		if got := laneOf(kind); got != want {
			t.Errorf("laneOf(%q) = %q, want %q", kind, got, want)
		}
	}
}
