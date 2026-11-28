//go:build linux

package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/uiclient"
)

// hotOp is one thing the interface asks the backend for, as it asks it
// (same method, arguments and page size as app/), timed over the socket.
type hotOp struct {
	Name string
	Desc string
	Do   func(ctx context.Context, c *uiclient.Client) error
}

// pageSize and searchLimit are app/mail_list.go's.
const (
	pageSize    = 50
	searchLimit = 200
)

func hotOps(m fixtureMeta) []hotOp {
	big := m.Accounts[0]
	list := func(name, desc string, v core.View) hotOp {
		return hotOp{name, desc, func(ctx context.Context, c *uiclient.Client) error {
			p, err := c.ListThreads(ctx, v, "", pageSize)
			if err == nil && len(p.Items) == 0 {
				err = fmt.Errorf("%s: empty", name)
			}
			return err
		}}
	}
	ops := []hotOp{
		list("list.unified_inbox", "ListThreads, unified inbox, first page", core.View{Unified: "inbox"}),
		list("list.inbox_big", "ListThreads, the largest account's INBOX", core.View{FolderID: big.Inbox}),
		list("list.archive_big", "ListThreads, the largest account's Archive", core.View{FolderID: big.Archive}),
		list("list.sent_big", "ListThreads, the largest account's Sent", core.View{FolderID: big.Sent}),
		list("list.starred", "ListThreads, unified starred", core.View{Unified: "starred"}),
		{"list.unified_inbox_page3", "ListThreads, unified inbox, third page (cursor)", func(ctx context.Context, c *uiclient.Client) error {
			cur := ""
			for range 3 {
				p, err := c.ListThreads(ctx, core.View{Unified: "inbox"}, cur, pageSize)
				if err != nil {
					return err
				}
				cur = p.NextCursor
			}
			return nil
		}},
		{"badges.counts", "the sidebar's counts: ListFolders + ListOutbox per account, StarredCounts, ListLabels", func(ctx context.Context, c *uiclient.Client) error {
			for _, a := range m.Accounts {
				if _, err := c.ListFolders(ctx, a.ID); err != nil {
					return err
				}
				if _, err := c.ListOutbox(ctx, a.ID); err != nil {
					return err
				}
			}
			if _, _, err := c.StarredCounts(ctx); err != nil {
				return err
			}
			_, err := c.ListLabels(ctx)
			return err
		}},
		{"thread.open_big", fmt.Sprintf("GetThread + ThreadLabels, the longest thread (%d messages)", m.BigThreadN), func(ctx context.Context, c *uiclient.Client) error {
			if _, err := c.GetThread(ctx, m.BigThread); err != nil {
				return err
			}
			_, err := c.ThreadLabels(ctx, []int64{m.BigThread})
			return err
		}},
		{"message.open_big", "GetMessageLazyImages, the largest HTML body", func(ctx context.Context, c *uiclient.Client) error {
			_, err := c.GetMessageLazyImages(ctx, m.BigMessage, false)
			return err
		}},
		{"message.open_typical", "GetMessageLazyImages, a typical personal message", func(ctx context.Context, c *uiclient.Client) error {
			_, err := c.GetMessageLazyImages(ctx, m.TypMessage, false)
			return err
		}},
	}
	ops = slices.DeleteFunc(ops, func(op hotOp) bool { // a real account may have no archive or sent folder
		return op.Name == "list.archive_big" && big.Archive == 0 || op.Name == "list.sent_big" && big.Sent == 0
	})
	if m.TopLabel != 0 {
		ops = append(ops, list("list.label_top", "ListThreads, the label on the most messages", core.View{LabelID: m.TopLabel}))
	}
	for _, t := range m.SearchTerms {
		q := t
		ops = append(ops, hotOp{"search." + slugify(q), fmt.Sprintf("SearchWith %q, all accounts, limit %d", q, searchLimit), func(ctx context.Context, c *uiclient.Client) error {
			_, err := c.SearchWith(ctx, core.SearchQuery{Text: q}, searchLimit)
			return err
		}})
	}
	return ops
}

func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
			b.WriteByte('_')
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// runHot times every op rounds times (after one untimed warm-up round),
// interleaved so no op gets a cache another left; after each round it calls
// between (a memory sample).
func runHot(ctx context.Context, c *uiclient.Client, ops []hotOp, rounds int, prefix string, rep *report, between func()) error {
	return runHotWith(ctx, c, ops, rounds, true, prefix, rep, between)
}

// runHotWith is runHot, with or without the warm-up round: the saved views
// take seconds each, and the opening before them has warmed the cache.
func runHotWith(ctx context.Context, c *uiclient.Client, ops []hotOp, rounds int, warmup bool, prefix string, rep *report, between func()) error {
	for i := 0; warmup && i < len(ops); i++ { // warm-up, and a check that each works
		if err := ops[i].Do(ctx, c); err != nil {
			return fmt.Errorf("%s: %w", ops[i].Name, err)
		}
	}
	for range rounds {
		for _, op := range ops {
			t0 := time.Now()
			if err := op.Do(ctx, c); err != nil {
				return fmt.Errorf("%s: %w", op.Name, err)
			}
			rep.add(prefix+"."+op.Name+"_ms", "ms", "lower", op.Desc, ms(time.Since(t0)))
		}
		if between != nil {
			between()
		}
	}
	return nil
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }

// viewOps are the calibration targets measured on Franklin's Mac: the
// accounts list, the counts, the saved views' unread counts and contents.
func viewOps(m fixtureMeta) []hotOp {
	big := m.Accounts[0]
	ops := []hotOp{
		{"accounts.list", "ListAccounts", func(ctx context.Context, c *uiclient.Client) error {
			_, err := c.ListAccounts(ctx)
			return err
		}},
		{"counts.folders_big", "ListFolders of the largest (Gmail-like) account: its folders' unread and total counts", func(ctx context.Context, c *uiclient.Client) error {
			_, err := c.ListFolders(ctx, big.ID)
			return err
		}},
		{"views.saved_views", "SavedViews: every view with its unread count, as the sidebar loads them", func(ctx context.Context, c *uiclient.Client) error {
			_, err := c.SavedViews(ctx)
			return err
		}},
	}
	for _, v := range m.Views {
		id := v.ID
		ops = append(ops, hotOp{"view." + slugify(v.Name), fmt.Sprintf("ViewContents of the view %q (groups and a first page of %d)", v.Name, pageSize), func(ctx context.Context, c *uiclient.Client) error {
			_, err := c.ViewContents(ctx, id, pageSize)
			return err
		}})
	}
	return ops
}
