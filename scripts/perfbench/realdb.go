//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/godwit-email/godwit/core/store"
)

// The real-database mode (-db) runs the parts on a copy of a real
// Godwit database instead of the fixture. That database holds private
// mail and real accounts, so the mode:
//
//   - refuses to run unless the process sees no network but loopback
//     (checkIsolated; run.sh starts it in a container with --network
//     none);
//   - copies the database into one scratch directory, and never writes
//     the source;
//   - points every account's servers at 127.0.0.1:1 in the copy before
//     the first start (neutralize), and gives the backends an empty
//     keyring, so no account can log in anywhere;
//   - writes no mail content, address or subject to the report: the
//     accounts get neutral names, and backend log lines lose addresses.

// realDB is the real-database mode's source and scratch directory.
type realDB struct {
	src     string // the database file to measure; only read
	scratch string // the one directory the copy lives in; emptied at the end
	native  string // mygo's native-module cache, copied into each backend's HOME
}

// realSearchTerms are what the search hot path asks on a real database:
// common words of French and English mail, and one made-up word (a
// search with no hit).
var realSearchTerms = []string{"projet", "facture", "meeting", "github", "zqxjvw"}

// checkIsolated refuses to go on unless no network but loopback is
// reachable: a backend on a real database must never reach a real mail
// server (it could move or delete real mail). On the VPS, 127.0.0.1:1143
// is a real IMAP gateway: it must not answer either.
func checkIsolated() error {
	ifs, err := net.Interfaces()
	if err != nil {
		return err
	}
	var names []string
	for _, i := range ifs {
		names = append(names, i.Name)
		if i.Flags&net.FlagLoopback == 0 {
			return fmt.Errorf("network interface %s is present: run in a network namespace with loopback only (run.sh -db does it with docker --network none)", i.Name)
		}
	}
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:53", "[2606:4700:4700::1111]:443", "127.0.0.1:1143", "127.0.0.1:993", "127.0.0.1:5432"} {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			c.Close()
			return fmt.Errorf("%s answered: the process is not isolated", addr)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0") // the loopback IMAP server needs loopback
	if err != nil {
		return fmt.Errorf("loopback: %w", err)
	}
	defer ln.Close()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		return fmt.Errorf("loopback: %w", err)
	}
	c.Close()
	log.Printf("isolation: ok (interfaces %v; outside and host services unreachable; loopback works)", names)
	return nil
}

// fresh copies the source into dir (gone first): data/godwit.db, an empty
// file keyring, a HOME with mygo's native module. It neutralizes the copy
// before anything starts on it, and returns the layout as a fixtureMeta
// (without the targets: see targets).
func (r realDB) fresh(dir string) (fixtureMeta, error) {
	meta := fixtureMeta{Name: "realdb:" + filepath.Base(r.src), Real: true, DataDir: "data", KeyringFile: "keyring.bin", KeyringKey: "keyring.key",
		Unreachable: "127.0.0.1:1 (connection refused), in a network namespace with loopback only"}
	if err := os.RemoveAll(dir); err != nil {
		return meta, err
	}
	data := filepath.Join(dir, meta.DataDir)
	if err := os.MkdirAll(data, 0o700); err != nil {
		return meta, err
	}
	t0 := time.Now()
	for _, suf := range []string{"", "-wal"} { // a VACUUM INTO copy has no WAL; a live one may
		if err := copyFile(r.src+suf, filepath.Join(data, "godwit.db"+suf)); err != nil {
			if suf != "" && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return meta, fmt.Errorf("copy: %w", err)
		}
	}
	log.Printf("realdb: copied %.0f MB in %s", float64(fileSize(r.src))/1e6, time.Since(t0).Round(time.Millisecond))
	if err := neutralize(context.Background(), filepath.Join(data, "godwit.db")); err != nil {
		return meta, fmt.Errorf("neutralize: %w", err)
	}
	if err := newMemKeyring().save(filepath.Join(dir, meta.KeyringFile), filepath.Join(dir, meta.KeyringKey)); err != nil {
		return meta, err
	}
	if r.native != "" {
		if err := copyTree(r.native, filepath.Join(dir, "home", ".cache", "mygo")); err != nil {
			return meta, fmt.Errorf("native cache: %w", err)
		}
	}
	dropCache(data)
	return meta, nil
}

// neutralize points every account's servers at a closed port and every
// other configured endpoint at nothing, in the copy, with a raw
// connection (no migration: the build's first open does those, timed as
// open.first). It checks the result.
func neutralize(ctx context.Context, path string) error {
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		return err
	}
	defer db.Close()
	cols := func(table string) map[string]bool {
		out := map[string]bool{}
		r, err := db.Query(ctx, "SELECT name FROM pragma_table_info(?)", table)
		if err == nil {
			for _, v := range r.Values {
				out[store.Str(v[0])] = true
			}
		}
		return out
	}
	acc := cols("accounts")
	var set []string
	for _, c := range []string{"recv_host", "send_host", "imap_host", "smtp_host"} {
		if acc[c] {
			set = append(set, c+" = '127.0.0.1'")
		}
	}
	for _, c := range []string{"recv_port", "send_port", "imap_port", "smtp_port"} {
		if acc[c] {
			set = append(set, c+" = 1")
		}
	}
	if len(set) < 4 {
		return fmt.Errorf("accounts has no host and port columns this mode knows: %v", acc)
	}
	stmts := []string{"UPDATE accounts SET " + strings.Join(set, ", ")}
	for table, c := range map[string]string{"model_services": "base_url", "mcp_servers": "url", "address_books": "url"} {
		if cols(table)[c] {
			stmts = append(stmts, fmt.Sprintf("UPDATE %s SET %s = 'http://127.0.0.1:1/'", table, c))
		}
	}
	if cols("mcp_servers")["command"] {
		stmts = append(stmts, "UPDATE mcp_servers SET command = '/bin/false'")
	}
	for _, s := range stmts {
		if _, err := db.Execute(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	var where []string
	for _, s := range set {
		col, val, _ := strings.Cut(s, " = ")
		where = append(where, fmt.Sprintf("%s IS NOT %s", col, val))
	}
	r, err := db.Query(ctx, "SELECT count(*), (SELECT count(*) FROM accounts) FROM accounts WHERE "+strings.Join(where, " OR "))
	if err != nil {
		return err
	}
	if n := store.Int(r.Values[0][0]); n != 0 {
		return fmt.Errorf("%d accounts still point elsewhere", n)
	}
	if _, err := db.Query(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	log.Printf("realdb: %d accounts point at 127.0.0.1:1; keyring empty", store.Int(r.Values[0][1]))
	return nil
}

// targets fills what the hot paths and the window open, from the copy
// after the build's first open (its migrations done). Accounts are in
// order of size, named by id only.
func (r realDB) targets(ctx context.Context, dbPath string, meta *fixtureMeta) error {
	t0 := time.Now()
	db, err := sqlite.Open(ctx, dbPath, sqlite.OpenOptions{})
	if err != nil {
		return err
	}
	defer db.Close()
	q := func(sql string, args ...any) ([][]any, error) {
		r, err := db.Query(ctx, sql, args...)
		return r.Values, err
	}
	one := func(sql string) int64 {
		r, err := q(sql)
		if err != nil || len(r) == 0 {
			if err != nil {
				meta.Notes = append(meta.Notes, fmt.Sprintf("%.60s: %v", sql, err))
			}
			return 0
		}
		return store.Int(r[0][0])
	}
	rows, err := q("SELECT account_id, count(*) FROM messages GROUP BY account_id ORDER BY 2 DESC")
	if err != nil {
		return err
	}
	for _, row := range rows {
		ma := metaAccount{ID: store.Int(row[0]), Messages: int(store.Int(row[1]))}
		ma.Email = fmt.Sprintf("account-%d", ma.ID)
		// the folders by role, empty ones left out: the hot paths fail on an
		// empty list; Archive is the archive or All Mail, the fuller one
		fr, err := q("SELECT f.id, f.role, (SELECT count(*) FROM message_locations l WHERE l.folder_id = f.id) FROM folders f WHERE f.account_id = ? AND f.role IN ('inbox', 'sent', 'archive', 'all')", ma.ID)
		if err != nil {
			return err
		}
		archiveN := int64(0)
		for _, f := range fr {
			id, n := store.Int(f[0]), store.Int(f[2])
			if n == 0 {
				continue
			}
			switch store.Str(f[1]) {
			case "inbox":
				ma.Inbox = id
			case "sent":
				ma.Sent = id
			default:
				if n > archiveN {
					ma.Archive, archiveN = id, n
				}
			}
		}
		meta.Accounts = append(meta.Accounts, ma)
		meta.Messages += ma.Messages
	}
	if len(meta.Accounts) == 0 || meta.Accounts[0].Inbox == 0 {
		return fmt.Errorf("the database has no account with an inbox")
	}
	meta.Threads = int(one("SELECT count(*) FROM threads"))
	if rs, err := q("SELECT id, name FROM saved_views ORDER BY position, id"); err == nil {
		for _, v := range rs {
			meta.Views = append(meta.Views, metaView{store.Int(v[0]), store.Str(v[1])})
		}
	}
	if rs, err := q("SELECT thread_id, count(*) FROM messages GROUP BY thread_id ORDER BY 2 DESC LIMIT 1"); err == nil && len(rs) > 0 {
		meta.BigThread, meta.BigThreadN = store.Int(rs[0][0]), int(store.Int(rs[0][1]))
	}
	meta.BigMessage = one("SELECT id FROM messages ORDER BY length(body_html) DESC LIMIT 1")
	meta.TypMessage = one("SELECT id FROM messages WHERE body_html IS NOT NULL ORDER BY abs(length(body_html) - 6000), id LIMIT 1")
	meta.TopLabel = one("SELECT label_id FROM message_labels WHERE active = 1 GROUP BY label_id ORDER BY count(*) DESC LIMIT 1")
	meta.SearchTerms = realSearchTerms
	meta.TableMB = map[string]float64{}
	if rs, err := q("SELECT name, sum(pgsize) FROM dbstat GROUP BY name ORDER BY 2 DESC LIMIT 12"); err == nil {
		for _, t := range rs {
			meta.TableMB[store.Str(t[0])] = float64(store.Int(t[1])) / 1e6
		}
	} else {
		meta.Notes = append(meta.Notes, "dbstat: "+err.Error())
	}
	meta.DBBytes = fileSize(r.src)
	log.Printf("realdb: %d messages, %d threads, %d accounts, %d views; targets read in %s", meta.Messages, meta.Threads, len(meta.Accounts), len(meta.Views), time.Since(t0).Round(time.Millisecond))
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(out, in, make([]byte, 4<<20)); err != nil {
		out.Close()
		return err
	}
	fadviseDontNeed(in) // the source's pages must not stay cached as the copy's
	return out.Close()
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(t, 0o700)
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		return copyFile(p, t)
	})
}

// addressRe matches an e-mail address, for the backend log lines an error
// shows in the real-database mode.
var addressRe = regexp.MustCompile(`[^\s<>"'@:,;()]+@[^\s<>"'@:,;()]+`)

// redactLogs is set in the real-database mode: log tails lose addresses.
var redactLogs bool

func redact(s string) string {
	if !redactLogs {
		return s
	}
	return addressRe.ReplaceAllString(s, "<address>")
}

// realConfig checks and completes the flags of the real-database mode.
// Its parts are open, mem, hot, idle, contention and window: the
// pressured parts need a cgroup the isolated run does not get, the sync
// part measures an empty database, and the web part would put a real
// body in a page.
func realConfig(c *runConfig) error {
	if fi, err := os.Stat(c.db); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("-db %s: not a database file (%v)", c.db, err)
	}
	if c.scratch == "" {
		return fmt.Errorf("-db needs -scratch: the directory its one copy lives in")
	}
	if fi, err := os.Stat(c.scratch); err != nil || !fi.IsDir() {
		return fmt.Errorf("-scratch %s: not a directory (%v)", c.scratch, err)
	}
	if c.native == "" {
		if d, err := os.UserCacheDir(); err == nil {
			c.native = filepath.Join(d, "mygo")
		}
	}
	if _, err := os.Stat(c.native); err != nil {
		return fmt.Errorf("-native-cache %s: %v (the backends cannot download mygo's SQLite without network)", c.native, err)
	}
	if len(c.only) == 0 {
		c.only = map[string]bool{"open": true, "mem": true, "hot": true, "idle": true, "contention": true, "window": true}
	}
	for _, p := range []string{"pressure", "sync", "web"} {
		if c.only[p] {
			log.Printf("-db: the %s part is skipped on a real database", p)
			delete(c.only, p)
		}
	}
	c.pressure = "off"
	if c.statusAccounts < 0 {
		c.statusAccounts = 3
	}
	redactLogs = true
	return nil
}
