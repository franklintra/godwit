package webui

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log"
	netmail "net/mail"
	"path/filepath"
	"sync"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/godwit-email/godwit/core"
	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/internal/i18n"
)

var (
	// ErrNoDraft is returned to a window that has no draft attached.
	ErrNoDraft = errors.New("this window has no draft")
	// ErrNotReady is returned before the data is open.
	ErrNotReady = errors.New("the data is not open yet")
	// ErrNoRevision is returned for a save without the revision it edits;
	// core skips the conflict check for revision 0 (core/compose.go SaveDraft).
	ErrNoRevision = errors.New("a save needs the revision it edits")
	// ErrNoPicker is returned for attaching files when there is no file dialog.
	ErrNoPicker = errors.New("no file dialog")
)

// saveHook, when set, runs in Save while it holds the session (tests).
var saveHook func()

// session is one window's draft and the parts of its body the page does not edit.
type session struct {
	mu               sync.Mutex // serializes the window's calls and its release
	draftID          int64
	quote, signature string // HTML, kept as loaded
	readReceipt      bool   // kept as loaded: the page shows it, never edits it
	loaded           bool
	closed           bool // released: calls get ErrNoDraft
	gone             bool // sent: calls get ErrNoDraft, nothing to release
}

// adopt keeps d's quote, signature and read receipt as the ones the page
// does not edit.
func (s *session) adopt(d core.Draft) (editable string) {
	editable, s.quote, s.signature = mail.SplitDraft(d.HTML)
	s.readReceipt = d.ReadReceipt
	s.loaded = true
	return editable
}

// Compose keeps each compose window's session: its draft and the parts of
// the draft the page does not edit. It is not bound: pages reach it only
// through ComposePage.
type Compose struct {
	mu       sync.Mutex
	svc      Service
	sessions map[int]*session
	follows  map[int]*follow // each window's Wit subscription
	closed   func(winID, gen int, saved bool)
	done     func(winID int)
}

// NewCompose is a Compose with no windows; SetService opens it up.
func NewCompose() *Compose { return &Compose{sessions: map[int]*session{}, follows: map[int]*follow{}} }

// SetService gives Compose the service, once the data is open.
func (c *Compose) SetService(svc Service) {
	c.mu.Lock()
	c.svc = svc
	c.mu.Unlock()
}

// Attach ties draftID to window winID, as the window opens.
func (c *Compose) Attach(winID int, draftID int64) {
	c.mu.Lock()
	c.sessions[winID] = &session{draftID: draftID}
	c.mu.Unlock()
}

// Detach forgets window winID's draft: new calls from it get ErrNoDraft; one
// already in progress finishes. Its Wit events stop.
func (c *Compose) Detach(winID int) {
	c.mu.Lock()
	delete(c.sessions, winID)
	c.mu.Unlock()
	c.unfollow(winID)
}

// Release lets go of closed window winID: new calls from it get ErrNoDraft,
// the call in progress finishes first, then its draft is deleted if it is
// empty (core DeleteDraftIfEmpty, on the revision checked), else stored on
// the server at once where the account keeps drafts there (core
// FlushServerDraft). Its Wit events stop; a run goes on.
func (c *Compose) Release(winID int) {
	c.mu.Lock()
	s := c.sessions[winID]
	delete(c.sessions, winID)
	svc := c.svc
	c.mu.Unlock()
	c.unfollow(winID)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if svc == nil || s.gone {
		return
	}
	ctx := context.Background()
	d, err := svc.GetDraft(ctx, s.draftID)
	deleted := false
	if err == nil {
		deleted, err = svc.DeleteDraftIfEmpty(ctx, d.ID, d.Revision)
	}
	if err == nil && !deleted {
		err = svc.FlushServerDraft(ctx, d.ID)
	}
	if err != nil {
		log.Printf("releasing compose window %d: %v", winID, err)
	}
}

// exists reports whether draft id is still stored.
func (c *Compose) exists(id int64) bool {
	c.mu.Lock()
	svc := c.svc
	c.mu.Unlock()
	if svc == nil {
		return false
	}
	_, err := svc.GetDraft(context.Background(), id)
	return err == nil
}

// OnClosed sets the hook that hears the page's final flush: gen is the
// close attempt it answers (the Closing event's payload), saved whether it
// succeeded.
func (c *Compose) OnClosed(fn func(winID, gen int, saved bool)) {
	c.mu.Lock()
	c.closed = fn
	c.mu.Unlock()
}

// OnDone sets the hook told that window winID's draft was sent or
// discarded: the window can close without asking.
func (c *Compose) OnDone(fn func(winID int)) {
	c.mu.Lock()
	c.done = fn
	c.mu.Unlock()
}

// finish tells OnDone's hook winID is done; no session lock is held.
func (c *Compose) finish(winID int) {
	c.mu.Lock()
	fn := c.done
	c.mu.Unlock()
	if fn != nil {
		fn(winID)
	}
}

func (c *Compose) get(winID int) (Service, *session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.svc == nil {
		return nil, nil, ErrNotReady
	}
	s := c.sessions[winID]
	if s == nil {
		return nil, nil, ErrNoDraft
	}
	return c.svc, s, nil
}

// PageDeps is what ComposePage needs from the platform.
type PageDeps struct {
	// WinOf names the calling window (mygo.CallerWindow(ctx).ID() in production).
	WinOf func(context.Context) (int, bool)
	// PickFiles asks for files to attach, in a dialog on window winID; none
	// picked is no error.
	PickFiles func(ctx context.Context, winID int) ([]string, error)
	// PopupMenu shows entries as a native menu at x, y of window winID's
	// page and returns the ID chosen, "" for none.
	PopupMenu func(winID int, entries []MenuEntry, x, y int) string
	// SendLater opens send later on window winID at at, unix ms, and
	// returns the time chosen, 0 when cancelled.
	SendLater func(winID int, at int64) int64
	// Emit sends a Wit event to a window or page (WitStream.Emit or
	// EmitPage in production).
	Emit func(t Target, ev WitEvent)
	// OpenURL opens an http(s) link in the browser; OpenCompose opens draft
	// id in a compose window.
	OpenURL     func(url string) error
	OpenCompose func(draftID int64)
	// OpenModels opens the model settings in the main window; OpenMessage
	// message id in its reader.
	OpenModels  func()
	OpenMessage func(id int64)
	// Toast, ToastBar and TitleBar hand window winID's toasts, where its send
	// bar is, and its title bar's text to native, which draws them.
	Toast    func(winID int, kind, text string)
	ToastBar func(winID int, bar, field ui.Rect)
	TitleBar func(winID int, title, status string)
}

// ComposePage is what compose pages call (mygo.Bind); every method acts on
// the calling window's draft only.
type ComposePage struct {
	c *Compose
	d PageDeps
}

// NewComposePage is the bound facade of c.
func NewComposePage(c *Compose, d PageDeps) *ComposePage {
	return &ComposePage{c: c, d: d}
}

// open is the calling window and its session, locked: the caller unlocks it.
func (p *ComposePage) open(ctx context.Context) (int, Service, *session, error) {
	id, ok := p.d.WinOf(ctx)
	if !ok {
		return 0, nil, nil, ErrNoDraft
	}
	svc, s, err := p.c.get(id)
	if err != nil {
		return 0, nil, nil, err
	}
	s.mu.Lock()
	if s.closed || s.gone {
		s.mu.Unlock()
		return 0, nil, nil, ErrNoDraft
	}
	return id, svc, s, nil
}

// Load is the calling window's draft, with the accounts, the theme and the
// page's copy.
func (p *ComposePage) Load(ctx context.Context) (ComposeState, error) {
	_, svc, s, err := p.open(ctx)
	if err != nil {
		return ComposeState{}, err
	}
	defer s.mu.Unlock()
	d, err := svc.GetDraft(ctx, s.draftID)
	if err != nil {
		return ComposeState{}, err
	}
	accs, err := senders(ctx, svc)
	if err != nil {
		return ComposeState{}, err
	}
	st, err := svc.GetSettings(ctx)
	if err != nil {
		return ComposeState{}, err
	}
	wit, err := witInfo(ctx, svc, d)
	if err != nil {
		return ComposeState{}, err
	}
	editable := s.adopt(d)
	sig, err := signatureOf(ctx, svc, d, s.signature)
	if err != nil {
		return ComposeState{}, err
	}
	pgp, err := openpgpOf(ctx, svc, d)
	if err != nil {
		return ComposeState{}, err
	}
	out := ComposeState{SignatureHTML: sig, Wit: wit, DraftID: d.ID, Accounts: accs, From: d.AccountID, Identity: d.IdentityID, To: nz(d.To), Cc: nz(d.Cc), Bcc: nz(d.Bcc),
		ReplyTo: d.ReplyTo, Subject: d.Subject, Body: pageBody(d, editable), Quote: quoteOf(d, s.quote),
		Attachments: attachments(d), Headers: headers(d), ReadReceipt: d.ReadReceipt, Revision: d.Revision, Format: d.Format,
		UpdatedAt: d.UpdatedAt.UnixMilli(), Theme: ThemeOf(st), Locale: i18n.Language(), FromProgram: d.FromProgram, OpenPGP: pgp}
	if !d.ScheduledFor.IsZero() {
		out.ScheduledFor = d.ScheduledFor.UnixMilli()
	}
	return out, nil
}

// pageBody is d's editable part as the page edits it: a plain draft's as its
// text's paragraphs, even if a program wrote other HTML into it.
func pageBody(d core.Draft, editable string) string {
	if d.Format == core.FormatPlain {
		return mail.PlainHTML(mail.PlainText(editable))
	}
	return editable
}

// quoteOf is d's quote block as the page shows it, read-only text: a plain
// draft's as it is sent, quoted with ">".
func quoteOf(d core.Draft, quote string) string {
	if d.Format == core.FormatPlain {
		return mail.PlainText(quote)
	}
	return mail.HTMLToText(quote)
}

// signatureOf is d's signature block as the page shows it: a plain draft's
// as the text it is sent as, escaped (images have no place in it).
func signatureOf(ctx context.Context, svc Service, d core.Draft, block string) (string, error) {
	if d.Format == core.FormatPlain {
		return html.EscapeString(mail.PlainText(block)), nil
	}
	return svc.SignatureShown(ctx, block)
}

// senders are the accounts a draft can be sent from, each with its
// identities.
func senders(ctx context.Context, svc Service) ([]Account, error) {
	accs, err := svc.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := svc.ListIdentities(ctx, 0)
	if err != nil {
		return nil, err
	}
	var out []Account
	for _, a := range accs {
		if a.Kind != core.KindLocalArchive && !a.ReceiveOnly { // no sender
			acc := Account{ID: a.ID, Email: a.Email, Identities: []Identity{}}
			for _, i := range ids {
				if i.AccountID == a.ID {
					acc.Identities = append(acc.Identities, Identity{ID: i.ID, Name: i.Name, Email: i.Email})
				}
			}
			out = append(out, acc)
		}
	}
	return out, nil
}

// Latest is the draft as stored now, as Load, for resolving a conflict: its
// quote and signature become the ones saves keep.
func (p *ComposePage) Latest(ctx context.Context) (ComposeState, error) { return p.Load(ctx) }

// Save stores the edit in the calling window's draft, keeping its quote and
// signature. A draft changed elsewhere is a Conflict, not an error.
func (p *ComposePage) Save(ctx context.Context, e ComposeEdit) (SaveResult, error) {
	_, svc, s, err := p.open(ctx)
	if err != nil {
		return SaveResult{}, err
	}
	defer s.mu.Unlock()
	if e.Revision == 0 {
		return SaveResult{}, ErrNoRevision
	}
	// The save writes only what the edit gives, by ID: once loaded, no draft
	// is read for it (over the Local API, each read is the whole draft).
	// Deleted elsewhere, SaveDraft fails: never a new draft.
	d := core.Draft{ID: s.draftID}
	if !s.loaded { // saved before Load: keep what the draft has
		got, err := svc.GetDraft(ctx, s.draftID)
		if err != nil {
			return SaveResult{}, err
		}
		s.adopt(got)
		d = got
	}
	d.To, d.Cc, d.Bcc, d.ReplyTo, d.Subject = e.To, e.Cc, e.Bcc, e.ReplyTo, e.Subject
	// A marker in the body would pass for the quote or the signature.
	d.HTML = mail.JoinDraft(mail.StripMarkers(e.Body), s.quote, s.signature)
	d.ReadReceipt = s.readReceipt // SaveDraft writes it: keep what the page shows
	d.Revision = e.Revision
	if saveHook != nil {
		saveHook()
	}
	d, err = svc.SaveDraft(ctx, d)
	if errors.Is(err, core.ErrStaleDraft) {
		return SaveResult{Conflict: true}, nil
	}
	if err != nil {
		return SaveResult{}, err
	}
	return SaveResult{Revision: d.Revision}, nil
}

// SetAccount moves the calling window's draft to account id, sent as its
// default identity, whose signature replaces the old one; revision must be
// the stored one, else Conflict.
func (p *ComposePage) SetAccount(ctx context.Context, id, revision int64) (Change, error) {
	_, svc, s, err := p.open(ctx)
	if err != nil {
		return Change{}, err
	}
	defer s.mu.Unlock()
	d, err := svc.SetDraftAccount(ctx, s.draftID, id, revision)
	if errors.Is(err, core.ErrStaleDraft) {
		return conflict, nil
	}
	if err != nil {
		return Change{}, err
	}
	s.adopt(d)
	return changeOf(ctx, svc, d)
}

// SetFrom sends the calling window's draft from account as its identity
// (0: the account's own address), as the From menu picks it: the
// identity's signature (else the account's) replaces the old one, and its
// Reply-To may replace the draft's (Change.ReplyTo). revision must be the
// stored one, else Conflict.
func (p *ComposePage) SetFrom(ctx context.Context, account, identity, revision int64) (FromChange, error) {
	_, svc, s, err := p.open(ctx)
	if err != nil {
		return FromChange{}, err
	}
	defer s.mu.Unlock()
	d, err := svc.SetDraftFrom(ctx, s.draftID, account, identity, revision)
	if errors.Is(err, core.ErrStaleDraft) {
		return FromChange{Change: conflict}, nil
	}
	if err != nil {
		return FromChange{}, err
	}
	s.adopt(d)
	ch, err := changeOf(ctx, svc, d)
	return FromChange{Change: ch, ReplyTo: d.ReplyTo}, err
}

// SetOpenPGP sets whether the calling window's draft is signed and how it
// is encrypted (core.OpenPGPEncryptOff, Auto or On); revision must be the
// stored one, else Conflict.
func (p *ComposePage) SetOpenPGP(ctx context.Context, sign bool, encrypt string, revision int64) (Change, error) {
	_, svc, s, err := p.open(ctx)
	if err != nil {
		return Change{}, err
	}
	defer s.mu.Unlock()
	d, err := svc.SetDraftOpenPGP(ctx, s.draftID, revision, core.DraftOpenPGP{Sign: sign, Encrypt: encrypt})
	if errors.Is(err, core.ErrStaleDraft) {
		return conflict, nil
	}
	if err != nil {
		return Change{}, err
	}
	return changeOf(ctx, svc, d)
}

// OpenPGPMissing lists the addresses among addrs with no valid OpenPGP key
// in the user's keyring, which a message cannot be encrypted to. It reads
// the local keyring only; an address that does not parse yet is skipped.
func (p *ComposePage) OpenPGPMissing(ctx context.Context, addrs []string) ([]string, error) {
	_, svc, s, err := p.open(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Unlock() // gpg may take a moment: saves need not wait for it
	var valid []string
	for _, a := range addrs {
		if _, err := netmail.ParseAddress(a); err == nil {
			valid = append(valid, a)
		}
	}
	keys, err := svc.OpenPGPRecipientKeys(ctx, valid)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, k := range keys {
		if k.Key == "" {
			out = append(out, k.Address)
		}
	}
	return out, nil
}

// openpgpOf is d's OpenPGP choices for the page.
func openpgpOf(ctx context.Context, svc Service, d core.Draft) (OpenPGP, error) {
	a, err := svc.AccountOpenPGP(ctx, d.AccountID)
	if err != nil {
		return OpenPGP{}, err
	}
	return OpenPGP{Available: a.Key != "", Sign: d.OpenPGP.Sign, Encrypt: d.OpenPGP.Encrypt}, nil
}

// SetFormat saves the calling window's draft in format, core.FormatHTML or
// core.FormatPlain, its body turned into it (core.Draft.Format), if the
// draft is at revision (else Conflict): the draft as stored after.
func (p *ComposePage) SetFormat(ctx context.Context, format string, revision int64) (BodyChange, error) {
	_, svc, s, err := p.open(ctx)
	if err != nil {
		return BodyChange{}, err
	}
	defer s.mu.Unlock()
	if format != core.FormatHTML && format != core.FormatPlain {
		return BodyChange{}, fmt.Errorf("unknown draft format %q", format)
	}
	d, err := svc.GetDraft(ctx, s.draftID)
	if err != nil {
		return BodyChange{}, err
	}
	if d.Revision != revision {
		return BodyChange{Change: conflict}, nil
	}
	d.Format = format
	_, err = svc.SaveDraft(ctx, d)
	return bodyChange(ctx, svc, s, err)
}

// AddAttachments asks for files in a dialog on the calling window and adds
// each to its draft, if the draft is at revision (else Conflict, without the
// dialog). A file over the limit or failing is reported, not an error; a
// write from elsewhere stops the adding with Conflict, the files added so
// far in Attachments.
//
// The dialog runs without the session lock: Release does not wait for it
// (AppKit gives no promise a sheet on a closed window ever ends), and once
// the window is released the files are dropped (ErrNoDraft).
func (p *ComposePage) AddAttachments(ctx context.Context, revision int64) (AttachResult, error) {
	res := AttachResult{TooLarge: []string{}, Failed: []string{}}
	win, svc, s, err := p.open(ctx)
	if err != nil {
		return AttachResult{}, err
	}
	d, err := svc.GetDraft(ctx, s.draftID)
	s.mu.Unlock()
	if err != nil {
		return AttachResult{}, err
	}
	if d.Revision != revision {
		res.Change = conflict
		return res, nil
	}
	if p.d.PickFiles == nil {
		return AttachResult{}, ErrNoPicker
	}
	paths, err := p.d.PickFiles(ctx, win)
	if err != nil {
		return AttachResult{}, err
	}
	_, svc, s, err = p.open(ctx)
	if err != nil {
		return AttachResult{}, err
	}
	defer s.mu.Unlock()
	rev, stale := revision, false
	for _, path := range paths {
		_, now, err := svc.AddAttachmentAt(ctx, s.draftID, rev, path)
		if now != 0 {
			rev = now
		}
		switch {
		case errors.Is(err, core.ErrStaleDraft):
			stale = true
		case errors.Is(err, core.ErrTooLarge):
			res.TooLarge = append(res.TooLarge, filepath.Base(path))
		case err != nil:
			log.Printf("attaching %s to draft %d: %v", path, s.draftID, err)
			res.Failed = append(res.Failed, filepath.Base(path))
		}
		if stale {
			break
		}
	}
	if d, err = svc.GetDraft(ctx, s.draftID); err != nil {
		return AttachResult{}, err
	}
	if res.Change, err = changeOf(ctx, svc, d); err != nil {
		return AttachResult{}, err
	}
	res.Conflict = stale || d.Revision != rev // a write from elsewhere, even with no file picked
	return res, nil
}

// RemoveAttachment removes attachment id from the calling window's draft,
// if the draft is at revision (else Conflict).
func (p *ComposePage) RemoveAttachment(ctx context.Context, id, revision int64) (Change, error) {
	_, svc, s, err := p.open(ctx)
	if err != nil {
		return Change{}, err
	}
	defer s.mu.Unlock()
	rev, err := svc.RemoveAttachmentAt(ctx, s.draftID, id, revision)
	if errors.Is(err, core.ErrStaleDraft) {
		return conflict, nil
	}
	if err != nil {
		return Change{}, err
	}
	d, err := svc.GetDraft(ctx, s.draftID)
	if err != nil {
		return Change{}, err
	}
	ch, err := changeOf(ctx, svc, d)
	ch.Conflict = d.Revision != rev // a write from elsewhere since
	return ch, err
}

// Send sends exactly revision of the calling window's draft, else Conflict.
// digest, from an earlier SendResult's warnings, sends despite them. at, in
// unix milliseconds, sends it later, then (core.Service.SendLater); 0 now.
// Once sent, the window closes (OnDone).
func (p *ComposePage) Send(ctx context.Context, revision int64, digest string, at int64) (SendResult, error) {
	win, svc, s, err := p.open(ctx)
	if err != nil {
		return SendResult{}, err
	}
	if at != 0 {
		err = svc.SendLater(ctx, s.draftID, revision, time.UnixMilli(at), digest)
	} else if digest == "" {
		err = svc.SendRevision(ctx, s.draftID, revision)
	} else {
		err = svc.SendAcknowledgedRevision(ctx, s.draftID, revision, digest)
	}
	res := SendResult{Warnings: []SendWarning{}}
	var we *core.SendWarningsError
	var be *core.SendBlockedError
	switch {
	case err == nil:
		res.Sent, s.gone = true, true
	case errors.Is(err, core.ErrStaleDraft):
		res.Conflict, err = true, nil
	case errors.As(err, &we):
		for _, w := range we.Check.Warnings {
			res.Warnings = append(res.Warnings, SendWarning{Plugin: w.Plugin, Message: w.Message})
		}
		res.Digest, err = we.Check.Digest, nil
	case errors.As(err, &be):
		res.Blocked, err = &SendWarning{Plugin: be.Plugin, Message: be.Message}, nil
	default:
		res = SendResult{}
	}
	s.mu.Unlock()
	if res.Sent {
		p.c.finish(win)
	}
	return res, err
}

// PopupMenu shows entries as a native menu at x, y, CSS pixels from the
// top-left of the calling window's page, and returns the ID of the entry
// chosen, "" for none: the menus of the page's own controls, in the limits
// the Wit page's are (WitPage.PopupMenu).
func (p *ComposePage) PopupMenu(ctx context.Context, entries []MenuEntry, x, y int) (string, error) {
	win, ok := p.d.WinOf(ctx)
	if !ok || p.d.PopupMenu == nil {
		return "", ErrNoDraft
	}
	left := maxMenuEntries
	if err := checkMenu(entries, 0, &left); err != nil {
		return "", err
	}
	return p.d.PopupMenu(win, entries, x, y), nil
}

// SendLater opens send later, the native dialog choosing a date and time,
// on the calling window at at, unix ms, and returns the time chosen, 0 when
// cancelled. It schedules nothing: the page sends at the time it returns.
func (p *ComposePage) SendLater(ctx context.Context, at int64) (int64, error) {
	win, ok := p.d.WinOf(ctx)
	if !ok || p.d.SendLater == nil {
		return 0, ErrNoDraft
	}
	return p.d.SendLater(win, at), nil
}

// Closed is the page's final flush for the close attempt gen, the payload
// of the Closing event it answers: saved reports whether it succeeded.
func (p *ComposePage) Closed(ctx context.Context, gen int, saved bool) error {
	id, ok := p.d.WinOf(ctx)
	if !ok {
		return ErrNoDraft
	}
	p.c.mu.Lock()
	_, ok = p.c.sessions[id]
	fn := p.c.closed
	p.c.mu.Unlock()
	if !ok {
		return ErrNoDraft
	}
	if fn != nil {
		fn(id, gen, saved)
	}
	return nil
}

// conflict is the Change for a draft not at the revision the page passed.
var conflict = Change{Conflict: true, Attachments: []Attachment{}}

// changeOf is d's state after a change.
func changeOf(ctx context.Context, svc Service, d core.Draft) (Change, error) {
	_, _, sig := mail.SplitDraft(d.HTML)
	shown, err := signatureOf(ctx, svc, d, sig)
	if err != nil {
		return Change{}, err
	}
	pgp, err := openpgpOf(ctx, svc, d)
	return Change{Revision: d.Revision, Attachments: attachments(d), SignatureHTML: shown,
		UpdatedAt: d.UpdatedAt.UnixMilli(), OpenPGP: pgp}, err
}

// attachments is d's attachments for the page, never nil.
func attachments(d core.Draft) []Attachment {
	out := []Attachment{}
	for _, a := range d.Attachments {
		out = append(out, Attachment{ID: a.ID, Filename: a.Filename, Size: a.Size})
	}
	return out
}

func headers(d core.Draft) []Header {
	out := []Header{}
	for _, h := range d.Headers {
		out = append(out, Header{Name: h.Name, Value: h.Value})
	}
	return out
}

// nz is l, or an empty list for nil, so JSON carries [] and not null.
func nz(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}
