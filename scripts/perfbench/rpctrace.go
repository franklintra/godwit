//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"os"
)

// rpcTrace watches a Local API connection (line-delimited JSON-RPC) and
// records each call: method, when it was written, when its answer came,
// and the answer's size. Streams ($/stream) count as bytes only.
type rpcTrace struct {
	mu      sync.Mutex
	calls   []rpcCall
	pending map[uint64]int
	inBuf   []byte
	outBuf  []byte
}

type rpcCall struct {
	Method  string        `json:"method"`
	Start   time.Duration `json:"start_ns"` // since the trace's origin
	Dur     time.Duration `json:"dur_ns"`
	ReqB    int           `json:"req_bytes"`
	RespB   int           `json:"resp_bytes"`
	Done    bool          `json:"done"`
	startAt time.Time
}

func newRPCTrace() *rpcTrace { return &rpcTrace{pending: map[uint64]int{}} }

type tracedConn struct {
	io.ReadWriteCloser
	t *rpcTrace
}

func (t *rpcTrace) wrap(c io.ReadWriteCloser) io.ReadWriteCloser { return &tracedConn{c, t} }

func (c *tracedConn) Write(p []byte) (int, error) {
	c.t.feed(&c.t.outBuf, p, true)
	return c.ReadWriteCloser.Write(p)
}

func (c *tracedConn) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	if n > 0 {
		c.t.feed(&c.t.inBuf, p[:n], false)
	}
	return n, err
}

func (t *rpcTrace) feed(buf *[]byte, p []byte, out bool) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	*buf = append(*buf, p...)
	for {
		i := bytes.IndexByte(*buf, '\n')
		if i < 0 {
			return
		}
		line := (*buf)[:i]
		var m struct {
			ID     *uint64 `json:"id"`
			Method string  `json:"method"`
		}
		_ = json.Unmarshal(line, &m)
		switch {
		case out && m.ID != nil && m.Method != "":
			t.pending[*m.ID] = len(t.calls)
			t.calls = append(t.calls, rpcCall{Method: m.Method, ReqB: len(line), startAt: now})
		case !out && m.ID != nil && m.Method == "":
			if k, ok := t.pending[*m.ID]; ok {
				c := &t.calls[k]
				c.Dur, c.RespB, c.Done = now.Sub(c.startAt), len(line), true
				delete(t.pending, *m.ID)
			}
		}
		*buf = (*buf)[i+1:]
	}
}

// snapshot returns the calls so far with Start relative to origin.
func (t *rpcTrace) snapshot(origin time.Time) []rpcCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]rpcCall, len(t.calls))
	for i, c := range t.calls {
		c.Start = c.startAt.Sub(origin)
		out[i] = c
	}
	return out
}

func (t *rpcTrace) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls, t.pending = nil, map[uint64]int{}
}

func dialUnix(path string) (net.Conn, error) { return net.Dial("unix", path) }

func fadviseDontNeed(f *os.File) { _ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED) }
