package store

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/egoist/mygo/plugins/sqlite"
)

// This file is the franklintra/godwit fork's only change to how the schema
// migrates (FORK.md). Upstream numbers its migrations by PRAGMA user_version;
// the fork never adds a numbered one, so a database it opened keeps
// upstream's number and a later upstream migration of the same number still
// runs. The fork's schema lives in ForkMigrations instead: additive steps
// that every Open applies when their object is missing, after the numbered
// migrations.

// ForkBase is the number of upstream migrations the fork's steps were
// written against: len(Migrations) on the upstream release the fork sits on.
// The steps run only on a database at that version or later, so tests that
// open an older schema (Migrations cut short) see upstream's alone.
// TestMigrationsAreUpstreams fails when len(Migrations) differs: a merged
// branch put a numbered migration in schema.go instead of here.
var ForkBase = 86

// ForkMigration is one fork feature's schema: statements that only add,
// each an ALTER TABLE … ADD COLUMN, a CREATE TABLE or a CREATE [UNIQUE]
// INDEX. Open runs those whose column, table or index is missing, so a step
// is never recorded and a database that lost the object (a downgrade, a
// rollback) gets it back. A step is never removed, even once upstream
// adopts its object: it is what lets upstream's numbered migration skip
// re-creating the object (see skipForkObjects).
type ForkMigration struct {
	Name  string // the feature, unique: "headers", "server-drafts"
	Stmts []string
}

// ForkMigrations are applied in order, after Migrations.
var ForkMigrations = []ForkMigration{{
	Name: "headers",
	Stmts: []string{
		// A draft's own header fields, sent after Godwit's: a JSON array of
		// {"Name","Value"} as mail.CheckHeaders left them, in order.
		`ALTER TABLE drafts ADD COLUMN headers TEXT NOT NULL DEFAULT '[]'`,
		// 1: the message asks for a read receipt to the sending account's own
		// address (Disposition-Notification-To).
		`ALTER TABLE drafts ADD COLUMN read_receipt INTEGER NOT NULL DEFAULT 0`,
	},
}, {
	Name: "server-drafts",
	Stmts: []string{
		// Server drafts (ADR 0047): 1 keeps the account's drafts in its Drafts
		// folder as well as here.
		`ALTER TABLE accounts ADD COLUMN server_drafts INTEGER NOT NULL DEFAULT 0`,
		// One row per copy of a draft in a Drafts folder that Godwit stored or
		// took over. draft_id NULL: its draft was sent or deleted, so the copy
		// goes. token is the draft's X-Godwit-Draft-ID, message_id_hdr the copy's
		// own Message-ID ('' if it has none) and message_id the local message of a
		// copy taken over from another app; either hides the copy from the mail
		// lists. state: appending (the APPEND may have gone through), stored,
		// failed (refused; tried again at the next revision), gone (another app
		// removed it; pushed again at the next revision) or removed (Godwit
		// deleted it; the row goes once the sync drops the local message). seen:
		// a sync stored the copy here once, so its absence means another app
		// removed it.
		`CREATE TABLE server_drafts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			draft_id INTEGER REFERENCES drafts(id) ON DELETE SET NULL,
			token TEXT NOT NULL,
			message_id_hdr TEXT NOT NULL DEFAULT '',
			message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
			revision INTEGER NOT NULL,
			folder TEXT NOT NULL,
			server_ref TEXT,
			state TEXT NOT NULL,
			seen INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL)`,
		`CREATE INDEX server_drafts_draft ON server_drafts(draft_id)`,
		`CREATE INDEX server_drafts_account ON server_drafts(account_id, message_id_hdr)`,
	},
}, {
	Name: "identities",
	Stmts: []string{
		// 发件身份 (ADR 0048): the other names and addresses an account sends as
		// (a Gmail send-as address, a shared mailbox, a second name on the same
		// address). signature_id NULL writes with the account's signature;
		// is_default marks the one new mail starts from, none the account's own.
		// A draft's identity_id NULL sends as the account itself.
		`CREATE TABLE identities (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL,
			reply_to TEXT NOT NULL DEFAULT '',
			signature_id INTEGER REFERENCES signatures(id) ON DELETE SET NULL,
			is_default INTEGER NOT NULL DEFAULT 0,
			UNIQUE(account_id, email, name))`,
		`ALTER TABLE drafts ADD COLUMN identity_id INTEGER REFERENCES identities(id) ON DELETE SET NULL`,
	},
}, {
	Name: "openpgp",
	Stmts: []string{
		// OpenPGP through the user's GnuPG (ADR 0050): the secret key an account
		// signs with (its fingerprint; '' none) and its defaults for new drafts,
		// and a draft's own choices, NULL while it follows its account's.
		// openpgp_encrypt is off, auto or on.
		`CREATE TABLE account_openpgp (
			account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			key_fpr TEXT NOT NULL DEFAULT '',
			sign INTEGER NOT NULL DEFAULT 0,
			encrypt INTEGER NOT NULL DEFAULT 0)`,
		`ALTER TABLE drafts ADD COLUMN openpgp_sign INTEGER`,
		`ALTER TABLE drafts ADD COLUMN openpgp_encrypt TEXT`,
	},
}, {
	Name: "plaintext",
	Stmts: []string{
		// A draft's format: 'html' (multipart/alternative) or 'plain' (text/plain,
		// format=flowed). An account's compose_format is what its new drafts
		// start in: '' follows Settings.ComposeFormat.
		`ALTER TABLE drafts ADD COLUMN format TEXT NOT NULL DEFAULT 'html'`,
		`ALTER TABLE accounts ADD COLUMN compose_format TEXT NOT NULL DEFAULT ''`,
	},
}, {
	Name: "perf-indexes",
	Stmts: []string{
		// Unread mail only: the folder, starred and view counts read their
		// rows through it, so they scale with unread mail, not all mail. A
		// query uses it only when its WHERE holds this term as written,
		// instr(m.flags, '\Seen') = 0 (COALESCE or another spelling does
		// not match). An existing database builds it once at its next
		// open: one scan of every message's flags.
		`CREATE INDEX messages_unseen ON messages(account_id, thread_id) WHERE instr(flags, '\Seen') = 0`,
		// Newest threads first across accounts: a unified list walks it and
		// stops at its page instead of sorting every thread.
		`CREATE INDEX threads_last_date ON threads(last_date DESC, id DESC)`,
	},
}, {
	Name: "scan-indexes",
	Stmts: []string{
		// Every message by sender: the contacts gathering (Find, the
		// Contacts page), a sender's From headers (a LIKE on from_addr,
		// which no index range serves: it scans this instead of the
		// table) and a contact's newest mail (ContactAvatar) read it
		// alone, not the message rows that inline bodies spread over
		// the whole file. id is spelled out after date so a sender's
		// mail of one date stays in id order, as a scan of the table
		// gave it: a bare column beside max(m.date) keeps the same row.
		// Building it reads every message's leading columns once.
		`CREATE INDEX messages_from ON messages(from_addr, date, id, account_id, flags)`,
		// The same for mail with a List-Unsubscribe header: a contact's
		// newest list mail. list_unsubscribe sits after the inline bodies,
		// so the test is octet_length, which SQLite answers from the row's
		// header without reading the value: building this reads no more
		// than messages_from. A query uses it only with this term as
		// written; the last column lets it answer the term from the index.
		`CREATE INDEX messages_from_list ON messages(from_addr, date, id, account_id, octet_length(list_unsubscribe)) WHERE octet_length(list_unsubscribe) > 0`,
		// Content-ID first: GROUP BY cid reads the index, not the images.
		`CREATE INDEX signature_images_cid ON signature_images(cid, content_type)`,
	},
}}

// schemaObject is what an additive statement creates.
type schemaObject struct {
	kind  string // "column", "table" or "index"
	table string // the column's table; "" for a table or an index
	name  string
}

func (o schemaObject) key() string {
	return o.kind + ":" + strings.ToLower(o.table) + "." + strings.ToLower(o.name)
}

var (
	addColumnRE   = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+"?(\w+)"?\s+ADD\s+(?:COLUMN\s+)?"?(\w+)"?`)
	createTableRE = regexp.MustCompile(`(?is)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)"?`)
	createIndexRE = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?"?(\w+)"?`)
)

// additiveObject is the object stmt creates, if it is one of the statements
// a ForkMigration may hold.
func additiveObject(stmt string) (schemaObject, bool) {
	if m := addColumnRE.FindStringSubmatch(stmt); m != nil {
		return schemaObject{"column", m[1], m[2]}, true
	}
	if m := createTableRE.FindStringSubmatch(stmt); m != nil {
		return schemaObject{"table", "", m[1]}, true
	}
	if m := createIndexRE.FindStringSubmatch(stmt); m != nil {
		return schemaObject{"index", "", m[1]}, true
	}
	return schemaObject{}, false
}

// CheckForkMigrations reports the first fork statement that is not
// additive, or a name used twice.
func CheckForkMigrations() error {
	names := map[string]bool{}
	for _, m := range ForkMigrations {
		if m.Name == "" || names[m.Name] {
			return fmt.Errorf("store: fork migration name %q empty or used twice", m.Name)
		}
		names[m.Name] = true
		for _, s := range m.Stmts {
			if _, ok := additiveObject(s); !ok {
				return fmt.Errorf("store: fork migration %s: not an ADD COLUMN, CREATE TABLE or CREATE INDEX: %.60s", m.Name, strings.TrimSpace(s))
			}
		}
	}
	return nil
}

func (db *DB) exists(ctx context.Context, o schemaObject) (bool, error) {
	var (
		q    string
		args []any
	)
	switch o.kind {
	case "column":
		q, args = "SELECT 1 FROM pragma_table_info(?) WHERE name = ? COLLATE NOCASE", []any{o.table, o.name}
	default:
		q, args = "SELECT 1 FROM sqlite_master WHERE type = ? AND name = ? COLLATE NOCASE", []any{o.kind, o.name}
	}
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return false, err
	}
	return len(rows.Values) > 0, nil
}

// applyFork runs the fork statements whose object is missing, one
// transaction per ForkMigration.
func (db *DB) applyFork(ctx context.Context) error {
	if len(Migrations) < ForkBase {
		return nil
	}
	if err := CheckForkMigrations(); err != nil {
		return err
	}
	for _, m := range ForkMigrations {
		var stmts []sqlite.Statement
		for _, s := range m.Stmts {
			o, _ := additiveObject(s)
			ok, err := db.exists(ctx, o)
			if err != nil {
				return err
			}
			if !ok {
				stmts = append(stmts, sqlite.Statement{SQL: s})
			}
		}
		if len(stmts) == 0 {
			continue
		}
		if _, err := db.Transaction(ctx, stmts); err != nil {
			return fmt.Errorf("store: fork migration %s: %w", m.Name, err)
		}
	}
	return db.ensureRewrites(ctx)
}

// skipForkObjects drops from an upstream migration the statements that
// create an object a fork step declares and the database already has:
// upstream adopted a fork feature under the same name, and its migration
// would otherwise fail on a column or table the fork made first. Objects no
// fork step declares are never skipped, so upstream's own migrations run
// exactly as upstream wrote them.
func (db *DB) skipForkObjects(ctx context.Context, version int, stmts []string) ([]string, error) {
	declared := map[string]bool{}
	for _, m := range ForkMigrations {
		for _, s := range m.Stmts {
			if o, ok := additiveObject(s); ok {
				declared[o.key()] = true
			}
		}
	}
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		if o, ok := additiveObject(s); ok && declared[o.key()] {
			has, err := db.exists(ctx, o)
			if err != nil {
				return nil, err
			}
			if has {
				slog.Default().Info("skipping a migration statement the fork already applied", "version", version, "object", o.key())
				continue
			}
		}
		out = append(out, s)
	}
	return out, nil
}
