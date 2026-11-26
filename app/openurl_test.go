package app

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/coretest"
	"github.com/godwit-email/godwit/core/events"
	gomail "github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/internal/i18n"
)

func TestParseOpenLink(t *testing.T) {
	for link, want := range map[string]openLink{
		"godwit://draft/45":  {kind: linkDraft, id: 45},
		"godwit:draft/45":    {kind: linkDraft, id: 45},
		"GODWIT://Draft/45/": {kind: linkDraft, id: 45},
		"godwit://message/7": {kind: linkMessage, id: 7},
		"godwit://message?message-id=%3Ca%40b%3E&account=me@x":    {kind: linkMessage, messageID: "a@b", account: "me@x"},
		"godwit://compose?mailto=mailto%3Abob%40x%3Fsubject%3DHi": {kind: linkCompose, mailto: "mailto:bob@x?subject=Hi"},
		"godwit://compose?to=bob@x&subject=A+%26+B&account=2":     {kind: linkCompose, mailto: "mailto:?to=bob%40x&subject=A%20%26%20B", account: "2"},
		"godwit://reply?message-id=a@b&body=Yes%0Asure&all=1":     {kind: linkReply, messageID: "a@b", body: "Yes\nsure", all: true},
		"godwit://reply?message-id=<a@b>&all=0":                   {kind: linkReply, messageID: "a@b"},
	} {
		got, err := parseOpenLink(link)
		if err != nil {
			t.Errorf("%s: %v", link, err)
			continue
		}
		if got != want {
			t.Errorf("%s: %+v, want %+v", link, got, want)
		}
	}
	for _, link := range []string{
		"godwit://link/example.com/x?m=1&h=0123456789abcdef", // a model's link reference, never one to open
		"godwit://draft/x", "godwit://draft/0", "godwit://draft", "godwit://message", "godwit://message?message-id=<>",
		"godwit://compose/1", "godwit://compose?mailto=https://x", "godwit://reply?body=x", "godwit://unknown/1",
		"mailto:bob@x", "https://godwit.email",
	} {
		if got, err := parseOpenLink(link); err == nil {
			t.Errorf("%s: %+v, want an error", link, got)
		}
	}
}

// A compose link's fields become a mailto: link core reads back as given.
func TestMailtoOfRoundTrip(t *testing.T) {
	q := url.Values{"to": {"bob@x", "carol@x"}, "cc": {"dan@x"}, "subject": {"A + B & C = 50%"}, "body": {"Line 1\nLine 2?"}}
	m, err := gomail.ParseMailto(mailtoOf(q))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.To, ",") != "bob@x,carol@x" || strings.Join(m.Cc, ",") != "dan@x" || m.Subject != "A + B & C = 50%" || m.Body != "Line 1\nLine 2?" {
		t.Fatalf("%+v", m)
	}
}

func TestValidOpenTarget(t *testing.T) {
	for _, what := range []string{OpenWindow, OpenCompose, "draft:3", "message:3", "account:2", "mailto:bob@x", "MAILTO:bob@x", "godwit://draft/3"} {
		if !ValidOpenTarget(what) {
			t.Errorf("%q not taken", what)
		}
	}
	for _, what := range []string{"", "draft:", "draft:x", "message:-1", "mcp_send:1", "https://x", "file:///etc/passwd", "settings"} {
		if ValidOpenTarget(what) {
			t.Errorf("%q taken", what)
		}
	}
}

// Drafts and links reach the App by Open (a launch, a second launch, the
// system) or the backend's events (core.OpenDraft, a link the backend was
// handed): each opens a compose window or the message, or says why not.
func TestAppOpensDraftsAndLinks(t *testing.T) {
	h, acc := withAccount(t)
	ctx := context.Background()
	h.env.Deliver(t, acc.ID, "INBOX", coretest.Msg("Bob <bob@example.com>", acc.Email, "Lunch", "Lunch on Tuesday?", coretest.MessageID("lunch@example.com")))
	refs, err := h.env.Svc.FindMessagesByMessageID(ctx, "lunch@example.com")
	if err != nil || len(refs) != 1 {
		t.Fatalf("%v %v", refs, err)
	}

	d, err := h.env.Svc.NewDraft(ctx, acc.ID, "new", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.app.Open(core.OpenTarget(core.OpenDraftPrefix, d.ID))
	if got := h.composeOpened(0); got != d.ID {
		t.Fatalf("opened draft %d, want %d", got, d.ID)
	}
	d2, err := h.env.Svc.NewDraft(ctx, acc.ID, "new", 0)
	if err != nil {
		t.Fatal(err)
	}
	h.env.Svc.Publish(events.Event{Type: events.OpenRequested, Open: "godwit://draft/" + strconv.FormatInt(d2.ID, 10)})
	if got := h.composeOpened(d.ID); got != d2.ID {
		t.Fatalf("opened draft %d, want %d", got, d2.ID)
	}

	h.app.Open("godwit://reply?message-id=%3Clunch%40example.com%3E&body=" + url.QueryEscape("Yes, godwit://link/x?m=1&h=0123456789abcdef"))
	reply := h.draft(h.composeOpened(d2.ID))
	if reply.Mode != "reply" || reply.SourceMessageID != refs[0].ID || !strings.Contains(reply.HTML, "Yes, ") {
		t.Fatalf("reply %+v", reply)
	}
	if strings.Contains(reply.HTML, "godwit://link/") {
		t.Fatalf("a link reference from outside stays one: %s", reply.HTML)
	}

	h.app.Open("mailto:carol@example.com?subject=Hello")
	mt := h.draft(h.composeOpened(reply.ID))
	if strings.Join(mt.To, ",") != "carol@example.com" || mt.Subject != "Hello" || mt.AccountID != acc.ID {
		t.Fatalf("mailto draft %+v", mt)
	}
	n := len(h.plat.Calls())
	h.app.Open("mailto:carol@example.com?subject=Hello") // the system and a second launch both gave it
	h.app.Open("godwit://compose?to=dan@example.com&subject=Plan&account=ALICE@example.com")
	cd := h.draft(h.composeOpened(mt.ID))
	if strings.Join(cd.To, ",") != "dan@example.com" || cd.Subject != "Plan" {
		t.Fatalf("compose draft %+v", cd)
	}
	opened := 0
	for _, c := range h.plat.Calls()[n:] {
		if strings.HasPrefix(c, "OpenCompose ") {
			opened++
		}
	}
	if opened != 1 {
		t.Fatalf("%d compose windows, want 1: the repeated link was not dropped", opened)
	}

	h.app.Open("godwit://compose?to=x@example.com&account=nobody@example.com")
	h.see(i18n.Tf("open.no_account", "nobody@example.com"))
	h.app.Open("godwit://reply?message-id=missing@example.com")
	h.see(i18n.T("open.no_message"))
	h.app.Open("godwit://link/example.com?m=1&h=0123456789abcdef")
	h.see(i18n.T("open.bad_link"))

	h.app.Open("godwit://message?message-id=lunch@example.com")
	h.until("the message open", func() bool { return h.app.route.ThreadID == refs[0].ThreadID })
}

// A link the interface is launched with waits for the first load: it needs
// the accounts to write from.
func TestAppOpenLinkBeforeLoad(t *testing.T) {
	h, _ := withAccount(t)
	h.dropUI()
	h.connectUI() // nothing loaded yet
	h.app.Open("mailto:carol@example.com")
	d := h.draft(h.composeOpened(0))
	if strings.Join(d.To, ",") != "carol@example.com" {
		t.Fatalf("draft %+v", d)
	}
}
