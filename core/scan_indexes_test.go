package core

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// The contacts gathering, a sender's From headers, a contact's newest
// mail and the signature images read the indexes of the fork step
// scan-indexes instead of the message rows. These tests hold them to the
// rows of the queries and plans they replaced, on a store with what those
// handle: one address under several From headers, a header in several
// accounts, mail of one sender on one date (ties for the bare column
// beside max), list mail with and without List-Unsubscribe (NULL, ''),
// hidden locations, Sent, Drafts, Trash and Junk, a non-ASCII domain.

var scanHeaders = []any{
	"Ann <ann@x.com>", "ann@x.com", `"Ann B" <ANN@x.com>`, "News <news@list.example>", "news@list.example",
	"Bob <bob@y.org>", "", nil, "Shop <shop@z.net>", "Zoë <zoe@exämple.de>", "Carl <carl@x.com>", "a@x",
}

// scanStore is a service holding a random mailbox of three accounts.
func scanStore(t *testing.T, seed uint64) *Service {
	t.Helper()
	keyring.MockInit()
	s, err := New(t.Context(), Options{DataDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	r := rand.New(rand.NewPCG(seed, 11))
	exec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := s.db.Execute(t.Context(), q, args...)
		require.NoError(t, err, q)
		return res.LastInsertID
	}
	exec(`INSERT INTO accounts(id, email) VALUES (1, 'a@x'), (2, 'b@x'), (3, 'c@x')`)
	roles := []string{"inbox", "sent", "drafts", "trash", "junk", "archive", "", ""}
	lists := []any{nil, "", "<mailto:u@list.example>"}
	flags := []string{"", `\Seen`, `\Flagged`, `\Seen \Flagged`}
	for acc := int64(1); acc <= 3; acc++ {
		var folders []int64
		for i, role := range roles {
			folders = append(folders, exec(`INSERT INTO folders(account_id, name, role) VALUES (?, ?, ?)`, acc, fmt.Sprintf("F%d-%d", acc, i), role))
		}
		thread := exec(`INSERT INTO threads(account_id, subject, last_date) VALUES (?, 's', 0)`, acc)
		for range 150 {
			from := scanHeaders[r.IntN(len(scanHeaders))]
			to := fmt.Sprintf(`["%s"]`, []string{"Ann <ann@x.com>", "bob@y.org", "Dee <dee@w.io>"}[r.IntN(3)])
			id := exec(`INSERT INTO messages(account_id, thread_id, subject, from_addr, to_addrs, cc_addrs, date, flags, list_unsubscribe)
				VALUES (?, ?, 's', ?, ?, '[]', ?, ?, ?)`, acc, thread, from, to,
				1_700_000_000+r.Int64N(12)*86400, flags[r.IntN(len(flags))], lists[r.IntN(len(lists))])
			for _, k := range r.Perm(len(folders))[:1+r.IntN(2)] {
				exec(`INSERT INTO message_locations(message_id, folder_id, shown, entered_at) VALUES (?, ?, ?, 0)`, id, folders[k], r.IntN(5))
			}
		}
	}
	for sig := int64(1); sig <= 3; sig++ {
		exec(`INSERT INTO signatures(id, name) VALUES (?, ?)`, sig, fmt.Sprint("sig", sig))
		for _, cid := range []string{"logo@x", fmt.Sprintf("own%d@x", sig), "banner@x"} {
			exec(`INSERT INTO signature_images(signature_id, cid, content_type, data) VALUES (?, ?, 'image/png', ?)`, sig, cid, []byte(cid))
		}
	}
	return s
}

// oldContactQueries are contacts' statements before scan-indexes.
func oldContactQueries(left, sent []any, accountID int64) contactsSQL {
	acc, accArgs := "", []any(nil)
	if accountID != 0 {
		acc, accArgs = " AND m.account_id = ?", []any{accountID}
	}
	from := " FROM messages m WHERE " + shownFolder + " IS NOT NULL AND NOT " + shownIn("("+placeholders(len(left))+")") + acc
	return contactsSQL{
		received: `SELECT m.from_addr, count(*), sum(instr(m.flags, '\Seen') = 0), sum(` + shownIn("(SELECT id FROM folders WHERE role = 'inbox')") + `), max(m.date), m.id` +
			from + " GROUP BY m.from_addr ORDER BY 5, 1",
		lists:    "SELECT m.from_addr, max(m.date), m.id" + from + " AND m.list_unsubscribe <> '' GROUP BY m.from_addr",
		ours:     "SELECT m.to_addrs, m.cc_addrs, m.date FROM messages m WHERE " + shownIn("("+placeholders(len(sent))+")") + acc + " ORDER BY m.date",
		args:     append(slices.Clone(left), accArgs...),
		oursArgs: append(slices.Clone(sent), accArgs...),
	}
}

func dropScanIndexes(t *testing.T, s *Service) {
	t.Helper()
	for _, ix := range []string{"messages_from", "messages_from_list", "signature_images_cid"} {
		_, err := s.db.Execute(t.Context(), "DROP INDEX "+ix)
		require.NoError(t, err)
	}
}

func queryRows(t *testing.T, s *Service, q string, args []any) [][]any {
	t.Helper()
	rows, err := s.db.Query(t.Context(), q, args...)
	require.NoError(t, err, q)
	return rows.Values
}

// scanCase is what one store answers through the code under test.
type scanCase struct {
	contacts  map[int64][]Contact
	sql       map[string][][]any
	headers   map[string][]string
	newest    map[string][][]any
	sigImages []string
	sigTypes  []string
}

func readScanCase(t *testing.T, s *Service, queries func(left, sent []any, acc int64) contactsSQL) scanCase {
	t.Helper()
	ctx := t.Context()
	c := scanCase{contacts: map[int64][]Contact{}, sql: map[string][][]any{}, headers: map[string][]string{}, newest: map[string][][]any{}}
	left, err := s.roleFolderIDs(ctx, "sent", "drafts", "trash", "junk")
	require.NoError(t, err)
	sent, err := s.roleFolderIDs(ctx, "sent")
	require.NoError(t, err)
	for _, acc := range []int64{0, 1, 2} {
		got, err := s.contacts(ctx, acc)
		require.NoError(t, err)
		c.contacts[acc] = got
		q := queries(left, sent, acc)
		c.sql[fmt.Sprint("received", acc)] = queryRows(t, s, q.received, q.args)
		c.sql[fmt.Sprint("lists", acc)] = queryRows(t, s, q.lists, q.args)
		c.sql[fmt.Sprint("ours", acc)] = queryRows(t, s, q.ours, q.oursArgs)
	}
	for _, key := range []string{"ann@x.com", "news@list.example", "zoe@exämple.de", "nobody@x.com"} {
		q, err := s.withSenders(ctx, SearchQuery{FromAddr: key})
		require.NoError(t, err)
		slices.Sort(q.fromHeaders) // an IN list: its order is not used
		c.headers[key] = q.fromHeaders
		c.newest[key] = queryRows(t, s, newestFromSQL(len(left)), append([]any{jsonStrings(q.fromHeaders)}, left...))
	}
	hs, err := s.fromHeadersForAddrs(ctx, []string{"ann@x.com", "bob@y.org"})
	require.NoError(t, err)
	slices.Sort(hs)
	c.headers["ann+bob"] = hs
	imgs, err := s.signatureImages(ctx, false)
	require.NoError(t, err)
	for cid, a := range imgs {
		c.sigImages = append(c.sigImages, cid)
		c.sigTypes = append(c.sigTypes, cid+" "+a.ContentType)
	}
	slices.Sort(c.sigImages)
	slices.Sort(c.sigTypes)
	return c
}

func jsonStrings(ss []string) string {
	out := "["
	for i, h := range ss {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("%q", h)
	}
	return out + "]"
}

// tiesByPlan are the statements whose bare m.id, among a sender's mail of
// its newest date, came from the plan before: with one account the old
// statements walked messages_account_date (date DESC, id DESC) and kept the
// highest id, with all accounts they scanned the table and kept the lowest.
// The indexes keep the lowest in both. The rest of the row is compared.
var tiesByPlan = map[string]bool{"received1": true, "received2": true, "lists1": true, "lists2": true}

func withoutID(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, r := range rows {
		out[i] = r[:len(r)-1]
	}
	return out
}

func TestScanIndexesMatchOldQueries(t *testing.T) {
	for seed := range uint64(5) {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			s := scanStore(t, seed)
			got := readScanCase(t, s, contactQueries)
			assert.NotEmpty(t, got.sql["lists0"], "list mail")
			assert.NotEmpty(t, got.sql["ours0"], "mail in Sent")
			assert.NotEmpty(t, got.headers["ann@x.com"])
			assert.NotEmpty(t, got.newest["ann@x.com"])
			assert.Len(t, got.sigImages, 5)
			dropScanIndexes(t, s)
			want := readScanCase(t, s, oldContactQueries)
			for k, rows := range want.sql {
				if tiesByPlan[k] {
					rows, got.sql[k] = withoutID(rows), withoutID(got.sql[k])
				}
				assert.Equal(t, rows, got.sql[k], k)
			}
			assert.Equal(t, want.contacts, got.contacts)
			assert.Equal(t, want.headers, got.headers)
			assert.Equal(t, want.newest, got.newest)
			assert.Equal(t, want.sigTypes, got.sigTypes)
		})
	}
}

// The contacts gathering received the lowest id of a sender's newest mail
// before the indexes, and still does: the Latest a contact opens.
func TestContactsLatestTies(t *testing.T) {
	keyring.MockInit()
	s, err := New(t.Context(), Options{DataDir: t.TempDir()})
	require.NoError(t, err)
	defer s.Close()
	for _, q := range []string{
		`INSERT INTO accounts(id, email) VALUES (1, 'a@x'), (2, 'b@x')`,
		`INSERT INTO folders(id, account_id, name, role) VALUES (1, 1, 'INBOX', 'inbox'), (2, 2, 'INBOX', 'inbox')`,
		`INSERT INTO threads(id, account_id, last_date) VALUES (1, 1, 1)`,
		// one mail to both accounts: same header and date, the second account's row first
		`INSERT INTO messages(id, account_id, thread_id, from_addr, date, flags, list_unsubscribe) VALUES
			(7, 2, 1, 'News <n@l.example>', 100, '', '<mailto:u@l.example>'),
			(9, 1, 1, 'News <n@l.example>', 100, '\Seen', '<mailto:u@l.example>'),
			(3, 1, 1, 'News <n@l.example>', 50, '', '<mailto:u@l.example>')`,
		`INSERT INTO message_locations(message_id, folder_id, shown, entered_at) VALUES (7, 2, 1, 0), (9, 1, 1, 0), (3, 1, 1, 0)`,
	} {
		_, err := s.db.Execute(t.Context(), q)
		require.NoError(t, err, q)
	}
	cs, err := s.Contacts(t.Context())
	require.NoError(t, err)
	require.Len(t, cs, 1)
	assert.Equal(t, int64(7), cs[0].Latest)
	assert.Equal(t, int64(7), cs[0].ListMessage)
	assert.Equal(t, 2, cs[0].Unread)
}

// The plans read the indexes, not the message rows. Without sqlite_stat1
// the planner decides on the schema alone, so these are the plans on a
// large store.
func TestScanIndexPlans(t *testing.T) {
	s := scanStore(t, 1)
	ctx := t.Context()
	left, err := s.roleFolderIDs(ctx, "sent", "drafts", "trash", "junk")
	require.NoError(t, err)
	sent, err := s.roleFolderIDs(ctx, "sent")
	require.NoError(t, err)
	for _, acc := range []int64{0, 2} {
		q := contactQueries(left, sent, acc)
		p := plan(t, s, q.received, q.args...)
		t.Logf("contacts received, account %d:\n%s", acc, p)
		assert.Regexp(t, `(?m)SCAN m USING COVERING INDEX messages_from$`, p)
		assert.NotContains(t, p, "GROUP BY")
		p = plan(t, s, q.lists, q.args...)
		t.Logf("contacts lists, account %d:\n%s", acc, p)
		assert.Contains(t, p, "SCAN m USING COVERING INDEX messages_from_list")
		p = plan(t, s, q.ours, q.oursArgs...)
		t.Logf("contacts ours, account %d:\n%s", acc, p)
		assert.Contains(t, p, "SEARCH m USING INTEGER PRIMARY KEY (rowid=?)")
		assert.NotRegexp(t, `SCAN m\b`, p)
	}
	p := plan(t, s, fromHeadersSQL+`from_addr LIKE ? ESCAPE '\'`, "%@x.com%")
	t.Logf("from headers:\n%s", p)
	assert.Contains(t, p, "SCAN messages USING COVERING INDEX messages_from")
	p = plan(t, s, newestFromSQL(len(left)), append([]any{`["Ann <ann@x.com>"]`}, left...)...)
	t.Logf("newest from:\n%s", p)
	assert.Contains(t, p, "SEARCH m USING COVERING INDEX messages_from (from_addr=?)")
	p = plan(t, s, "SELECT cid, content_type, NULL FROM signature_images GROUP BY cid")
	t.Logf("signature images:\n%s", p)
	assert.Contains(t, p, "SCAN signature_images USING COVERING INDEX signature_images_cid")
	assert.NotContains(t, p, "TEMP B-TREE")
}
