//go:build linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/store"
)

// genVersion changes when the generator makes different mail: a fixture
// of another version is generated again, beside the old one (delete it).
const genVersion = 1

// fixtureMeta is what the generator wrote, for the runs: where things are
// and what the hot paths open.
type fixtureMeta struct {
	Name        string             `json:"name"`
	Schema      int                `json:"schema"` // len(store.Migrations)
	Generator   int                `json:"generator"`
	Scale       float64            `json:"scale"`
	Seed        uint64             `json:"seed"`
	CreatedAt   time.Time          `json:"created_at"`
	GenSeconds  float64            `json:"gen_seconds"`
	Messages    int                `json:"messages"`
	Threads     int                `json:"threads"`
	DBBytes     int64              `json:"db_bytes"`
	DirBytes    int64              `json:"dir_bytes"`
	Accounts    []metaAccount      `json:"accounts"`
	BigThread   int64              `json:"big_thread"`   // the thread with the most messages
	BigThreadN  int                `json:"big_thread_n"` // its messages
	BigMessage  int64              `json:"big_message"`  // the largest HTML body
	TypMessage  int64              `json:"typ_message"`  // a median-sized personal message
	TopLabel    int64              `json:"top_label"`    // the label on the most messages
	SearchTerms []string           `json:"search_terms"` // what the search hot path asks
	KeyringFile string             `json:"keyring_file"` // relative to the fixture dir
	KeyringKey  string             `json:"keyring_key"`  // likewise
	DataDir     string             `json:"data_dir"`     // likewise
	Unreachable string             `json:"unreachable"`  // where the accounts' servers point now
	Views       []metaView         `json:"views"`
	TableMB     map[string]float64 `json:"table_mb"` // the largest tables and indexes (dbstat), MB         // the saved views, Franklin's three included
	Notes       []string           `json:"notes,omitempty"`
}

type metaView struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type metaAccount struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Messages int    `json:"messages"`
	Inbox    int64  `json:"inbox"`
	Archive  int64  `json:"archive"`
	Sent     int64  `json:"sent"`
}

func fixtureName(scale float64) string {
	n := fmt.Sprintf("fixture-v%d-g%d", len(store.Migrations), genVersion)
	if scale != 1 {
		n += fmt.Sprintf("-s%g", scale)
	}
	return n
}

func readMeta(dir string) (fixtureMeta, error) {
	var m fixtureMeta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// ensureFixture returns the fixture for scale under root, generating it
// when missing. Generation takes the whole run's time budget once.
func ensureFixture(ctx context.Context, root string, scale float64, seed uint64, godwitBin string) (string, fixtureMeta, error) {
	dir := filepath.Join(root, fixtureName(scale))
	if m, err := readMeta(dir); err == nil {
		return dir, m, nil
	}
	log.Printf("fixture: %s is missing, generating it (scale %g)", dir, scale)
	m, err := generateFixture(ctx, dir, scale, seed, godwitBin)
	return dir, m, err
}

func cmdFixture(ctx context.Context, args []string) error {
	fl := flag.NewFlagSet("fixture", flag.ExitOnError)
	root := fl.String("root", defaultRoot(), "where fixtures live (one shared copy)")
	scale := fl.Float64("scale", 1, "fraction of the 45,000-message shape")
	seed := fl.Uint64("seed", 716, "generator seed")
	godwit := fl.String("godwit", "", "godwit binary to open the fixture once after generation (settles first-open work)")
	force := fl.Bool("force", false, "generate again even if it exists")
	fl.Parse(args)
	debug.SetMemoryLimit(2 << 30) // the IMAP server holds an account's mail (~0.7 GB for the largest) beside the sync
	dir := filepath.Join(*root, fixtureName(*scale))
	if *force {
		os.RemoveAll(dir)
	}
	dir, m, err := ensureFixture(ctx, *root, *scale, *seed, *godwit)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	fmt.Printf("%s\n%s\n", dir, b)
	return nil
}

func generateFixture(ctx context.Context, dir string, scale float64, seed uint64, godwitBin string) (fixtureMeta, error) {
	start := time.Now()
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	data := filepath.Join(tmp, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		return fixtureMeta{}, err
	}
	os.Setenv("GODWIT_OFFLINE_DEFAULTS", "1") // the network privacy switches start off: nothing reaches out
	kr := newMemKeyring()
	svc, err := core.New(ctx, core.Options{DataDir: data, Keyring: kr, SystemLanguage: "fr"}) // a language seeds the preset labels the views name
	if err != nil {
		return fixtureMeta{}, err
	}
	closed := false
	defer func() {
		if !closed {
			svc.Close()
		}
	}()
	st, err := svc.GetSettings(ctx)
	if err != nil {
		return fixtureMeta{}, err
	}
	st.AutoLabel = false
	if _, err := svc.SetSettings(ctx, st); err != nil {
		return fixtureMeta{}, err
	}
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	plans := accountPlans(scale)
	meta := fixtureMeta{Name: filepath.Base(dir), Schema: len(store.Migrations), Generator: genVersion, Scale: scale, Seed: seed, CreatedAt: time.Now().UTC()}
	total := 0
	for i, plan := range plans {
		t0 := time.Now()
		srv, nmsg, raw, err := startLoopMail(plan.Email, plan.Extra, func(emit func(genMsg)) { genAccount(seed, i, plan, now, emit) })
		if err != nil {
			return meta, err
		}
		acc, err := svc.AddAccount(ctx, core.AccountInput{
			Email: plan.Email, Name: fmt.Sprintf("Bench User %d", i+1), Username: plan.Email, Password: benchPassword,
			IsDefault: i == 0, Sync: &core.SyncRange{},
			Receiving: core.Server{Protocol: "imap", Host: srv.IMAPHost, Port: srv.IMAPPort, Security: core.SecurityNone},
			Sending:   core.Server{Protocol: "smtp", Host: srv.IMAPHost, Port: srv.SMTPPort, Security: core.SecurityNone},
		})
		if err != nil {
			srv.Close()
			return meta, fmt.Errorf("add %s: %w", plan.Email, err)
		}
		got := 0
		for round := 0; round < 50; round++ {
			if err := svc.SyncNow(ctx, acc.ID); err != nil {
				srv.Close()
				return meta, fmt.Errorf("sync %s: %w", plan.Email, err)
			}
			if got, err = countMessages(ctx, svc, acc.ID); err != nil {
				srv.Close()
				return meta, err
			}
			if got >= nmsg {
				break
			}
			log.Printf("fixture: %s round %d: %d/%d", plan.Email, round+1, got, nmsg)
		}
		if err := labelSome(ctx, svc, acc.ID); err != nil {
			log.Printf("fixture: labels on %s: %v", plan.Email, err)
		}
		if err := svc.SyncNow(ctx, acc.ID); err != nil { // pushes the labels' keyword ops, if any
			log.Printf("fixture: final sync %s: %v", plan.Email, err)
		}
		srv.Close()
		total += got
		el := time.Since(t0)
		log.Printf("fixture: account %d/%d %s: %d messages (%.0f MB raw) in %s, %.0f msg/s", i+1, len(plans), plan.Email, got, float64(raw)/1e6, el.Round(time.Second), float64(got)/el.Seconds())
	}
	if err := addDrafts(ctx, svc); err != nil {
		log.Printf("fixture: drafts: %v", err)
	}
	if err := addViews(ctx, svc); err != nil {
		return meta, fmt.Errorf("views: %w", err)
	}
	if err := svc.Close(); err != nil {
		return meta, err
	}
	closed = true
	if err := kr.save(filepath.Join(tmp, "keyring.bin"), filepath.Join(tmp, "keyring.key")); err != nil {
		return meta, err
	}
	meta.KeyringFile, meta.KeyringKey, meta.DataDir = "keyring.bin", "keyring.key", "data"
	meta.Unreachable = "127.0.0.1:1 (connection refused)"
	if err := finishDB(ctx, filepath.Join(data, "godwit.db"), &meta); err != nil {
		return meta, err
	}
	if godwitBin != "" { // the build's own first open: later runs start from a settled database
		if err := openOnce(ctx, godwitBin, tmp, meta); err != nil {
			return meta, fmt.Errorf("first open: %w", err)
		}
	}
	meta.GenSeconds = time.Since(start).Seconds()
	meta.DBBytes = fileSize(filepath.Join(data, "godwit.db"))
	meta.DirBytes = dirSize(tmp)
	b, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "meta.json"), b, 0o644); err != nil {
		return meta, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return meta, err
	}
	log.Printf("fixture: %s ready: %d messages, db %.0f MB, %s", dir, meta.Messages, float64(meta.DBBytes)/1e6, time.Since(start).Round(time.Second))
	return meta, nil
}

func countMessages(ctx context.Context, svc *core.Service, accountID int64) (int, error) {
	fs, err := svc.ListFolders(ctx, accountID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range fs {
		n += f.Total
	}
	return n, nil
}

// labelSome puts the built-in labels on matching mail by hand, as a user
// sorting mail would: bills on receipts, newsletter on newsletters, travel
// on trips; about one message in ten.
func labelSome(ctx context.Context, svc *core.Service, accountID int64) error {
	labels, err := svc.ListLabels(ctx)
	if err != nil {
		return err
	}
	bySlug := map[string]int64{}
	for _, l := range labels {
		bySlug[l.Slug] = l.ID
	}
	folders, err := svc.ListFolders(ctx, accountID)
	if err != nil {
		return err
	}
	want := map[string][]int64{}
	for _, f := range folders {
		if f.Role == "trash" || f.Role == "junk" {
			continue
		}
		cursor := ""
		for page := 0; page < 40; page++ {
			p, err := svc.ListMessages(ctx, core.View{FolderID: f.ID}, cursor, 200)
			if err != nil {
				return err
			}
			for k, m := range p.Items {
				s := strings.ToLower(m.Subject)
				switch {
				case strings.Contains(s, "receipt") || strings.Contains(s, "invoice"):
					want["bill"] = append(want["bill"], m.ID)
				case strings.Contains(s, "shipped"):
					want["order"] = append(want["order"], m.ID)
				case strings.Contains(s, "flight") || strings.Contains(s, "lisbon"):
					want["travel"] = append(want["travel"], m.ID)
				case strings.Contains(m.From, "weekly") && k%3 == 0:
					want["newsletter"] = append(want["newsletter"], m.ID)
				case strings.Contains(s, "invitation"):
					want["calendar"] = append(want["calendar"], m.ID)
				}
			}
			if p.NextCursor == "" {
				break
			}
			cursor = p.NextCursor
		}
	}
	for slug, ids := range want {
		id, ok := bySlug[slug]
		if !ok {
			continue
		}
		for len(ids) > 0 {
			n := min(len(ids), 500)
			if err := svc.SetLabel(ctx, ids[:n], id, true); err != nil {
				return err
			}
			ids = ids[n:]
		}
	}
	return nil
}

// macViews are Franklin's three saved views, as their documents read on
// his Mac (2026-11-26), with his folder ids mapped onto the fixture's:
// "Pro" takes five folders across four accounts, "Perso" three across two.
func macViews(pro, perso []int64) []core.SavedView {
	unreadGroup := []core.ViewGroup{{ID: "unread", Name: "Non lus", Match: core.MatchAll,
		Conditions: []core.RuleCondition{{Field: core.FieldRead, Op: core.OpIs, Value: core.ViewReadUnread}}}}
	folders := func(ids []int64) []core.RuleCondition {
		var out []core.RuleCondition
		for _, id := range ids {
			out = append(out, core.RuleCondition{Field: core.FieldFolder, Op: core.OpIs, Value: fmt.Sprint(id)})
		}
		return out
	}
	return []core.SavedView{
		{Name: "Non lus", Icon: "mail-unread", Doc: core.SavedViewDoc{Match: core.MatchAll, Conditions: []core.RuleCondition{
			{Field: core.FieldFolder, Op: core.OpIs, Value: "inbox"},
			{Field: core.FieldRead, Op: core.OpIs, Value: core.ViewReadUnread},
			{Field: core.FieldLabel, Op: core.OpNotContains, Value: "otp"},
			{Field: core.FieldLabel, Op: core.OpNotContains, Value: "notice"},
			{Field: core.FieldLabel, Op: core.OpNotContains, Value: "newsletter"},
		}, Groups: []core.ViewGroup{}}},
		{Name: "Pro", Icon: "briefcase", Doc: core.SavedViewDoc{Match: core.MatchAny, Conditions: folders(pro), Groups: unreadGroup}},
		{Name: "Perso", Icon: "home", Doc: core.SavedViewDoc{Match: core.MatchAny, Conditions: folders(perso), Groups: unreadGroup}},
	}
}

func addViews(ctx context.Context, svc *core.Service) error {
	accs, err := svc.ListAccounts(ctx)
	if err != nil {
		return err
	}
	byName := map[[2]any]int64{} // (account index, folder name) -> id
	for i, a := range accs {
		fs, err := svc.ListFolders(ctx, a.ID)
		if err != nil {
			return err
		}
		for _, f := range fs {
			byName[[2]any{i, f.Name}] = f.ID
		}
	}
	get := func(i int, name string) int64 { return byName[[2]any{i, name}] }
	pro := []int64{get(1, "INBOX"), get(1, "Clients/Acme"), get(1, "Team"), get(2, "INBOX"), get(3, "INBOX")}
	perso := []int64{get(0, "INBOX"), get(0, "Family"), get(4, "INBOX")}
	for _, id := range append(slices.Clone(pro), perso...) {
		if id == 0 {
			return fmt.Errorf("a view's folder is missing: pro %v perso %v", pro, perso)
		}
	}
	for _, v := range macViews(pro, perso) {
		if _, err := svc.SaveView(ctx, v); err != nil {
			return fmt.Errorf("%s: %w", v.Name, err)
		}
	}
	return nil
}

// addDrafts leaves a dozen drafts, a few long, across the accounts.
func addDrafts(ctx context.Context, svc *core.Service) error {
	accs, err := svc.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for i := range 12 {
		acc := accs[i%len(accs)]
		body := strings.Repeat("<p>Draft paragraph with some words to send later, maybe.</p>", 5+i*20)
		if _, err := svc.SaveDraft(ctx, core.Draft{AccountID: acc.ID, Mode: "new", To: []string{fmt.Sprintf("someone%d@example.org", i)}, Subject: fmt.Sprintf("Draft %d", i+1), HTML: body}); err != nil {
			return err
		}
	}
	return nil
}

// finishDB points every account at a closed port so no run reaches a
// server, checkpoints the WAL into the file, and picks the hot paths'
// targets.
func finishDB(ctx context.Context, path string, meta *fixtureMeta) error {
	db, err := store.Open(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Execute(ctx, "UPDATE accounts SET recv_host = '127.0.0.1', recv_port = 1, send_host = '127.0.0.1', send_port = 1"); err != nil {
		return err
	}
	q := func(sql string, args ...any) ([][]any, error) {
		r, err := db.Query(ctx, sql, args...)
		return r.Values, err
	}
	rows, err := q("SELECT a.id, a.email, (SELECT count(*) FROM messages m WHERE m.account_id = a.id) FROM accounts a ORDER BY a.id")
	if err != nil {
		return err
	}
	for _, r := range rows {
		ma := metaAccount{ID: store.Int(r[0]), Email: store.Str(r[1]), Messages: int(store.Int(r[2]))}
		fr, err := q("SELECT id, role, name FROM folders WHERE account_id = ?", ma.ID)
		if err != nil {
			return err
		}
		for _, f := range fr {
			switch {
			case store.Str(f[1]) == "inbox":
				ma.Inbox = store.Int(f[0])
			case store.Str(f[1]) == "sent":
				ma.Sent = store.Int(f[0])
			case store.Str(f[1]) == "archive" || (ma.Archive == 0 && store.Str(f[2]) == "Archive"):
				ma.Archive = store.Int(f[0])
			}
		}
		meta.Accounts = append(meta.Accounts, ma)
		meta.Messages += ma.Messages
	}
	one := func(sql string) int64 {
		r, err := q(sql)
		if err != nil || len(r) == 0 {
			if err != nil {
				meta.Notes = append(meta.Notes, fmt.Sprintf("%s: %v", sql, err))
			}
			return 0
		}
		return store.Int(r[0][0])
	}
	meta.Threads = int(one("SELECT count(*) FROM threads"))
	if r, err := q("SELECT id, name FROM saved_views ORDER BY position, id"); err == nil {
		for _, v := range r {
			meta.Views = append(meta.Views, metaView{store.Int(v[0]), store.Str(v[1])})
		}
	}
	if r, err := q("SELECT thread_id, count(*) FROM messages GROUP BY thread_id ORDER BY 2 DESC LIMIT 1"); err == nil && len(r) > 0 {
		meta.BigThread, meta.BigThreadN = store.Int(r[0][0]), int(store.Int(r[0][1]))
	}
	meta.BigMessage = one("SELECT id FROM messages ORDER BY length(body_html) DESC LIMIT 1")
	meta.TypMessage = one("SELECT id FROM messages WHERE body_html IS NOT NULL ORDER BY abs(length(body_html) - 6000), id LIMIT 1")
	meta.TopLabel = one("SELECT label_id FROM message_labels WHERE active = 1 GROUP BY label_id ORDER BY count(*) DESC LIMIT 1")
	// a common word, a made-up one, a surname, a subject phrase, an address part
	meta.SearchTerms = []string{"projet", words[len(words)-7], "Martin", "Flight to Lisbon", "github"}
	meta.TableMB = map[string]float64{}
	if r, err := q("SELECT name, sum(pgsize) FROM dbstat GROUP BY name ORDER BY 2 DESC LIMIT 12"); err == nil {
		for _, t := range r {
			meta.TableMB[store.Str(t[0])] = float64(store.Int(t[1])) / 1e6
		}
	} else {
		meta.Notes = append(meta.Notes, "dbstat: "+err.Error())
		if r, err := q("SELECT sum(length(body_html)), sum(length(body_text)), sum(length(snippet)) FROM messages"); err == nil && len(r) > 0 {
			meta.TableMB["messages.body_html (bytes)"] = float64(store.Int(r[0][0])) / 1e6
			meta.TableMB["messages.body_text (bytes)"] = float64(store.Int(r[0][1])) / 1e6
		}
	}
	if _, err := db.Query(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return err
	}
	return nil
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func dirSize(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}
