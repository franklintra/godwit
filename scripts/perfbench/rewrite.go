//go:build linux

package main

import (
	"context"
	"crypto/sha3"
	"encoding/binary"
	"fmt"
	"hash"
	"log"
	"math"
	"path/filepath"
	"syscall"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/store"
)

// The fork rewrites (FORK.md, "Fork rewrites"; ADR 0053) move data in
// the background after the build's first open of a database: on a copy
// of a stock database, the mail bodies to message_bodies. A copy is
// measured once they are done (premove), so no part measures the move by
// accident; the migrate part measures the interface during the move, and
// the killmove part kills the backend in the middle of it.

// rewriteWatch samples a copy while its rewrites run: their state, the
// WAL's size, the main file's size. Its connection runs one short
// statement at a time, so it holds no snapshot that would stop a
// checkpoint.
type rewriteWatch struct {
	db      *sqlite.DB
	path    string
	walPeak int64
}

func watchRewrites(ctx context.Context, path string) (*rewriteWatch, error) {
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		return nil, err
	}
	return &rewriteWatch{db: db, path: path}, nil
}

// poll samples the WAL and says whether every rewrite is done.
func (w *rewriteWatch) poll(ctx context.Context) (bool, error) {
	w.walPeak = max(w.walPeak, fileSize(w.path+"-wal"))
	for _, r := range store.ForkRewrites {
		rows, err := w.db.Query(ctx, "SELECT value FROM meta WHERE key = ?", "fork."+r.Name)
		if err != nil {
			return false, err
		}
		if len(rows.Values) == 0 || store.Str(rows.Values[0][0]) != store.RewriteDone {
			return false, nil
		}
	}
	return true, nil
}

func (w *rewriteWatch) close() { w.db.Close() }

// waitRewrites polls until every rewrite is done, the backend exits or
// limit passes.
func waitRewrites(ctx context.Context, b *backend, w *rewriteWatch, limit time.Duration, between func() error) error {
	end := time.Now().Add(limit)
	for {
		done, err := w.poll(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case err := <-b.exited:
			b.exited <- err
			return fmt.Errorf("backend exited during the move (%v); log:\n%s", err, redact(tail(b.logPath, 20)))
		default:
		}
		if time.Now().After(end) {
			return fmt.Errorf("the rewrites were not done after %s", limit)
		}
		if between != nil {
			if err := between(); err != nil {
				return err
			}
		} else {
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// recordMove adds a move's numbers under prefix: its time, the WAL's peak,
// and the main file's growth from before to after (the backend stopped,
// its WAL folded in).
func recordMove(rep *report, prefix string, took time.Duration, walPeak, before, after int64) {
	rep.add(prefix+".time_to_done_s", "s", "lower", "backend start to every fork rewrite done (the body move), the interface idle", took.Seconds())
	rep.add(prefix+".wal_peak_mb", "MB", "lower", "largest WAL seen during the move (sampled every 250 ms)", float64(walPeak)/1e6)
	rep.add(prefix+".file_growth_mb", "MB", "lower", "main file after the move minus the copy before its first open", float64(after-before)/1e6)
	if before > 0 {
		rep.add(prefix+".file_growth_pct", "%", "lower", "the same, in % of the copy", 100*float64(after-before)/float64(before))
	}
	log.Printf("%s: done in %s, WAL peak %.0f MB, file %.0f -> %.0f MB", prefix, took.Round(time.Second), float64(walPeak)/1e6, float64(before)/1e6, float64(after)/1e6)
}

// premove runs the build on the copy without the interface until every
// fork rewrite is done, and records the move under prefix.
func premove(ctx context.Context, o *opener, before int64, rep *report, prefix string) error {
	path := filepath.Join(o.data, "godwit.db")
	b, err := startBackend(o.bin, o.data, o.env, o.group, false)
	if err != nil {
		return err
	}
	defer b.cleanup()
	w, err := watchRewrites(ctx, path)
	if err != nil {
		b.stop()
		return err
	}
	err = waitRewrites(ctx, b, w, 30*time.Minute, nil)
	took := time.Since(b.start)
	w.close()
	b.stop()
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	recordMove(rep, prefix, took, w.walPeak, before, fileSize(path))
	return nil
}

// runMigrate opens mail, as the reader does, while the move runs on a
// copy the build has opened once, then as many times after it is done.
func runMigrate(ctx context.Context, o *opener, before int64, rep *report) error {
	path := filepath.Join(o.data, "godwit.db")
	b, err := startBackend(o.bin, o.data, o.env, o.group, false)
	if err != nil {
		return err
	}
	defer func() { b.stop(); b.cleanup() }()
	c, _, err := b.dialUI(ctx, 10*time.Minute, nil, "perfbench-migrate")
	if err != nil {
		return err
	}
	defer c.Close()
	w, err := watchRewrites(ctx, path)
	if err != nil {
		return err
	}
	tids, err := listThreadIDs(ctx, c, 60)
	if err != nil {
		w.close()
		return err
	}
	n := 0
	open := func(prefix string) error {
		tid := tids[n%len(tids)]
		n++
		t0 := time.Now()
		done := make(chan struct{})
		_, shown, err := openMail(ctx, c, tid, func(mailOpen) { close(done) })
		if err != nil {
			return fmt.Errorf("open thread %d: %w", tid, err)
		}
		<-done
		rep.add(prefix+".message_open_ms", "ms", "lower", "selection to the newest message's body read (GetThread, ThreadLabels, GetMessageLazyImages, ImagesAllowed), as the window part", ms(shown))
		rep.add(prefix+".open_all_ms", "ms", "lower", "selection to the last of the mail's calls answered", ms(time.Since(t0)))
		l0 := time.Now()
		if _, err := c.ListThreads(ctx, core.View{Unified: "inbox"}, "", pageSize); err != nil {
			return err
		}
		rep.add(prefix+".list_ms", "ms", "lower", "ListThreads, unified inbox, first page", ms(time.Since(l0)))
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	err = waitRewrites(ctx, b, w, 30*time.Minute, func() error { return open("migrate.during") })
	took := time.Since(b.start)
	opened := n
	w.close()
	if err != nil {
		return err
	}
	rep.add("migrate.during.opens", "count", "higher", "mails opened during the move", float64(opened))
	for range max(opened, 20) {
		if err := open("migrate.after"); err != nil {
			return err
		}
	}
	b.stop()
	recordMove(rep, "migrate", took, w.walPeak, before, fileSize(path))
	return nil
}

// bodyHashes are SHA3-256 sums of every message's bodies (id and both
// columns, wherever stored) and of the full-text index's shadow tables,
// read in pages so no table is held in memory.
type bodyHashes struct {
	Bodies, FTS string
	WithBody    int64 // messages with a body, inline or moved
}

func hashBodies(ctx context.Context, db *sqlite.DB) (bodyHashes, error) {
	var out bodyHashes
	hasTable := func(name string) bool {
		r, err := db.Query(ctx, "SELECT 1 FROM sqlite_master WHERE name = ?", name)
		return err == nil && len(r.Values) > 0
	}
	q := "SELECT id, body_text, body_html FROM messages WHERE id > ? ORDER BY id LIMIT 200"
	if hasTable("message_bodies") {
		q = "SELECT m.id, " + store.BodyTextSQL("m") + ", " + store.BodyHTMLSQL("m") + " FROM messages m WHERE m.id > ? ORDER BY m.id LIMIT 200"
	}
	h := sha3.New256()
	if err := hashPages(ctx, db, q, h, func(r []any) {
		if r[1] != nil || r[2] != nil {
			out.WithBody++
		}
	}); err != nil {
		return out, err
	}
	out.Bodies = fmt.Sprintf("%x", h.Sum(nil))
	h = sha3.New256()
	for _, t := range []string{"messages_fts_data", "messages_fts_content", "messages_fts_docsize"} {
		if err := hashPages(ctx, db, "SELECT * FROM "+t+" WHERE id > ? ORDER BY id LIMIT 500", h, nil); err != nil {
			return out, err
		}
	}
	for _, t := range []string{"messages_fts_idx", "messages_fts_config"} {
		r, err := db.Query(ctx, "SELECT * FROM "+t+" ORDER BY 1, 2")
		if err != nil {
			return out, err
		}
		for _, row := range r.Values {
			hashRow(h, row)
		}
	}
	out.FTS = fmt.Sprintf("%x", h.Sum(nil))
	return out, nil
}

// hashPages hashes q's rows page by page; q takes the last first column.
func hashPages(ctx context.Context, db *sqlite.DB, q string, h hash.Hash, each func([]any)) error {
	last := int64(math.MinInt64)
	for {
		r, err := db.Query(ctx, q, last)
		if err != nil {
			return err
		}
		for _, row := range r.Values {
			hashRow(h, row)
			if each != nil {
				each(row)
			}
		}
		if len(r.Values) == 0 {
			return nil
		}
		last = store.Int(r.Values[len(r.Values)-1][0])
	}
}

func hashRow(h hash.Hash, row []any) {
	var n [8]byte
	for _, v := range row {
		switch x := v.(type) {
		case nil:
			h.Write([]byte{0})
		case int64:
			binary.BigEndian.PutUint64(n[:], uint64(x))
			h.Write([]byte{1})
			h.Write(n[:])
		case float64:
			binary.BigEndian.PutUint64(n[:], math.Float64bits(x))
			h.Write([]byte{2})
			h.Write(n[:])
		case string:
			binary.BigEndian.PutUint64(n[:], uint64(len(x)))
			h.Write([]byte{3})
			h.Write(n[:])
			h.Write([]byte(x))
		case []byte:
			binary.BigEndian.PutUint64(n[:], uint64(len(x)))
			h.Write([]byte{4})
			h.Write(n[:])
			h.Write(x)
		}
	}
}

// movePlaces checks the copy after a kill: rows with a body in both
// places, the bodies counted in either place, quick_check and (when fts)
// the full-text index's integrity-check.
func movePlaces(ctx context.Context, path string, fts bool) (both, placed int64, err error) {
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	r, err := db.Query(ctx, `SELECT
		(SELECT count(*) FROM messages m JOIN message_bodies b ON b.message_id = m.id WHERE m.body_text IS NOT NULL OR m.body_html IS NOT NULL),
		(SELECT count(*) FROM messages WHERE body_text IS NOT NULL OR body_html IS NOT NULL) + (SELECT count(*) FROM message_bodies)`)
	if err != nil {
		return 0, 0, err
	}
	both, placed = store.Int(r.Values[0][0]), store.Int(r.Values[0][1])
	qc, err := db.Query(ctx, "PRAGMA quick_check")
	if err != nil {
		return both, placed, err
	}
	if len(qc.Values) != 1 || store.Str(qc.Values[0][0]) != "ok" {
		return both, placed, fmt.Errorf("quick_check: %d problems", len(qc.Values))
	}
	if fts {
		if _, err := db.Execute(ctx, "INSERT INTO messages_fts(messages_fts) VALUES ('integrity-check')"); err != nil {
			return both, placed, fmt.Errorf("FTS integrity-check: %w", err)
		}
	}
	return both, placed, nil
}

// runKillMove kills the backend (SIGKILL) at each of kills into the move
// of a fresh copy, checks the copy after each kill, then lets the move
// finish and compares the bodies and the full-text index with the copy's
// before its first open. Every check is a metric: 1 is a pass.
func runKillMove(ctx context.Context, o *opener, kills []time.Duration, rep *report) error {
	path := filepath.Join(o.data, "godwit.db")
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		return err
	}
	t0 := time.Now()
	pre, err := hashBodies(ctx, db)
	db.Close()
	if err != nil {
		return fmt.Errorf("hash before: %w", err)
	}
	log.Printf("killmove: %d messages with a body hashed in %s", pre.WithBody, time.Since(t0).Round(time.Second))
	pass := func(name, desc string, ok bool) {
		v := 0.0
		if ok {
			v = 1
		}
		rep.add("killmove."+name, "pass", "higher", desc, v)
		log.Printf("killmove: %s: %v", name, ok)
	}
	start := time.Now()
	for i, at := range kills {
		b, err := startBackend(o.bin, o.data, o.env, o.group, false)
		if err != nil {
			return err
		}
		select {
		case err := <-b.exited:
			b.cleanup()
			return fmt.Errorf("backend exited before the kill (%v)", err)
		case <-time.After(at):
		}
		syscall.Kill(-b.pid, syscall.SIGKILL)
		<-b.exited
		b.cleanup()
		both, placed, err := movePlaces(ctx, path, false)
		pass(fmt.Sprintf("kill%d.integrity", i+1), fmt.Sprintf("after SIGKILL %s into the start: quick_check ok", at), err == nil)
		if err != nil && both == 0 && placed == 0 {
			return err
		}
		pass(fmt.Sprintf("kill%d.one_place", i+1), "after the kill: no body in both places, none in neither", both == 0 && placed == pre.WithBody)
		rep.add(fmt.Sprintf("killmove.kill%d.both", i+1), "rows", "lower", "messages with a body in both places after the kill", float64(both))
		rep.add(fmt.Sprintf("killmove.kill%d.lost", i+1), "rows", "lower", "messages whose body is in neither place after the kill", float64(pre.WithBody-placed))
	}
	b, err := startBackend(o.bin, o.data, o.env, o.group, false)
	if err != nil {
		return err
	}
	w, err := watchRewrites(ctx, path)
	if err != nil {
		b.stop()
		b.cleanup()
		return err
	}
	err = waitRewrites(ctx, b, w, 30*time.Minute, nil)
	w.close()
	b.stop()
	b.cleanup()
	pass("completes", "the move finishes after the kills", err == nil)
	if err != nil {
		return err
	}
	rep.add("killmove.time_to_done_s", "s", "lower", "first start to done, kills and restarts included", time.Since(start).Seconds())
	_, _, ferr := movePlaces(ctx, path, true)
	pass("final_integrity", "after the move: quick_check and the FTS integrity-check ok", ferr == nil)
	db, err = sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		return err
	}
	post, err := hashBodies(ctx, db)
	inline := int64(-1)
	if r, qerr := db.Query(ctx, "SELECT count(*) FROM messages WHERE body_text IS NOT NULL OR body_html IS NOT NULL"); qerr == nil {
		inline = store.Int(r.Values[0][0])
	}
	db.Close()
	if err != nil {
		return fmt.Errorf("hash after: %w", err)
	}
	pass("none_inline", "after the move: no body left inline", inline == 0)
	pass("bodies_sha3", "SHA3 of (id, body_text, body_html) equal before and after", pre.Bodies == post.Bodies)
	pass("fts_sha3", "SHA3 of the FTS shadow tables (data, idx, content, docsize, config) equal before and after", pre.FTS == post.FTS)
	if ferr != nil {
		return ferr
	}
	return nil
}
