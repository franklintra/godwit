// Package perftrace records timings for scripts/perfbench, only when its
// environment variables ask for it; unset (always, outside a benchmark)
// every call returns after reading a package variable.
//
// GODWIT_STARTUP_TRACE names a file that gets "<step> <unix ns>" per Mark:
// when each step of a backend's start ended.
//
// GODWIT_SQL_TRACE names a file that gets one tab-separated line per
// database call: start and end (unix ns), kind, the Local API call it
// serves ("" for background work) and the statement's start. The kind is
// <call>@<conn>:<lane> (core/store's lanes, ADR 0052): call is q, x or
// t<n> (a query, an execute, a transaction of n statements), conn the
// connection that ran it (W, I0, I1, B), lane the caller's class (i
// interactive, u ui-background, b background). Start is when the call
// asked for its connection, so the wait for it is included. Calls on one
// connection run one at a time, so a reader recovers each call's wait
// from the order of the ends within its connection. Two kinds are not
// calls: busy@<conn> spans the time a statement retried SQLITE_BUSY
// inside its call, ck@W a checkpoint of the store's own. A trace from
// before ADR 0052 has bare kinds (q, x, t<n>), all on one connection.
package perftrace

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The variables that turn the traces on.
const (
	StartupEnv = "GODWIT_STARTUP_TRACE"
	SQLEnv     = "GODWIT_SQL_TRACE"
)

var (
	startupPath = os.Getenv(StartupEnv)
	sqlPath     = os.Getenv(SQLEnv)
	markMu      sync.Mutex
)

// Mark records that step has just ended. Best effort.
func Mark(step string) {
	if startupPath == "" {
		return
	}
	now := time.Now().UnixNano()
	markMu.Lock()
	defer markMu.Unlock()
	f, err := os.OpenFile(startupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s %d\n", step, now)
	f.Close()
}

// SQLOn reports whether database calls are traced.
func SQLOn() bool { return sqlPath != "" }

type rpcKey struct{}
type clientKey struct{}

var rpcSeq atomic.Int64

// WithClient tags a connection's ctx with the client name its hello gave.
// ctx itself when the SQL trace is off.
func WithClient(ctx context.Context, client string) context.Context {
	if sqlPath == "" {
		return ctx
	}
	return context.WithValue(ctx, clientKey{}, client)
}

// WithRPC tags ctx with the Local API method it serves, as
// "client/method#n", numbered so each call's statements group together.
// ctx itself when the SQL trace is off.
func WithRPC(ctx context.Context, method string) context.Context {
	if sqlPath == "" {
		return ctx
	}
	client, _ := ctx.Value(clientKey{}).(string)
	return context.WithValue(ctx, rpcKey{}, fmt.Sprintf("%s/%s#%d", client, method, rpcSeq.Add(1)))
}

var (
	sqlOnce sync.Once
	sqlMu   sync.Mutex
	sqlW    *bufio.Writer
)

func openSQL() {
	f, err := os.OpenFile(sqlPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	sqlW = bufio.NewWriterSize(f, 1<<20)
	go func() {
		for range time.Tick(100 * time.Millisecond) {
			sqlMu.Lock()
			sqlW.Flush()
			sqlMu.Unlock()
		}
	}()
}

// SQL records a database call that began at start and has just ended.
func SQL(ctx context.Context, kind, stmt string, start time.Time) {
	if sqlPath == "" {
		return
	}
	end := time.Now()
	sqlOnce.Do(openSQL)
	if sqlW == nil {
		return
	}
	tag, _ := ctx.Value(rpcKey{}).(string)
	if len(stmt) > 160 {
		stmt = stmt[:160]
	}
	stmt = strings.Join(strings.Fields(stmt), " ")
	sqlMu.Lock()
	fmt.Fprintf(sqlW, "%d\t%d\t%s\t%s\t%s\n", start.UnixNano(), end.UnixNano(), kind, tag, stmt)
	sqlMu.Unlock()
}
