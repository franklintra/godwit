package app

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/internal/i18n"
	"github.com/godwit-email/godwit/internal/kit"
)

// Links from outside Godwit (ADR 0049): the system hands Godwit the mailto:
// links it is the mail app for and godwit: links, and `godwit open` and
// `godwit --open` take them too. All of them only show something or start a
// draft the user reviews: none sends, deletes or changes mail.
//
//	godwit://draft/<id>                       the draft in a compose window
//	godwit://message/<id>                     the message in the main window
//	godwit://message?message-id=<Message-ID>  the same, found by its Message-ID header
//	godwit://compose?mailto=<mailto: link>    a new draft, as the mailto: link fills it
//	godwit://compose?to=…&cc=…&subject=…&body=…
//	godwit://reply?message-id=<Message-ID>&body=…&all=1
//
// compose and reply take account=<address or ID>, the account to write
// from (compose: the default one; reply: the account that has the message).
// godwit://link/… is no link of this kind: it is how Godwit shows a model a
// link in mail (core's link references), and it is refused here.

// IsOpenURL reports whether link is a URL Godwit opens from outside: a
// mailto: or godwit: one.
func IsOpenURL(link string) bool {
	scheme, _, ok := strings.Cut(link, ":")
	return ok && (strings.EqualFold(scheme, "mailto") || strings.EqualFold(scheme, "godwit"))
}

// ValidOpenTarget reports whether what is something `godwit --open` (Open)
// opens: the main window, a new compose window, a draft, a message, an
// account's inbox, or a link IsOpenURL takes.
func ValidOpenTarget(what string) bool {
	if what == OpenWindow || what == OpenCompose || IsOpenURL(what) {
		return true
	}
	for _, prefix := range []string{core.OpenDraftPrefix, core.OpenMessagePrefix, "account:"} {
		if _, ok := core.ParseOpenTarget(what, prefix); ok {
			return true
		}
	}
	return false
}

// openLinkKind is what a godwit: link asks for.
type openLinkKind string

const (
	linkDraft   openLinkKind = "draft"
	linkMessage openLinkKind = "message"
	linkCompose openLinkKind = "compose"
	linkReply   openLinkKind = "reply"
)

// openLink is a parsed godwit: link.
type openLink struct {
	kind      openLinkKind
	id        int64  // draft, message: Godwit's ID
	messageID string // message, reply: the RFC 5322 Message-ID
	account   string // compose, reply: the account's address or ID; "" for the usual one
	mailto    string // compose: the mailto: link that fills the draft
	body      string // reply: plain text above the quote
	all       bool   // reply: to everyone
}

var errBadLink = errors.New("not a link Godwit opens")

// parseOpenLink reads a godwit: link, in its godwit://kind/… form or the
// opaque godwit:kind/… one.
func parseOpenLink(link string) (openLink, error) {
	u, err := url.Parse(link)
	if err != nil || !strings.EqualFold(u.Scheme, "godwit") {
		return openLink{}, errBadLink
	}
	rest := u.Opaque
	if rest == "" {
		rest = u.Host + u.Path
	}
	kind, arg, _ := strings.Cut(strings.Trim(rest, "/"), "/")
	q := u.Query()
	l := openLink{kind: openLinkKind(strings.ToLower(kind)), account: q.Get("account")}
	switch l.kind {
	case linkDraft, linkMessage:
		if arg != "" {
			l.id, err = strconv.ParseInt(arg, 10, 64)
			if err != nil || l.id <= 0 {
				return openLink{}, errBadLink
			}
			return l, nil
		}
		if l.kind == linkMessage {
			if l.messageID = core.NormalizeMessageID(q.Get("message-id")); l.messageID != "" {
				return l, nil
			}
		}
	case linkCompose:
		if arg != "" {
			break
		}
		l.mailto = q.Get("mailto")
		if l.mailto == "" {
			l.mailto = mailtoOf(q)
		}
		if !strings.HasPrefix(strings.ToLower(l.mailto), "mailto:") {
			break
		}
		return l, nil
	case linkReply:
		if arg != "" {
			break
		}
		l.messageID, l.body = core.NormalizeMessageID(q.Get("message-id")), q.Get("body")
		switch strings.ToLower(q.Get("all")) {
		case "", "0", "false", "no":
		default:
			l.all = true
		}
		if l.messageID != "" {
			return l, nil
		}
	}
	return openLink{}, errBadLink
}

// mailtoOf is a mailto: link with q's to, cc, bcc, subject and body, as
// core.NewMailtoDraft reads it: percent-encoded, + kept for itself.
func mailtoOf(q url.Values) string {
	var parts []string
	for _, k := range []string{"to", "cc", "bcc", "subject", "body"} {
		for _, v := range q[k] {
			parts = append(parts, k+"="+strings.ReplaceAll(url.QueryEscape(v), "+", "%20"))
		}
	}
	return "mailto:?" + strings.Join(parts, "&")
}

// openTarget opens a draft (draft:<id>), a mailto: link or a godwit: link
// (Open), after the first load. The same one given again within moments is
// dropped: the system and a second launch may both hand Godwit a link.
func (a *App) openTarget(what string) {
	if at, ok := a.noteSeen[what]; ok && a.clock.Now().Sub(at) < noteDedupe {
		return
	}
	if a.noteSeen == nil {
		a.noteSeen = map[string]time.Time{}
	}
	a.noteSeen[what] = a.clock.Now()
	if id, ok := core.ParseOpenTarget(what, core.OpenDraftPrefix); ok {
		a.openDraft(id)
		return
	}
	if u, err := url.Parse(what); err == nil && strings.EqualFold(u.Scheme, "mailto") {
		a.mailto(what, 0)
		return
	}
	l, err := parseOpenLink(what)
	if err != nil {
		a.plat.ShowWindow()
		a.toastAs(kit.ToastError, i18n.T("open.bad_link"))
		return
	}
	switch l.kind {
	case linkDraft:
		a.openDraft(l.id)
	case linkMessage:
		if l.id != 0 {
			a.NotificationClicked(core.OpenTarget(core.OpenMessagePrefix, l.id))
			return
		}
		a.openByMessageID(l)
	case linkCompose:
		acc, ok := a.linkAccount(l.account, a.defaultAccount())
		if ok {
			a.mailto(l.mailto, acc)
		}
	case linkReply:
		a.openByMessageID(l)
	}
}

// openDraft shows draft id in its compose window, once core has it.
func (a *App) openDraft(id int64) {
	a.do(func(ctx context.Context) error {
		_, err := a.svc.GetDraft(ctx, id)
		return err
	}, func(err error) {
		if err != nil {
			a.plat.ShowWindow()
			a.toastErr(err)
			return
		}
		a.plat.OpenCompose(id)
	})
}

// linkAccount is the account a link names, by address (any case) or ID;
// def for none. An account Godwit does not have is said in a toast: false.
func (a *App) linkAccount(name string, def int64) (int64, bool) {
	if name == "" {
		return def, true
	}
	for _, acc := range a.accounts {
		if strings.EqualFold(acc.Email, name) || strconv.FormatInt(acc.ID, 10) == name {
			return acc.ID, true
		}
	}
	a.plat.ShowWindow()
	a.toastAs(kit.ToastError, i18n.Tf("open.no_account", name))
	return 0, false
}

// openByMessageID finds l's message by its Message-ID, in l's account if
// it names one, else preferably the default account's copy, then shows it
// (linkMessage) or starts a reply to it in a compose window (linkReply).
func (a *App) openByMessageID(l openLink) {
	want, ok := a.linkAccount(l.account, 0)
	if !ok {
		return
	}
	def := a.defaultAccount()
	var ref core.MessageRef
	var draft int64
	a.do(func(ctx context.Context) error {
		refs, err := a.svc.FindMessagesByMessageID(ctx, l.messageID)
		if err != nil {
			return err
		}
		found := false
		for _, r := range refs { // newest first
			if want != 0 && r.AccountID != want {
				continue
			}
			if !found || want == 0 && r.AccountID == def && ref.AccountID != def {
				ref, found = r, true
			}
		}
		if !found || l.kind != linkReply {
			return nil
		}
		mode := "reply"
		if l.all {
			mode = "reply_all"
		}
		d, err := a.svc.NewDraft(ctx, ref.AccountID, mode, ref.ID)
		if err != nil {
			return err
		}
		if l.body != "" {
			d.HTML = core.TextToDraftHTML(l.body) + d.HTML
			if d, err = a.svc.SaveDraft(ctx, d); err != nil {
				return err
			}
		}
		draft = d.ID
		return nil
	}, func(err error) {
		switch {
		case err != nil:
			a.plat.ShowWindow()
			a.toastErr(err)
		case ref.ID == 0:
			a.plat.ShowWindow()
			a.toastAs(kit.ToastError, i18n.T("open.no_message"))
		case draft != 0:
			a.plat.OpenCompose(draft)
		default:
			a.NotificationClicked(core.OpenTarget(core.OpenMessagePrefix, ref.ID))
		}
	})
}
