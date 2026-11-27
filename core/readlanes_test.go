package core

import (
	"context"
	"testing"
	"time"

	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// endlessRead runs until its context ends.
const endlessRead = "SELECT count(*) FROM (WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT x FROM c)"

// hold runs endlessRead on ctx until the test ends.
func hold(t *testing.T, s *Service, ctx context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := s.db.Query(ctx, endlessRead)
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	time.Sleep(100 * time.Millisecond) // it has started
}

// The interface's reads run on their own connections (ADR 0052): while
// the backend's work holds the writer, the calls a window makes at launch
// and on a message open still answer, and a write the interface makes is
// seen by its next read.
func TestInterfaceReadsSkipTheWriter(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("lanes@x"))
	e.sync(t)
	id := e.idOf(t, "lanes@x")
	ui := WithUISession(t.Context(), e.s)
	require.NoError(t, e.s.SetFlags(ui, []int64{id}, []string{`\Flagged`}, nil))

	hold(t, e.s, bg) // a background scan on the writer
	quick, stop := context.WithTimeout(ui, 3*time.Second)
	defer stop()
	accs, err := e.s.ListAccounts(quick)
	require.NoError(t, err)
	assert.Len(t, accs, 1)
	page, err := e.s.ListThreads(quick, View{Unified: "inbox"}, "", 50)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	th, err := e.s.GetThread(quick, page.Items[0].ID)
	require.NoError(t, err)
	require.Len(t, th.Messages, 1)
	assert.Contains(t, th.Messages[0].Flags, `\Flagged`, "the interface's read sees its committed write")
	_, err = e.s.SavedViews(quick)
	require.NoError(t, err)
	_, err = e.s.ThreadDeadline(quick, page.Items[0].ID)
	require.NoError(t, err)
}

// The interface's scans (saved views) wait for the long lane alone: while
// one holds it, a thread still opens on the quick lane, and so does the
// deadline a reader message shows, which never queues behind them.
func TestInterfaceScansLeaveTheQuickLane(t *testing.T) {
	e := newSyncEnv(t)
	e.put(t, "INBOX", msg("scan@x"))
	e.sync(t)
	ui := WithUISession(t.Context(), e.s)
	page, err := e.s.ListThreads(ui, View{Unified: "inbox"}, "", 50)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	tid := page.Items[0].ID

	hold(t, e.s, store.WithLongRead(ui)) // a scan on the long lane
	quick, stop := context.WithTimeout(ui, 3*time.Second)
	defer stop()
	_, err = e.s.GetThread(quick, tid)
	require.NoError(t, err, "a thread opens beside the scan")
	_, err = e.s.ListAccounts(quick)
	require.NoError(t, err)
	_, err = e.s.ThreadDeadline(quick, tid)
	require.NoError(t, err, "the reader's deadline does not queue behind the scan")

	short, stop2 := context.WithTimeout(ui, 200*time.Millisecond)
	defer stop2()
	_, err = e.s.SavedViews(short)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "SavedViews queues for the long lane")
}

// Work detached from an interface call reads on the readers only while
// the connection lives, and a Wit run, the backend's work, never does.
func TestDetachedInterfaceWorkLeavesTheReaders(t *testing.T) {
	e := newSyncEnv(t)
	conn, end := context.WithCancel(t.Context())
	ui := WithUISession(conn, e.s)
	call, done := context.WithCancel(ui)
	detached := context.WithoutCancel(call)
	done()

	hold(t, e.s, bg) // a background scan on the writer
	quick, stop := context.WithTimeout(detached, 3*time.Second)
	defer stop()
	_, err := e.s.ListAccounts(quick)
	require.NoError(t, err, "the connection lives: a reader")

	writer := func(ctx context.Context, name string) {
		t.Helper()
		short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
		defer stop()
		_, err := e.s.ListAccounts(short)
		assert.ErrorIs(t, err, context.DeadlineExceeded, "%s: waits for the writer", name)
	}
	writer(runCaller(ui), "a Wit run")
	end()
	writer(detached, "the connection gone")
}
