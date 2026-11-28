package store

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/godwit-email/godwit/internal/perftrace"
)

// One writer and lanes of readers (ADR 0052).
//
// mygo's sqlite.DB runs one operation at a time, so on one connection a
// click waited behind every statement queued before it: a sync batch, a
// badge recount, a sender scan. The database runs in WAL mode, where
// read-only connections beside the writer read every committed state
// without waiting for it. So DB owns four connections:
//
//	W      read-write  every Execute, Transaction, PRAGMA, WITH, RETURNING
//	I0, I1 read-only   plain SELECTs of calls the user waits on (Interactive)
//	B      read-only   every other plain SELECT (UIBackground, Background)
//
// The lane of a statement comes from its caller, never from how long it
// runs: WithLane marks a context, rpcbind marks each Local API call by
// its role and method, and a context with no mark is Background (sync,
// rules, labeler, scanner, every goroutine the core starts).
//
// Each connection has our own admission in front of mygo's gate (sched),
// so mygo never queues. I is FIFO and borrows B when B is idle. B and W
// serve their waiters by lane (Interactive > UIBackground > Background).
// A waiter leaves its queue when its ctx ends. Before a Background
// statement starts on B or W it waits while an Interactive call is in
// flight, at most backgroundYield.
//
// mygo has no interactive transaction (Transaction is one batch under
// BEGIN IMMEDIATE, Query refuses BEGIN), so every write has committed when
// its call returns, and a statement started after it on any connection
// sees it. Checkpoints are coordinated in checkpoint.go.

// Lane is the priority class of a database call.
type Lane int8

// The lanes, lowest first.
const (
	// Background is the backend's own work and the full and mcp clients.
	// A context with no mark is Background.
	Background Lane = iota
	// UIBackground is work for Godwit's interface that the user does not
	// wait on now: badges, avatars, saved-view counts.
	UIBackground
	// Interactive is a call of the interface that the user waits on now:
	// open a mail, switch a view, compose.
	Interactive
	// laneHold is the checkpoint coordinator's: it comes before every
	// caller in a queue.
	laneHold
)

// String is the lane's name, as a lane hint carries it (ParseLane).
func (l Lane) String() string {
	switch l {
	case Background:
		return "background"
	case UIBackground:
		return "ui-background"
	case Interactive:
		return "interactive"
	}
	return "hold"
}

// letter is the lane in the SQL trace's kind.
func (l Lane) letter() string {
	switch l {
	case Background:
		return "b"
	case UIBackground:
		return "u"
	case Interactive:
		return "i"
	}
	return "h"
}

// ParseLane is the lane named s (Lane.String); ok is false for any other
// text.
func ParseLane(s string) (Lane, bool) {
	for _, l := range []Lane{Background, UIBackground, Interactive} {
		if s == l.String() {
			return l, true
		}
	}
	return Background, false
}

type laneKey struct{}

// laneMark is WithLane's value. It holds the marked context's Done
// channel: work detached from the call with context.WithoutCancel keeps
// the lane while the call runs and is Background once it has ended.
type laneMark struct {
	lane   Lane
	method string
	done   <-chan struct{}
}

// WithLane marks ctx's database calls with lane until ctx ends; method
// names the call in the slow-statement log. An Interactive mark on a
// context that can end counts as an interactive call in flight until
// then: Background statements yield to it (backgroundYield).
func WithLane(ctx context.Context, lane Lane, method string) context.Context {
	if lane < Background || lane > Interactive {
		lane = Background
	}
	m := laneMark{lane: lane, method: method, done: ctx.Done()}
	if lane == Interactive && m.done != nil {
		interactiveStart()
		context.AfterFunc(ctx, interactiveEnd)
	}
	return context.WithValue(ctx, laneKey{}, m)
}

// LaneOf is ctx's lane and the method that marked it: Background with no
// mark, or once the marked context has ended.
func LaneOf(ctx context.Context) (Lane, string) {
	m, ok := ctx.Value(laneKey{}).(laneMark)
	if !ok {
		return Background, ""
	}
	if m.done != nil {
		select {
		case <-m.done:
			return Background, m.method
		default:
		}
	}
	return m.lane, m.method
}

// Interactive calls in flight, for backgroundYield. Process-wide: every
// DB in a process shares the disk and the CPU the user waits on.
var uiCalls struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n drops to 0
}

func interactiveStart() {
	uiCalls.mu.Lock()
	if uiCalls.n == 0 {
		uiCalls.idle = make(chan struct{})
	}
	uiCalls.n++
	uiCalls.mu.Unlock()
}

func interactiveEnd() {
	uiCalls.mu.Lock()
	uiCalls.n--
	if uiCalls.n == 0 {
		close(uiCalls.idle)
	}
	uiCalls.mu.Unlock()
}

// backgroundYield bounds how long a Background statement waits for the
// interactive calls in flight before it starts: a burst of clicks slows
// the sync by at most this much per statement, and with no click in
// flight it does not wait at all.
const backgroundYield = 50 * time.Millisecond

// yieldToUI waits while an interactive call is in flight, at most
// backgroundYield.
func yieldToUI(ctx context.Context) {
	uiCalls.mu.Lock()
	if uiCalls.n == 0 {
		uiCalls.mu.Unlock()
		return
	}
	idle := uiCalls.idle
	uiCalls.mu.Unlock()
	t := time.NewTimer(backgroundYield)
	defer t.Stop()
	select {
	case <-idle:
	case <-t.C:
	case <-ctx.Done():
	}
}

// readOnlySQL reports whether sql can run on a read-only connection: its
// first keyword, after blanks and comments, is SELECT, and it calls none
// of the functions that read the writer's connection state. WITH stays on
// the writer, since a write can follow it, and so does anything
// unrecognized (PRAGMA, RETURNING after INSERT, ...); a write that still
// reaches a reader fails there with SQLITE_READONLY.
func readOnlySQL(sql string) bool {
	s := skipBlank(sql)
	if len(s) < 6 || !strings.EqualFold(s[:6], "select") || (len(s) > 6 && identByte(s[6])) {
		return false
	}
	low := strings.ToLower(s)
	for _, fn := range []string{"last_insert_rowid", "changes", "total_changes"} {
		if callsFunc(low, fn) {
			return false
		}
	}
	return true
}

// setsJournalMode reports whether sql is a PRAGMA that changes the journal
// mode: the readers must close first (a WAL database leaves WAL only with
// no other connection open), and reads stay on the writer after it.
func setsJournalMode(sql string) bool {
	s := skipBlank(sql)
	if len(s) < 6 || !strings.EqualFold(s[:6], "pragma") {
		return false
	}
	low := strings.ToLower(s)
	i := strings.Index(low, "journal_mode")
	return i >= 0 && strings.ContainsAny(low[i:], "=(")
}

// skipBlank drops leading white space and SQL comments.
func skipBlank(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n\f\v")
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return ""
			}
			s = s[i+1:]
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s[2:], "*/")
			if i < 0 {
				return ""
			}
			s = s[i+4:]
		default:
			return s
		}
	}
}

// callsFunc reports whether low (lower case) has name as a word followed,
// after optional blanks, by "(". A column of that name followed by "("
// would match too: that read just stays on the writer.
func callsFunc(low, name string) bool {
	for i := 0; ; {
		j := strings.Index(low[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		i = end
		if start > 0 && identByte(low[start-1]) {
			continue
		}
		if end < len(low) && identByte(low[end]) {
			continue
		}
		if rest := strings.TrimLeft(low[end:], " \t\r\n\f\v"); strings.HasPrefix(rest, "(") {
			return true
		}
	}
}

func identByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// connKind is what a connection runs.
type connKind int8

const (
	connW connKind = iota
	connI
	connB
)

// conn is one SQLite connection behind its admission.
type conn struct {
	db          *sqlite.DB
	name        string // W, I0, I1, B: the SQL trace's lane
	kind        connKind
	busyTimeout time.Duration // how long a statement retries SQLITE_BUSY (retryBusy)
	busy        bool          // sched.mu: a caller holds it
	busyWaits   atomic.Int64  // statements that met SQLITE_BUSY
	busyNanos   atomic.Int64  // time they spent retrying
	execNanos   atomic.Int64  // time statements held it
}

// waiter is a caller queued in a sched.
type waiter struct {
	lane Lane
	want func(*conn) bool
	got  chan *conn // receives the connection handed over; capacity 1
}

// sched is the admission of a set of connections: one caller holds a
// connection at a time. A free connection goes to the first one a caller
// wants; a released one goes to the waiter of the highest lane that wants
// it, the earliest within a lane. So a free connection never has a waiter
// that wants it.
type sched struct {
	mu      sync.Mutex
	conns   []*conn   // in order of preference
	waiters []*waiter // in order of arrival
}

// acquire returns a connection want accepts, waiting by lane; it returns
// ctx's error, holding nothing, when ctx ends first.
func (s *sched) acquire(ctx context.Context, lane Lane, want func(*conn) bool) (*conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, c := range s.conns {
		if !c.busy && want(c) {
			c.busy = true
			s.mu.Unlock()
			return c, nil
		}
	}
	w := &waiter{lane: lane, want: want, got: make(chan *conn, 1)}
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()
	select {
	case c := <-w.got:
		return c, nil
	case <-ctx.Done():
	}
	s.mu.Lock()
	if i := slices.Index(s.waiters, w); i >= 0 {
		s.waiters = slices.Delete(s.waiters, i, i+1)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
	s.mu.Unlock()
	// Handed a connection as ctx ended: pass it on.
	s.release(<-w.got)
	return nil, ctx.Err()
}

// release gives c back: to the best waiter that wants it, or free.
func (s *sched) release(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := -1
	for i, w := range s.waiters {
		if w.want(c) && (best < 0 || w.lane > s.waiters[best].lane) {
			best = i
		}
	}
	if best < 0 {
		c.busy = false
		return
	}
	w := s.waiters[best]
	s.waiters = slices.Delete(s.waiters, best, best+1)
	w.got <- c
}

// queued is the number of callers waiting, for tests.
func (s *sched) queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.waiters)
}

func anyConn(*conn) bool          { return true }
func backgroundConn(c *conn) bool { return c.kind == connB }

// wantFor is what a reader statement of lane may take: an interactive one
// any reader (I first, then an idle B), every other one B alone.
func wantFor(lane Lane) func(*conn) bool {
	if lane == Interactive {
		return anyConn
	}
	return backgroundConn
}

// Reader settings (ADR 0052). SQLite drops a reader's page cache whenever
// another connection has committed, which a sync does every few
// milliseconds, so a large reader cache buys nothing and costs RSS; the
// OS page cache serves the rest. mmap pages would count in RSS.
const (
	readerCacheKiB     = 1024
	interactiveBusyFor = 2 * time.Second
	backgroundBusyFor  = 5 * time.Second
	writerBusyFor      = 5 * time.Second
	// slowInteractive is the run time from which an interactive statement
	// is logged, once per statement text: the work list for indexes.
	slowInteractive = 250 * time.Millisecond
	maxSlowLogged   = 512
	// walSizeLimit is the writer's journal_size_limit: after a checkpoint
	// restarts the WAL, the file is cut back to this size.
	walSizeLimit = 64 << 20
)

// lanes is DB's connections and their admission. Until Open has migrated
// (ready), every statement runs on the writer.
type lanes struct {
	path  string
	w     *conn
	wq    sched
	ready atomic.Bool // migrated: plain SELECTs may go to the readers

	mu      sync.Mutex // opening and closing the readers
	readers atomic.Pointer[readerSet]
	off     bool // closed, not WAL, or opening failed: reads stay on W

	commits atomic.Uint64 // writes on W, for the checkpoint coordinator
	active  atomic.Int64  // unix ns of the last statement on any connection

	slowMu sync.Mutex
	slow   map[string]bool // statement texts logged as slow

	ck *checkpointer // checkpoint.go

	onReader func(conn, sql string) // tests: a statement about to run on a reader
}

// readerSet is the open readers and their admission.
type readerSet struct {
	sched
	i0, i1, b *conn
}

func newLanes(path string, w *sqlite.DB) *lanes {
	l := &lanes{path: path, w: &conn{db: w, name: "W", kind: connW, busyTimeout: writerBusyFor}}
	l.wq.conns = []*conn{l.w}
	l.active.Store(time.Now().UnixNano())
	return l
}

// readerSet returns the readers, opening them the first time; nil means
// the writer. A caller that never reads through a reader, such as the dev
// database copy that switches to journal_mode = DELETE (core/devsync.go),
// never opens them.
func (l *lanes) readerSet() *readerSet {
	if rs := l.readers.Load(); rs != nil {
		return rs
	}
	if !l.ready.Load() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if rs := l.readers.Load(); rs != nil {
		return rs
	}
	if l.off {
		return nil
	}
	rs, err := l.openReaders()
	if err != nil {
		l.off = true
		slog.Default().Warn("could not open read-only database connections; reads share the writer", "err", err)
		return nil
	}
	if rs == nil {
		l.off = true
		return nil
	}
	l.readers.Store(rs)
	return rs
}

// openReaders opens I0, I1 and B; nil, nil when the database is not in
// WAL mode, where a reader would wait for the writer anyway.
func (l *lanes) openReaders() (*readerSet, error) {
	// Not the caller's ctx: a canceled first read must not turn the readers off.
	ctx := context.Background()
	rows, err := l.w.db.Query(ctx, "PRAGMA journal_mode")
	if err != nil {
		return nil, err
	}
	if len(rows.Values) != 1 || !strings.EqualFold(Str(rows.Values[0][0]), "wal") {
		return nil, nil
	}
	var opened []*conn
	fail := func(err error) (*readerSet, error) {
		for _, c := range opened {
			c.db.Close()
		}
		return nil, err
	}
	for _, spec := range []struct {
		name string
		kind connKind
		busy time.Duration
	}{{"I0", connI, interactiveBusyFor}, {"I1", connI, interactiveBusyFor}, {"B", connB, backgroundBusyFor}} {
		// No busy wait in SQLite: retryBusy waits in Go, cancelable and counted.
		db, err := sqlite.Open(ctx, l.path, sqlite.OpenOptions{ReadOnly: true, BusyTimeout: -1})
		if err != nil {
			return fail(err)
		}
		c := &conn{db: db, name: spec.name, kind: spec.kind, busyTimeout: spec.busy}
		opened = append(opened, c)
		for _, p := range []string{"PRAGMA cache_size = -" + strconv.Itoa(readerCacheKiB), "PRAGMA mmap_size = 0"} {
			if _, err := l.retryBusy(ctx, c, p, func() error { _, err := db.Query(ctx, p); return err }); err != nil {
				return fail(err)
			}
		}
	}
	rs := &readerSet{i0: opened[0], i1: opened[1], b: opened[2]}
	rs.conns = opened
	return rs, nil
}

// closeReaders closes the readers and keeps later reads on the writer.
func (l *lanes) closeReaders() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.off = true
	rs := l.readers.Swap(nil)
	if rs == nil {
		return nil
	}
	var err error
	for _, c := range rs.conns {
		err = errors.Join(err, c.db.Close())
	}
	return err
}

// read runs sql on a reader of lane's; ok is false when it must run on
// the writer instead: no readers, or the reader met SQLITE_BUSY past its
// timeout, SQLITE_READONLY, or was closed under it.
func (l *lanes) read(ctx context.Context, lane Lane, method, sql string, args []any) (rows sqlite.Rows, err error, ok bool) {
	rs := l.readerSet()
	if rs == nil {
		return sqlite.Rows{}, nil, false
	}
	if lane == Background {
		yieldToUI(ctx)
	}
	start := time.Now()
	c, err := rs.acquire(ctx, lane, wantFor(lane))
	if err != nil {
		return sqlite.Rows{}, err, true
	}
	if l.onReader != nil {
		l.onReader(c.name, sql)
	}
	run := time.Now()
	_, err = l.retryBusy(ctx, c, sql, func() error {
		var e error
		rows, e = c.db.Query(ctx, sql, args...)
		return e
	})
	rs.release(c)
	now := time.Now()
	c.execNanos.Add(int64(now.Sub(run)))
	l.active.Store(now.UnixNano())
	if lane == Interactive {
		l.noteSlow(method, sql, c.name, now.Sub(run))
	}
	if perftrace.SQLOn() {
		perftrace.SQL(ctx, "q@"+c.name+":"+lane.letter(), sql, start)
	}
	if err != nil && ctx.Err() == nil && (isCode(err, sqliteBusy) || isCode(err, sqliteReadOnly) || errors.Is(err, sqlite.ErrClosed)) {
		slog.Default().Info("a read failed on a read-only connection; running it on the writer", "conn", c.name, "err", err, "sql", stmtPrefix(sql))
		return sqlite.Rows{}, nil, false
	}
	return rows, err, true
}

// onW runs fn on the writer, after its admission. kind is the SQL trace's
// call kind (q, x, t<n>).
func (l *lanes) onW(ctx context.Context, kind, sql string, write bool, fn func() error) error {
	lane, _ := LaneOf(ctx)
	if lane == Background {
		yieldToUI(ctx)
	}
	start := time.Now()
	c, err := l.wq.acquire(ctx, lane, anyConn)
	if err != nil {
		return err
	}
	run := time.Now()
	_, err = l.retryBusy(ctx, c, sql, fn)
	l.wq.release(c)
	if write {
		l.commits.Add(1)
	}
	now := time.Now()
	c.execNanos.Add(int64(now.Sub(run)))
	l.active.Store(now.UnixNano())
	if perftrace.SQLOn() {
		perftrace.SQL(ctx, kind+"@W:"+lane.letter(), sql, start)
	}
	return err
}

// SQLite result codes the store handles.
const (
	sqliteBusy     = 5
	sqliteReadOnly = 8
)

func isCode(err error, code int) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code == code
}

// busyBackoff is SQLite's own busy-handler schedule.
var busyBackoff = []time.Duration{1, 2, 5, 10, 15, 20, 25, 25, 25, 50, 50, 100}

// retryBusy runs fn and, while it fails with SQLITE_BUSY, again after
// SQLite's busy-handler pauses, for at most c.busyTimeout. The connections
// open with no busy wait of SQLite's own, so the wait here can be
// canceled and is counted: on one connection the Mac's trace shows single
// statements stalled for 0.35 to 1.4 s. A statement that met SQLITE_BUSY
// changed nothing (a Transaction rolls back), so running it again is safe.
// waited is the time spent retrying.
func (l *lanes) retryBusy(ctx context.Context, c *conn, sql string, fn func() error) (waited time.Duration, err error) {
	err = fn()
	if !isCode(err, sqliteBusy) {
		return 0, err
	}
	start := time.Now()
	for i := 0; isCode(err, sqliteBusy); i++ {
		d := busyBackoff[min(i, len(busyBackoff)-1)] * time.Millisecond
		if time.Since(start)+d > c.busyTimeout {
			break
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			waited = time.Since(start)
			l.countBusy(ctx, c, sql, start)
			return waited, ctx.Err()
		case <-t.C:
		}
		err = fn()
	}
	waited = time.Since(start)
	l.countBusy(ctx, c, sql, start)
	return waited, err
}

func (l *lanes) countBusy(ctx context.Context, c *conn, sql string, start time.Time) {
	d := time.Since(start)
	c.busyWaits.Add(1)
	c.busyNanos.Add(int64(d))
	if perftrace.SQLOn() {
		perftrace.SQL(ctx, "busy@"+c.name, sql, start)
	}
	if d >= 100*time.Millisecond {
		slog.Default().Debug("database statement waited for a lock", "conn", c.name, "wait", d.Round(time.Millisecond), "sql", stmtPrefix(sql))
	}
}

// noteSlow logs an interactive statement that ran slowInteractive or
// longer, once per statement text, with the method that ran it.
func (l *lanes) noteSlow(method, sql, connName string, d time.Duration) {
	if d < slowInteractive {
		return
	}
	l.slowMu.Lock()
	if l.slow[sql] || len(l.slow) >= maxSlowLogged {
		l.slowMu.Unlock()
		return
	}
	if l.slow == nil {
		l.slow = map[string]bool{}
	}
	l.slow[sql] = true
	l.slowMu.Unlock()
	slog.Default().Info("slow interactive statement", "method", method, "conn", connName, "ms", d.Milliseconds(), "sql", stmtPrefix(sql))
}

// stmtPrefix is the start of a statement for a log line: its text, never
// its arguments, so no mail content.
func stmtPrefix(sql string) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// LaneStats is one connection's counters, for tests and diagnostics.
type LaneStats struct {
	Conn      string
	BusyWaits int64
	BusyTime  time.Duration
	Busy      time.Duration // time statements held the connection
}

// Stats is the counters of the connections opened so far.
func (db *DB) Stats() []LaneStats {
	l := db.lanes
	if l == nil {
		return nil
	}
	cs := []*conn{l.w}
	if rs := l.readers.Load(); rs != nil {
		cs = append(cs, rs.conns...)
	}
	out := make([]LaneStats, 0, len(cs))
	for _, c := range cs {
		out = append(out, LaneStats{Conn: c.name, BusyWaits: c.busyWaits.Load(), BusyTime: time.Duration(c.busyNanos.Load()), Busy: time.Duration(c.execNanos.Load())})
	}
	return out
}
