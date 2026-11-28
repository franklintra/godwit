package core

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/godwit-email/godwit/core/mailsync"
	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// The folder, unread and starred counts read through messages_unseen and
// message_locations only, and the unified lists walk threads_last_date.
// These tests hold them to the numbers and pages of the queries they
// replaced, on a store with every case those queries handle: read, unread
// and starred mail, a message in several folders (Gmail) and hidden from
// some, deletes waiting or done, server copies of drafts (ADR 0047) by
// local row and by Message-ID, an account with none, Trash and Junk.

// Before messages_unseen: per folder, two correlated counts over every
// location's message row.
const oldFolderCounts = `SELECT f.id,
	(SELECT count(*) FROM messages m JOIN message_locations l ON l.message_id = m.id AND l.folder_id = f.id AND l.shown
		WHERE instr(m.flags, '\Seen') = 0 AND ` + notPurgedCond + `),
	(SELECT count(*) FROM messages m JOIN message_locations l ON l.message_id = m.id AND l.folder_id = f.id AND l.shown WHERE ` + notPurgedCond + `)
	FROM folders f WHERE f.account_id = ? ORDER BY f.id`

const oldStarredCounts = `SELECT account_id, count(*) FROM messages m
	WHERE instr(flags, '\Flagged') > 0 AND instr(flags, '\Seen') = 0 AND ` + notPurgedCond + ` GROUP BY account_id`

// countsStore is a service holding a random mailbox of three accounts:
// 1 Gmail-like (a message in up to three folders) with server drafts on
// and copies of drafts, 2 without any server draft, 3 with server drafts
// on but no copy.
func countsStore(t *testing.T, seed uint64) *Service {
	t.Helper()
	keyring.MockInit()
	s, err := New(t.Context(), Options{DataDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	r := rand.New(rand.NewPCG(seed, 7))
	exec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := s.db.Execute(t.Context(), q, args...)
		require.NoError(t, err, q)
		return res.LastInsertID
	}
	exec(`INSERT INTO accounts(id, email, server_drafts) VALUES (1, 'a@x', 1), (2, 'b@x', 0), (3, 'c@x', 1)`)
	roles := []string{"inbox", "sent", "drafts", "trash", "junk", "archive", "", "", ""}
	folders := map[int64][]int64{}
	for acc := int64(1); acc <= 3; acc++ {
		for i, role := range roles {
			id := exec(`INSERT INTO folders(account_id, name, role) VALUES (?, ?, ?)`, acc, fmt.Sprintf("F%d-%d", acc, i), role)
			folders[acc] = append(folders[acc], id)
		}
		if acc == 1 { // Gmail's Starred and All Mail
			folders[acc] = append(folders[acc],
				exec(`INSERT INTO folders(account_id, name, role) VALUES (1, '[Gmail]/Starred', 'flagged')`),
				exec(`INSERT INTO folders(account_id, name, role) VALUES (1, '[Gmail]/All Mail', 'all')`))
		}
		exec(`INSERT INTO folders(account_id, name, role) VALUES (?, 'Empty', '')`, acc)
	}
	flagSets := []string{"", `\Seen`, `\Flagged`, `\Seen \Flagged`, `\Answered`, `\Seen \Answered`, `\Flagged \Answered`}
	type msg struct {
		id, acc int64
		mid     string
	}
	var msgs []msg
	for acc := int64(1); acc <= 3; acc++ {
		var threads []int64
		for i := range 40 {
			var last any = 1_700_000_000 + r.Int64N(30)*3600 // ties on purpose
			if i == 0 {
				last = nil
			}
			threads = append(threads, exec(`INSERT INTO threads(account_id, subject, last_date) VALUES (?, ?, ?)`, acc, fmt.Sprint("t", i), last))
		}
		for i := range 160 {
			mid := fmt.Sprintf("<%d.%d@x>", acc, i)
			if r.IntN(10) == 0 {
				mid = "<shared@x>" // the same Message-ID in several messages and accounts
			}
			date := 1_700_000_000 + r.Int64N(30)*3600
			id := exec(`INSERT INTO messages(account_id, thread_id, message_id, subject, from_addr, date, flags, label_fp)
				VALUES (?, ?, ?, 's', 'p@x', ?, ?, ?)`, acc, threads[r.IntN(len(threads))], mid, date, flagSets[r.IntN(len(flagSets))], mid)
			msgs = append(msgs, msg{id, acc, mid})
			n := 1
			if acc == 1 {
				n = 1 + r.IntN(3)
			}
			fs := folders[acc]
			for _, k := range r.Perm(len(fs))[:n] {
				shown := 1
				if r.IntN(5) == 0 {
					shown = 0
				}
				exec(`INSERT INTO message_locations(message_id, folder_id, shown, entered_at, date) VALUES (?, ?, ?, 0, ?)`, id, fs[k], shown, date)
			}
			switch r.IntN(12) {
			case 0:
				exec(`INSERT INTO ops(account_id, message_id, kind, state) VALUES (?, ?, 'purge', 'pending')`, acc, id)
			case 1:
				exec(`INSERT INTO ops(account_id, message_id, kind, state) VALUES (?, ?, 'purge', 'done')`, acc, id)
			case 2:
				exec(`INSERT INTO ops(account_id, message_id, kind, state) VALUES (?, ?, 'move', 'pending')`, acc, id)
			}
		}
	}
	sd := func(acc int64, mid string, local any) {
		exec(`INSERT INTO server_drafts(account_id, token, message_id_hdr, message_id, revision, folder, state, created_at)
			VALUES (?, 'tok', ?, ?, 1, 'Drafts', 'stored', 0)`, acc, mid, local)
	}
	for _, m := range msgs {
		if m.acc != 1 {
			continue
		}
		switch r.IntN(15) {
		case 0:
			sd(1, "", m.id) // a copy taken over: by its local row
		case 1:
			sd(1, m.mid, nil) // a copy Godwit stored: by its Message-ID
		}
	}
	sd(1, "<2.3@x>", nil)    // another account's Message-ID: not a copy there
	sd(1, "", nil)           // a copy whose message the sync dropped
	sd(1, "<shared@x>", nil) // every account-1 message with this Message-ID
	for _, m := range msgs {
		if m.acc == 2 {
			sd(1, "", m.id) // a row naming another account's message: no match
			break
		}
	}
	return s
}

func TestFolderCountsMatchOldQueries(t *testing.T) {
	for seed := range uint64(6) {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			s := countsStore(t, seed)
			for acc := int64(1); acc <= 3; acc++ {
				old, err := s.db.Query(t.Context(), oldFolderCounts, acc)
				require.NoError(t, err)
				fs, err := s.ListFolders(t.Context(), acc)
				require.NoError(t, err)
				require.Len(t, fs, len(old.Values))
				nonzero := 0
				for i, f := range fs {
					want := old.Values[i]
					require.Equal(t, store.Int(want[0]), f.ID)
					assert.Equal(t, int(store.Int(want[1])), f.Unread, "account %d folder %d (%s) unread", acc, f.ID, f.Name)
					assert.Equal(t, int(store.Int(want[2])), f.Total, "account %d folder %d (%s) total", acc, f.ID, f.Name)
					nonzero += f.Unread
				}
				assert.Positive(t, nonzero, "account %d has unread mail", acc)
			}
			old, err := s.db.Query(t.Context(), oldStarredCounts)
			require.NoError(t, err)
			want, wantTotal := map[int64]int{}, 0
			for _, r := range old.Values {
				want[store.Int(r[0])] = int(store.Int(r[1]))
				wantTotal += int(store.Int(r[1]))
			}
			got, total, err := s.StarredCounts(t.Context())
			require.NoError(t, err)
			assert.Equal(t, want, got)
			assert.Equal(t, wantTotal, total)
			assert.Positive(t, total)
		})
	}
}

// A draft's server copy is never counted, and a delete that waits leaves
// the counts at once: the counts of one folder, by hand.
func TestFolderCountsLeaveOutCopiesAndPurges(t *testing.T) {
	keyring.MockInit()
	s, err := New(t.Context(), Options{DataDir: t.TempDir()})
	require.NoError(t, err)
	defer s.Close()
	for _, q := range []string{
		`INSERT INTO accounts(id, email, server_drafts) VALUES (1, 'a@x', 1)`,
		`INSERT INTO folders(id, account_id, name, role) VALUES (10, 1, 'Drafts', 'drafts')`,
		`INSERT INTO threads(id, account_id, last_date) VALUES (1, 1, 1)`,
		`INSERT INTO messages(id, account_id, thread_id, message_id, flags) VALUES (1, 1, 1, '<a@x>', ''), (2, 1, 1, '<b@x>', ''),
			(3, 1, 1, '<c@x>', '\Seen'), (4, 1, 1, '<d@x>', ''), (5, 1, 1, '<e@x>', '')`,
		`INSERT INTO message_locations(message_id, folder_id, shown, entered_at) VALUES (1, 10, 1, 0), (2, 10, 1, 0), (3, 10, 1, 0), (4, 10, 1, 0), (5, 10, 0, 0)`,
		`INSERT INTO server_drafts(account_id, token, message_id_hdr, message_id, revision, folder, state, created_at)
			VALUES (1, 't', '<a@x>', NULL, 1, 'Drafts', 'stored', 0), (1, 't', '', 2, 1, 'Drafts', 'stored', 0)`,
		`INSERT INTO ops(account_id, message_id, kind, state) VALUES (1, 4, 'purge', 'pending')`,
	} {
		_, err := s.db.Execute(t.Context(), q)
		require.NoError(t, err, q)
	}
	fs, err := s.ListFolders(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, fs, 1)
	assert.Equal(t, 1, fs[0].Total, "message 3 only: 1 and 2 are copies, 4 waits for its delete, 5 is hidden")
	assert.Zero(t, fs[0].Unread)
}

func TestLocPurgeWaitsIsPurgeWaits(t *testing.T) {
	assert.Equal(t, strings.Replace(mailsync.PurgeWaits, "o.message_id = m.id", "o.message_id = l.message_id", 1), locPurgeWaits)
}

// threadPages walks the view's pages of 7.
func threadPages(t *testing.T, s *Service, v View) []ThreadPage {
	t.Helper()
	var pages []ThreadPage
	cursor := ""
	for range 100 {
		p, err := s.ListThreads(t.Context(), v, cursor, 7)
		require.NoError(t, err)
		pages = append(pages, p)
		if p.NextCursor == "" {
			return pages
		}
		cursor = p.NextCursor
	}
	t.Fatal("no last page")
	return nil
}

func TestListThreadsSameWithoutDateIndex(t *testing.T) {
	s := countsStore(t, 3)
	views := []View{{Unified: "inbox"}, {Unified: "starred"}, {Unified: "starred:1"}, {FolderID: 1}}
	with := map[int][]ThreadPage{}
	for i, v := range views {
		with[i] = threadPages(t, s, v)
		require.NotEmpty(t, with[i][0].Items, "%+v", v)
	}
	assert.Greater(t, len(with[0]), 2, "the unified inbox spans pages")
	_, err := s.db.Execute(t.Context(), "DROP INDEX threads_last_date")
	require.NoError(t, err)
	for i, v := range views {
		assert.Equal(t, with[i], threadPages(t, s, v), "%+v", v)
	}
}

// plan is the query's EXPLAIN QUERY PLAN, one step per line.
func plan(t *testing.T, s *Service, q string, args ...any) string {
	t.Helper()
	rows, err := s.db.Query(t.Context(), "EXPLAIN QUERY PLAN "+q, args...)
	require.NoError(t, err)
	var lines []string
	for _, r := range rows.Values {
		lines = append(lines, store.Str(r[len(r)-1]))
	}
	return strings.Join(lines, "\n")
}

// listThreadsSQL is ListThreads's query for the view, first page or a
// cursor page.
func listThreadsSQL(t *testing.T, v View, cursor bool) (string, []any) {
	cond, args, err := viewCond(v)
	require.NoError(t, err)
	where := "EXISTS (SELECT 1 FROM messages m WHERE m.thread_id = t.id AND " + cond + ")"
	if cursor {
		where += " AND (t.last_date < ? OR (t.last_date = ? AND t.id < ?))"
		args = append(args, 1_700_050_000, 1_700_050_000, 99)
	}
	return "SELECT t.id, t.subject, t.last_date FROM threads t WHERE " + where + " ORDER BY t.last_date DESC, t.id DESC LIMIT ?", append(args, 51)
}

// The counts read unread mail through messages_unseen and the unified
// lists walk threads_last_date. Without sqlite_stat1 the planner decides
// on the schema alone, so the plans here are the plans on a large store.
func TestCountAndListPlans(t *testing.T) {
	s := countsStore(t, 1)
	p := plan(t, s, folderUnread, 1)
	t.Logf("ListFolders unread:\n%s", p)
	assert.Contains(t, p, "messages_unseen")
	p = plan(t, s, folderTotals, 1)
	t.Logf("ListFolders totals:\n%s", p)
	assert.Contains(t, p, "message_locations_list")
	assert.NotRegexp(t, `SCAN m\b`, p, "no scan of messages")
	p = plan(t, s, `SELECT m.account_id, count(*) FROM messages m
		WHERE instr(m.flags, '\Seen') = 0 AND instr(m.flags, '\Flagged') > 0 AND `+notPurgedCond+` GROUP BY m.account_id`)
	t.Logf("StarredCounts:\n%s", p)
	assert.Contains(t, p, "messages_unseen")
	for _, v := range []View{{Unified: "inbox"}, {Unified: "starred"}} {
		for _, cursor := range []bool{false, true} {
			q, args := listThreadsSQL(t, v, cursor)
			p := plan(t, s, q, args...)
			t.Logf("ListThreads %s, cursor %v:\n%s", v.Unified, cursor, p)
			assert.Contains(t, p, "threads_last_date")
			assert.NotContains(t, p, "TEMP B-TREE FOR ORDER BY")
		}
	}
}
