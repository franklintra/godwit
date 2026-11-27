package core

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
	"time"

	gomail "github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/core/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// refMailPeople is mailPeople as it read before (perf/bench2 0125090): the
// newest Sent mail found by walking messages in date order.
func refMailPeople(ctx context.Context, s *Service, roles map[int64]string) (mailPeople, error) {
	p := mailPeople{own: map[string]bool{}, known: map[string]bool{}}
	acc, err := s.db.Query(ctx, ownEmails)
	if err != nil {
		return p, err
	}
	for _, r := range acc.Values {
		p.own[ownKey(store.Str(r[0]))] = true
	}
	var sent []int64
	for id, r := range roles {
		if r == "sent" {
			sent = append(sent, id)
		}
	}
	rows, err := s.db.Query(ctx, "SELECT from_addr, to_addrs, cc_addrs FROM messages m WHERE "+shownIn("(SELECT value FROM json_each(?))")+" ORDER BY date DESC LIMIT ?", intList(sent), sentMailRead)
	if err != nil {
		return p, err
	}
	for _, r := range rows.Values {
		p.own[ownKey(store.Str(r[0]))] = true
		for _, a := range append(jsonList(store.Str(r[1])), jsonList(store.Str(r[2]))...) {
			p.known[gomail.AddrKey(a)] = true
		}
	}
	return p, nil
}

// refReplyStatuses is replyStatuses as it read before: every conversation,
// with refMailPeople.
func refReplyStatuses(ctx context.Context, s *Service, now int64) (map[int64]replyStatus, error) {
	roles, err := s.folderRoles(ctx)
	if err != nil {
		return nil, err
	}
	people, err := refMailPeople(ctx, s, roles)
	if err != nil {
		return nil, err
	}
	return s.replyStatusesOf(ctx, now, roles, people, nil)
}

// refThreadDeadline is ThreadDeadline as it read before, with statuses the
// whole mailbox's (refReplyStatuses) at the clock's now.
func refThreadDeadline(ctx context.Context, s *Service, statuses map[int64]replyStatus, threadID int64) (ReaderDeadline, error) {
	now := s.userNow(ctx)
	got, err := s.deadlinesIn(ctx, now, statuses, []int64{threadID})
	st := statuses[threadID]
	out := ReaderDeadline{Unreplied: st.Kind == ReplyStatusUnreplied, Waiting: st.Kind == ReplyStatusWaiting}
	if out.Waiting {
		out.WaitingDays = int((now.Unix() - st.Since) / 86400)
	}
	if d, ok := got[threadID]; ok && d.Date != "" {
		out.Deadline = &d
	}
	return out, err
}

// refCheckDeadline is deadlineWrites' checks for a source other than manual
// as they read before, with statuses the whole mailbox's.
func refCheckDeadline(ctx context.Context, s *Service, statuses map[int64]replyStatus, source string, threadID int64, d Deadline) error {
	now := s.userNow(ctx)
	current, err := s.deadlinesIn(ctx, now, statuses, []int64{threadID})
	if err != nil {
		return err
	}
	if cur, has := current[threadID]; has && deadlineRank(cur.Source) < deadlineRank(source) {
		return ErrDeadlineOutranked
	}
	return s.checkDeadlineMessage(ctx, threadID, statuses[threadID], d)
}

// replyMailbox is a randomized mailbox with everything 回复状态 reads: the
// user's addresses and aliases (+tags, Gmail dots), Sent copies and hidden
// locations, Trash/Junk/Drafts, All Mail standing in for archive, archived
// mail, \Answered, list and automated headers, role and noreply senders,
// Reply-To, mass mail by its words, Bcc'd mail, mail to someone else,
// Message-ID copies, pending purges, server drafts, archive marks (by
// Message-ID, by date, with times and versions), queued replies (failed or
// not), conversations older than the cutoff, and 期限 of every source.
type replyMailbox struct {
	s       *Service
	clock   interface{ Advance(time.Duration) }
	threads []int64
	msgs    map[int64][]int64 // by thread
}

func newReplyMailbox(t *testing.T, seed uint64) *replyMailbox {
	t.Helper()
	keyring.MockInit()
	clock := useTestClock(t)
	s, err := New(bg, Options{DataDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	q := func(sql string, args ...any) int64 {
		t.Helper()
		res, err := s.db.Execute(bg, sql, args...)
		require.NoError(t, err, sql)
		return res.LastInsertID
	}
	r := rand.New(rand.NewPCG(seed, 0x7d1d))
	pick := func(n int) int { return r.IntN(n) }
	chance := func(pct int) bool { return r.IntN(100) < pct }
	one := func(xs []string) string { return xs[pick(len(xs))] }

	accounts := []string{"me@one.example", "me@two.example", "me.three@gmail.com"}
	// How each account's mail is written as the user's: plain, an alias, a
	// +tag, Gmail dots and case, and an address only Sent mail shows.
	mine := [][]string{
		{"me@one.example", "Me <me@one.example>", "alias@one.example", "me+news@one.example"},
		{"me@two.example", "Desk <shared@desk.example>", "me@two.example"},
		{"me.three@gmail.com", "methree@gmail.com", "Me.Three+x@googlemail.com", "old-me@three.example"},
	}
	type folder struct{ name, role, override string }
	layouts := [][]folder{
		{{"INBOX", "inbox", ""}, {"Sent", "sent", ""}, {"Trash", "trash", ""}, {"Junk", "junk", ""}, {"Drafts", "drafts", ""}, {"Archive", "archive", ""}, {"Projects", "", ""}},
		{{"INBOX", "inbox", ""}, {"Sent", "sent", ""}, {"Trash", "trash", ""}, {"Drafts", "drafts", ""}, {"Done", "", "archive"}, {"Desk", "", "inbox"}, {"Old sent", "", "sent"}},
		{{"INBOX", "inbox", ""}, {"[Gmail]/Sent Mail", "sent", ""}, {"[Gmail]/Trash", "trash", ""}, {"[Gmail]/Spam", "junk", ""}, {"[Gmail]/Drafts", "drafts", ""}, {"[Gmail]/All Mail", "all", ""}},
	}
	folders := make([][]int64, len(accounts))
	sentOf := make([]int64, len(accounts))
	isSent := map[int64]bool{} // only the user's mail goes there: its senders are the user's
	for i, addr := range accounts {
		q("INSERT INTO accounts(id, email) VALUES (?, ?)", i+1, addr)
		for _, f := range layouts[i] {
			id := q("INSERT INTO folders(account_id, name, role, role_override) VALUES (?, ?, ?, ?)", i+1, f.name, f.role, f.override)
			folders[i] = append(folders[i], id)
			if f.role == "sent" {
				sentOf[i] = id
			}
			isSent[id] = f.role == "sent" || f.override == "sent"
		}
	}
	q("INSERT INTO identities(account_id, email) VALUES (1, 'alias@one.example')")
	q("INSERT INTO identities(account_id, email) VALUES (2, 'Shared@Desk.example')")

	people := []string{"Bob <bob@a.example>", "carol@b.example", "Dan <dan@c.example>", "erin@d.example", "frank@e.example",
		"news@news.example", "noreply@shop.example", "info@corp.example", "Support <support@help.example>", "jo@email.service.example",
		"gina@f.example", "hal@g.example"}
	flagSets := []string{"", `\Seen`, `\Seen \Flagged`, `\Answered \Seen`, `\Answered`, `\Seen`}
	words := []string{"see you", "Click to Unsubscribe", "manage preferences here", "thanks", "退订", "lunch?"}
	now := clock.Now().Unix()
	mb := &replyMailbox{s: s, clock: clock, msgs: map[int64][]int64{}}

	add := func(acc int, tid int64, mid string, date int64, fromMe bool) int64 {
		from, to := one(people), []string{one(mine[acc])}
		if fromMe {
			from, to = one(mine[acc]), []string{one(people)}
		}
		switch {
		case chance(8):
			to = []string{"someone@else.example"}
		case chance(5):
			to = nil // Bcc
		case chance(10):
			to = append(to, one(people))
		}
		cc := []string{}
		if chance(20) {
			cc = append(cc, one(people))
		}
		toJSON, ccJSON := `[]`, `[]`
		if len(to) > 0 {
			toJSON = `["` + strings.Join(to, `","`) + `"]`
		}
		if len(cc) > 0 {
			ccJSON = `["` + strings.Join(cc, `","`) + `"]`
		}
		replyTo, listID, listUnsub, auto, prec := "", "", "", "", ""
		if chance(10) {
			replyTo = one(people)
		}
		if chance(6) {
			listID = "<list.example>"
		}
		if chance(6) {
			listUnsub = "<mailto:u@list.example>"
		}
		if chance(5) {
			auto = one([]string{"auto-generated", "auto-replied; x", "no", "No"})
		}
		if chance(5) {
			prec = one([]string{"bulk", "List", "junk", "first-class"})
		}
		id := q(`INSERT INTO messages(account_id, thread_id, message_id, from_addr, to_addrs, cc_addrs, reply_to, list_id, list_unsubscribe,
			auto_submitted, precedence, subject, date, flags, snippet, body_text) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'snippet', ?)`,
			acc+1, tid, mid, from, toJSON, ccJSON, replyTo, listID, listUnsub, auto, prec, fmt.Sprintf("subject %d", tid), date, one(flagSets),
			"Please confirm by Friday. "+one(words))
		mb.msgs[tid] = append(mb.msgs[tid], id)
		fs := folders[acc]
		used := map[int64]bool{}
		for k := range 1 + pick(3) {
			f := fs[pick(len(fs))]
			if k == 0 && fromMe && chance(60) {
				f = sentOf[acc]
			} else if k == 0 && !fromMe && chance(50) {
				f = fs[0]
			}
			if used[f] || isSent[f] && !fromMe {
				continue
			}
			used[f] = true
			shown := 1
			if chance(12) {
				shown = 0
			}
			q("INSERT INTO message_locations(message_id, folder_id, shown, entered_at, date) VALUES (?, ?, ?, 0, ?)", id, f, shown, date)
		}
		if chance(4) {
			q("INSERT INTO ops(account_id, message_id, kind, state, created_at) VALUES (?, ?, 'purge', 'pending', 0)", acc+1, id)
		}
		if chance(3) {
			q(`INSERT INTO server_drafts(account_id, token, message_id, revision, folder, state, created_at) VALUES (?, ?, ?, 1, 'Drafts', 'stored', 0)`, acc+1, fmt.Sprint(id), id)
		}
		return id
	}

	outboxFailed := q("INSERT INTO outbox(account_id, state, created_at) VALUES (1, 'failed', 0)")
	outboxSent := q("INSERT INTO outbox(account_id, state, created_at) VALUES (1, 'sent', 0)")
	var byAcc [3][]int64
	for i := range 260 {
		acc := pick(3)
		tid := q("INSERT INTO threads(account_id, subject, last_date) VALUES (?, ?, 0)", acc+1, fmt.Sprintf("subject %d", i))
		mb.threads = append(mb.threads, tid)
		byAcc[acc] = append(byAcc[acc], tid)
		var last int64
		var lastMID string
		for j := range 1 + pick(4) {
			date := now - r.Int64N(45*86400) // some older than the 30-day cutoff
			if chance(10) {
				date = now - 3*86400 // ties order by id
			}
			mid := fmt.Sprintf("<%d-%d-%d@x>", seed, i, j)
			if chance(10) {
				mid = ""
			}
			add(acc, tid, mid, date, chance(30))
			if mid != "" && chance(12) { // a Gmail-style copy of the same mail
				add(acc, tid, mid, date, false)
			}
			if mid != "" && chance(4) && len(byAcc[acc]) > 1 { // the same Message-ID in another conversation
				other := byAcc[acc][pick(len(byAcc[acc])-1)]
				add(acc, other, mid, date, false)
				q("UPDATE threads SET last_date = max(last_date, ?) WHERE id = ?", date, other)
			}
			if date >= last {
				last, lastMID = date, mid
			}
		}
		q("UPDATE threads SET last_date = max(last_date, ?) WHERE id = ?", last, tid)
		if chance(15) {
			markMID, markDate := "", last-int64(pick(3))*86400
			if chance(40) {
				markMID = lastMID
			}
			q("INSERT INTO archive_marks(thread_id, message_id_hdr, date, archived_at, archived_version) VALUES (?, ?, ?, ?, ?)",
				tid, markMID, markDate, now-int64(pick(5))*86400, pick(4))
		}
		if chance(10) {
			ob := []int64{outboxFailed, outboxSent, 999999}[pick(3)]
			q("INSERT INTO reply_records(thread_id, outbox_id, at) VALUES (?, ?, ?)", tid, ob, last+int64(pick(3)-1)*3600)
		}
		if chance(25) {
			msgs := mb.msgs[tid]
			msg := any(msgs[pick(len(msgs))])
			if chance(10) {
				msg = nil
			}
			source := one([]string{DeadlineManual, DeadlineProgram, DeadlineWit, DeadlineAI})
			date := time.Unix(now, 0).AddDate(0, 0, pick(20)-10).Format(time.DateOnly)
			due := any(date)
			if source == DeadlineManual && chance(25) {
				due = nil // a removal marker
			}
			q(`INSERT INTO thread_deadlines(thread_id, due_date, due_time, source, message_id, quote, set_at, set_version)
				VALUES (?, ?, ?, ?, ?, 'confirm', ?, ?)`, tid, due, one([]string{"", "09:30"}), source, msg, now-int64(pick(6))*86400, pick(4))
		}
	}
	return mb
}

// One conversation's 回复状态 and reply date read for it alone are what the
// whole mailbox's read gives it, for every conversation of seeded mailboxes,
// now and later (when the 30-day cutoff has moved past some of them).
func TestThreadReplyStatusMatchesWholeMailbox(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			mb := newReplyMailbox(t, seed)
			s := mb.s
			roles, err := s.folderRoles(bg)
			require.NoError(t, err)
			people, err := s.mailPeople(bg, roles)
			require.NoError(t, err)
			ref, err := refMailPeople(bg, s, roles)
			require.NoError(t, err)
			assert.Equal(t, ref, people, "Sent mail read from its locations")

			for _, later := range []time.Duration{0, 12 * 24 * time.Hour} {
				mb.clock.Advance(later)
				now := s.userNow(bg).Unix()
				all, err := refReplyStatuses(bg, s, now)
				require.NoError(t, err)
				kinds := map[string]int{}
				for _, st := range all {
					kinds[st.Kind]++
				}
				require.Positive(t, kinds[ReplyStatusUnreplied], "the mailbox has 未回复 conversations")
				require.Positive(t, kinds[ReplyStatusWaiting], "and 等待回复 ones")
				require.Less(t, len(all), len(mb.threads), "and some with neither")

				got, err := s.threadReplyStatuses(bg, now, mb.threads...)
				require.NoError(t, err)
				assert.Equal(t, all, got, "every conversation at once")
				withDate := 0
				for _, tid := range mb.threads {
					one, err := s.threadReplyStatuses(bg, now, tid)
					require.NoError(t, err)
					want := map[int64]replyStatus{}
					if st, ok := all[tid]; ok {
						want[tid] = st
					}
					assert.Equal(t, want, one, "thread %d", tid)

					wantD, err := refThreadDeadline(bg, s, all, tid)
					require.NoError(t, err)
					gotD, err := s.ThreadDeadline(bg, tid)
					require.NoError(t, err)
					assert.Equal(t, wantD, gotD, "ThreadDeadline %d", tid)
					if gotD.Deadline != nil {
						withDate++
					}

					for _, source := range []string{DeadlineProgram, DeadlineAI} {
						for _, msg := range mb.msgs[tid] {
							d := Deadline{Date: time.Unix(now, 0).AddDate(0, 0, 3).Format(time.DateOnly), MessageID: msg, Quote: "confirm by Friday"}
							assert.Equal(t, refCheckDeadline(bg, s, all, source, tid, d), s.checkDeadline(bg, source, tid, d), "checkDeadline %s %d %d", source, tid, msg)
						}
					}
				}
				require.Positive(t, withDate, "some conversations show a reply date")

				var wantCount int
				rows, err := s.db.Query(bg, "SELECT thread_id FROM thread_deadlines WHERE source IN (?, ?)", DeadlineProgram, DeadlineWit)
				require.NoError(t, err)
				var ids []int64
				for _, r := range rows.Values {
					ids = append(ids, store.Int(r[0]))
				}
				inEffect, err := s.deadlinesIn(bg, s.userNow(bg), all, ids)
				require.NoError(t, err)
				wantCount = len(inEffect)
				count, err := s.CountAppAndWitDeadlines(bg)
				require.NoError(t, err)
				assert.Equal(t, wantCount, count, "CountAppAndWitDeadlines")
			}
		})
	}
}

// A conversation's read is driven from its id, never a scan of messages or
// threads.
func TestThreadReplyStatusPlan(t *testing.T) {
	mb := newReplyMailbox(t, 1)
	rows, err := mb.s.db.Query(bg, "EXPLAIN QUERY PLAN "+logicalMailsSQL("t.id IN (SELECT value FROM json_each(?)) AND t.last_date >= ?"), intList(mb.threads[:1]), 0)
	require.NoError(t, err)
	var lines []string
	for _, r := range rows.Values {
		lines = append(lines, store.Str(r[len(r)-1]))
	}
	plan := strings.Join(lines, "\n")
	t.Logf("plan:\n%s", plan)
	assert.Contains(t, plan, "messages_thread")
	assert.NotRegexp(t, regexp.MustCompile(`(?m)^SCAN (m|t)\b`), plan)
}
