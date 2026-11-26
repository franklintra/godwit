package core

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	netmail "net/mail"
	"os"
	"strings"
	"time"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/godwit-email/godwit/backend"
	"github.com/godwit-email/godwit/core/auth"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/core/mailsync"
	"github.com/godwit-email/godwit/core/store"
)

// Server drafts (ADR 0045): an account with ServerDrafts keeps a copy of each
// of its drafts in its Drafts folder, so other mail apps and tools see it. The
// draft stays the local row it always was; the copy follows it. A sync round
// stores a copy once the draft has been left unchanged for ServerDraftIdle,
// APPENDing the new one before removing the one it replaces, and removes the
// copy once the draft is sent or deleted. A draft another app put in the
// Drafts folder becomes a Godwit draft on demand (OpenServerDraft); its
// message is then the draft's copy.

// Header fields of a draft's server copy. DraftIDHeader names the draft
// across its copies, so an edit another app saves as a new message is still
// recognized; the mode and source keep a reply's link where a server drops
// In-Reply-To from a draft (Exchange does).
const (
	DraftIDHeader     = "X-Godwit-Draft-ID"
	DraftModeHeader   = "X-Godwit-Draft-Mode"
	DraftSourceHeader = "X-Godwit-Draft-Source"
)

// ServerDraftIdle is how long a draft stays unchanged before a sync stores
// it on the server; a closed compose window stores it at once
// (FlushServerDraft).
const ServerDraftIdle = 30 * time.Second

// ErrNotServerDraft: OpenServerDraft was given a message that is in no
// Drafts folder on its account's server.
var ErrNotServerDraft = errors.New("the message is not a draft on the server")

// notServerDraft is the condition on messages alias m that it is not the
// server copy of a draft (its Message-ID or local row a server_drafts row's).
const notServerDraft = `NOT EXISTS (SELECT 1 FROM server_drafts sd WHERE sd.account_id = m.account_id
	AND (sd.message_id = m.id OR sd.message_id_hdr != '' AND sd.message_id_hdr = m.message_id))`

// serverCopyHere is the condition on server_drafts alias sd that local
// storage holds its copy at a server location in the folder it was stored in.
const serverCopyHere = `EXISTS (SELECT 1 FROM messages m JOIN message_locations l ON l.message_id = m.id
	JOIN folders f ON f.id = l.folder_id WHERE m.account_id = sd.account_id AND f.account_id = sd.account_id
	AND f.server_ref = sd.folder AND l.server_ref IS NOT NULL
	AND (m.id = sd.message_id OR sd.message_id_hdr != '' AND m.message_id = sd.message_id_hdr))`

// copiedRevision is the newest revision of draft alias d that its account's
// server has, refused or removed by another app included (those wait for the
// next revision); 0 for none.
const copiedRevision = `IFNULL((SELECT MAX(sd.revision) FROM server_drafts sd WHERE sd.draft_id = d.id AND sd.account_id = d.account_id
	AND sd.state IN ('appending', 'stored', 'failed', 'gone')), 0)`

// serverDraft is a server_drafts row.
type serverDraft struct {
	id, draftID, messageID, revision int64
	token, mid, folder, ref, state   string
}

const serverDraftCols = "id, IFNULL(draft_id, 0), token, message_id_hdr, IFNULL(message_id, 0), revision, folder, IFNULL(server_ref, ''), state"

func scanServerDraft(r []any) serverDraft {
	return serverDraft{id: store.Int(r[0]), draftID: store.Int(r[1]), token: store.Str(r[2]), mid: store.Str(r[3]),
		messageID: store.Int(r[4]), revision: store.Int(r[5]), folder: store.Str(r[6]), ref: store.Str(r[7]), state: store.Str(r[8])}
}

func (sd serverDraft) at(ref string) backend.Location {
	return backend.Location{Folder: backend.FolderRef(sd.folder), Ref: backend.MessageRef(ref)}
}

// draftChanged has a sync store draftID's server copy once it has been left
// unchanged for ServerDraftIdle, and remove the copy an account it left
// holds. It changes nothing for an account that keeps drafts local.
func (s *Service) draftChanged(ctx context.Context, draftID int64) {
	rows, err := s.db.Query(ctx, `SELECT a.id FROM accounts a WHERE a.id IN (SELECT account_id FROM drafts WHERE id = ?) AND a.server_drafts = 1
		UNION SELECT account_id FROM server_drafts WHERE draft_id = ?`, draftID, draftID)
	if err != nil {
		return
	}
	at := s.clk().Now().Add(ServerDraftIdle)
	for _, r := range rows.Values {
		s.wakeWorkerAt(store.Int(r[0]), wakeServerDrafts, at)
	}
}

// draftGone wakes the workers of accounts holding a copy of a deleted
// draft: their next round removes it. accounts is serverDraftAccounts's
// answer from before the deletion.
func (s *Service) draftGone(accounts []int64) {
	for _, id := range accounts {
		s.wakeWorker(id)
	}
}

// serverDraftAccounts lists the accounts that hold a server copy of draftID.
func (s *Service) serverDraftAccounts(ctx context.Context, draftID int64) []int64 {
	rows, err := s.db.Query(ctx, "SELECT DISTINCT account_id FROM server_drafts WHERE draft_id = ?", draftID)
	if err != nil {
		return nil
	}
	out := make([]int64, 0, len(rows.Values))
	for _, r := range rows.Values {
		out = append(out, store.Int(r[0]))
	}
	return out
}

// FlushServerDraft stores the draft on its account's server at the account's
// next sync round, without waiting for ServerDraftIdle: its compose window
// closed. It does nothing for an account that keeps drafts local, or when
// the server already has this revision.
func (s *Service) FlushServerDraft(ctx context.Context, draftID int64) error {
	rows, err := s.db.Query(ctx, `SELECT d.account_id FROM drafts d JOIN accounts a ON a.id = d.account_id WHERE d.id = ? AND a.server_drafts = 1
		AND d.revision > `+copiedRevision, draftID)
	if err != nil || len(rows.Values) == 0 {
		return err
	}
	s.serverDraftFlush.Store(draftID, true)
	s.wakeWorker(store.Int(rows.Values[0][0]))
	return nil
}

// draftsFolder is the server reference of the account's Drafts folder, ""
// when it has none.
func (s *Service) draftsFolder(ctx context.Context, accountID int64) (backend.FolderRef, error) {
	id, _, err := mailsync.RoleFolder(ctx, s.db, accountID, "drafts")
	if err != nil || id == 0 {
		return "", err
	}
	rows, err := s.db.Query(ctx, "SELECT server_ref FROM folders WHERE id = ? AND server_ref IS NOT NULL", id)
	if err != nil || len(rows.Values) == 0 {
		return "", err
	}
	return backend.FolderRef(store.Str(rows.Values[0][0])), nil
}

// syncServerDrafts is a sync round's server drafts step, after the fetch:
// it settles APPENDs a crash left uncertain, notices copies another app
// changed or removed, stores the drafts that are due, removes the copies
// whose draft was sent, deleted, moved to another account or stored again,
// and forgets what is done. Copies Godwit already holds are still removed
// after ServerDrafts is turned off; nothing new is stored then.
func (s *Service) syncServerDrafts(ctx context.Context, recv backend.Receiver, accountID int64) error {
	s.serverDraftMu.Lock()
	defer s.serverDraftMu.Unlock()
	acc, err := s.account(ctx, accountID)
	if err != nil {
		return err
	}
	held, err := s.db.Query(ctx, "SELECT 1 FROM server_drafts WHERE account_id = ? LIMIT 1", accountID)
	if err != nil || !acc.ServerDrafts && len(held.Values) == 0 {
		return err
	}
	if err := s.settleAppends(ctx, recv, accountID); err != nil {
		return err
	}
	if err := s.checkServerCopies(ctx, recv, acc); err != nil {
		return err
	}
	if acc.ServerDrafts {
		if err := s.storeDueDrafts(ctx, recv, acc); err != nil {
			return err
		}
	}
	if err := s.removeStaleCopies(ctx, recv, acc); err != nil {
		return err
	}
	// Removed copies the sync no longer holds, and refusals a later copy
	// replaced or whose draft went, are forgotten.
	_, err = s.db.Execute(ctx, `DELETE FROM server_drafts AS sd WHERE sd.account_id = ? AND (
		sd.state = 'removed' AND NOT `+serverCopyHere+`
		OR sd.state = 'failed' AND (sd.draft_id IS NULL OR EXISTS (SELECT 1 FROM server_drafts n WHERE n.draft_id = sd.draft_id
			AND n.account_id = sd.account_id AND n.state = 'stored' AND n.id > sd.id)))`, accountID)
	return err
}

// settleAppends looks for the copies whose APPEND was cut off: one the
// server holds is stored, one it does not is forgotten and stored again.
func (s *Service) settleAppends(ctx context.Context, recv backend.Receiver, accountID int64) error {
	rows, err := s.db.Query(ctx, "SELECT "+serverDraftCols+" FROM server_drafts WHERE account_id = ? AND state = 'appending'", accountID)
	if err != nil {
		return err
	}
	for _, r := range rows.Values {
		sd := scanServerDraft(r)
		refs, err := recv.Find(ctx, backend.FolderRef(sd.folder), sd.mid, 0)
		if err != nil {
			return err
		}
		if len(refs) == 0 {
			_, err = s.db.Execute(ctx, "DELETE FROM server_drafts WHERE id = ?", sd.id)
		} else {
			_, err = s.db.Execute(ctx, "UPDATE server_drafts SET state = 'stored', server_ref = ? WHERE id = ?", string(refs[0]), sd.id)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// storeDueDrafts stores each of the account's drafts whose newest revision
// has no copy yet, once it has been left alone for ServerDraftIdle or its
// window closed, and has the worker wake for the next one that comes due.
// A draft in the outbox waits: it is read-only until taken back, and its
// copy goes once it is sent.
func (s *Service) storeDueDrafts(ctx context.Context, recv backend.Receiver, acc Account) error {
	folder, err := s.draftsFolder(ctx, acc.ID)
	if err != nil || folder == "" {
		return err // no Drafts folder (yet): nothing to store into
	}
	rows, err := s.db.Query(ctx, `SELECT d.id, d.updated_at FROM drafts d WHERE d.account_id = ?
		AND d.id NOT IN (SELECT draft_id FROM outbox WHERE draft_id IS NOT NULL)
		AND d.revision > `+copiedRevision+` ORDER BY d.id`, acc.ID)
	if err != nil {
		return err
	}
	now := s.clk().Now()
	var next time.Time
	for _, r := range rows.Values {
		id, changed := store.Int(r[0]), time.Unix(store.Int(r[1]), 0)
		_, flushed := s.serverDraftFlush.LoadAndDelete(id)
		if due := changed.Add(ServerDraftIdle); !flushed && due.After(now) {
			if next.IsZero() || due.Before(next) {
				next = due
			}
			continue
		}
		if err := s.storeDraft(ctx, recv, acc, folder, id); err != nil {
			return err
		}
	}
	s.wakeWorkerAt(acc.ID, wakeServerDrafts, next)
	return nil
}

// draftToken is the X-Godwit-Draft-ID of the draft's copies, a new one if it
// has none yet.
func (s *Service) draftToken(ctx context.Context, draftID int64) (string, error) {
	rows, err := s.db.Query(ctx, "SELECT token FROM server_drafts WHERE draft_id = ? ORDER BY id DESC LIMIT 1", draftID)
	if err != nil {
		return "", err
	}
	if len(rows.Values) > 0 {
		return store.Str(rows.Values[0][0]), nil
	}
	return strings.ToLower(rand.Text()), nil
}

// storeDraft APPENDs the draft as it is now to folder. The row is written
// first, as appending: a crash during the APPEND leaves it for
// settleAppends, and the copy is hidden from the lists from the start. A
// refusal is kept as failed, so that revision is not tried again.
func (s *Service) storeDraft(ctx context.Context, recv backend.Receiver, acc Account, folder backend.FolderRef, draftID int64) error {
	d, err := s.GetDraft(ctx, draftID)
	if errors.Is(err, errDraftNotFound) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	token, err := s.draftToken(ctx, draftID)
	if err != nil {
		return err
	}
	raw, mid, err := s.serverDraftRaw(ctx, d, token)
	if errors.Is(err, ErrTooLarge) {
		slog.Warn("draft too large to store on the server", "account", acc.ID, "draft", draftID)
		_, err = s.db.Execute(ctx, `INSERT INTO server_drafts(account_id, draft_id, token, message_id_hdr, revision, folder, state, created_at)
			VALUES (?, ?, ?, '', ?, ?, 'failed', ?)`, acc.ID, draftID, token, d.Revision, string(folder), s.clk().Now().Unix())
		return err
	}
	if err != nil {
		return err
	}
	r, err := s.db.DurableExecute(ctx, `INSERT INTO server_drafts(account_id, draft_id, token, message_id_hdr, revision, folder, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'appending', ?)`, acc.ID, draftID, token, mid, d.Revision, string(folder), s.clk().Now().Unix())
	if err != nil {
		return err
	}
	row := r.LastInsertID
	res, err := recv.Apply(ctx, backend.Append{To: folder, Raw: raw, Flags: backend.Flags{Seen: true, Draft: true}, MessageID: mid})
	if err != nil {
		return err // maybe stored: settleAppends finds out
	}
	ref := res.Ref
	switch res.Outcome {
	case backend.Failed, backend.Unsupported:
		slog.Warn("the server refused to store a draft", "account", acc.ID, "draft", draftID, "reason", res.Reason)
		_, err = s.db.Execute(ctx, "UPDATE server_drafts SET state = 'failed' WHERE id = ?", row)
		return err
	case backend.Unlocated: // no APPENDUID: look it up, else removal looks again
		refs, err := recv.Find(ctx, folder, mid, int64(len(raw)))
		if err != nil {
			return err
		}
		if len(refs) > 0 {
			ref = refs[0]
		}
	}
	_, err = s.db.Execute(ctx, "UPDATE server_drafts SET state = 'stored', server_ref = ? WHERE id = ?", nullStr(string(ref)), row)
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// serverDraftRaw is draft d as its server copy: the message it would send,
// with Bcc kept and the X-Godwit-Draft-* fields, under a new Message-ID.
// Placeholders and link references are restored as for a send; a draft that
// cannot be restored is stored as it is.
func (s *Service) serverDraftRaw(ctx context.Context, d Draft, token string) ([]byte, string, error) {
	x := d
	if s.expandDraft(ctx, &x) == nil {
		d = x
	}
	acc, err := s.account(ctx, d.AccountID)
	if err != nil {
		return nil, "", err
	}
	imgs, err := s.signatureImages(ctx, true)
	if err != nil {
		return nil, "", err
	}
	o := mail.Outgoing{From: (&netmail.Address{Name: acc.Name, Address: acc.Email}).String(), ReplyTo: d.ReplyTo,
		To: d.To, Cc: d.Cc, Bcc: d.Bcc, Subject: d.Subject, HTML: d.HTML, Draft: true,
		Header: map[string]string{DraftIDHeader: token}}
	for _, id := range mail.CIDs(d.HTML) {
		if a, ok := imgs[id]; ok {
			delete(imgs, id)
			o.Related = append(o.Related, a)
		}
	}
	src, err := s.threadHeaders(ctx, d, &o)
	if err != nil {
		return nil, "", err
	}
	if d.Mode != "new" {
		o.Header[DraftModeHeader] = d.Mode
	}
	if src != "" {
		o.Header[DraftSourceHeader] = "<" + src + ">"
	}
	if o.Attachments, err = s.draftFiles(ctx, d.ID); err != nil {
		return nil, "", err
	}
	raw, mid, err := mail.Build(o, s.clk().Now())
	if err != nil {
		return nil, "", err
	}
	if int64(len(raw)) > attachLimit {
		return nil, "", ErrTooLarge
	}
	return raw, mid, nil
}

// removeStaleCopies removes the copies that no draft of the account needs
// any more: its draft was sent or deleted, moved to another account, or
// stored again since. A copy whose location is not known is looked up by
// its Message-ID.
func (s *Service) removeStaleCopies(ctx context.Context, recv backend.Receiver, acc Account) error {
	rows, err := s.db.Query(ctx, `SELECT `+serverDraftCols+` FROM server_drafts sd WHERE sd.account_id = ? AND sd.state IN ('stored', 'gone') AND (
		sd.draft_id IS NULL
		OR NOT EXISTS (SELECT 1 FROM drafts d WHERE d.id = sd.draft_id AND d.account_id = sd.account_id)
		OR EXISTS (SELECT 1 FROM server_drafts n WHERE n.draft_id = sd.draft_id AND n.account_id = sd.account_id AND n.state = 'stored' AND n.id > sd.id))
		ORDER BY sd.id`, acc.ID)
	if err != nil {
		return err
	}
	for _, r := range rows.Values {
		sd := scanServerDraft(r)
		refs := []backend.MessageRef{backend.MessageRef(sd.ref)}
		if sd.ref == "" {
			if sd.mid == "" {
				refs = nil
			} else if refs, err = recv.Find(ctx, backend.FolderRef(sd.folder), sd.mid, 0); err != nil {
				return err
			}
		}
		for _, ref := range refs {
			if err := s.removeCopy(ctx, recv, acc, sd.at(string(ref))); err != nil {
				return err
			}
		}
		if _, err := s.db.Execute(ctx, "UPDATE server_drafts SET state = 'removed' WHERE id = ?", sd.id); err != nil {
			return err
		}
	}
	return nil
}

// removeCopy deletes a draft's copy for good: UID EXPUNGE with UIDPLUS, the
// backend's permanent delete elsewhere. An IMAP server without UIDPLUS gets
// the copy flagged \Deleted and no EXPUNGE, which would also remove what
// other apps flagged: mail apps hide it, and the next EXPUNGE removes it. A
// copy already gone is fine; a refusal is logged and given up.
func (s *Service) removeCopy(ctx context.Context, recv backend.Receiver, acc Account, at backend.Location) error {
	res, err := recv.Apply(ctx, backend.Purge{At: at})
	if err == nil && res.Outcome == backend.Unsupported && acc.Receiving.Protocol == auth.IMAP {
		res, err = recv.Apply(ctx, backend.SetFlags{At: at, Add: backend.Flags{Keywords: []string{`\Deleted`}}})
	}
	switch {
	case errors.Is(err, backend.ErrNotFound):
		return nil
	case err != nil:
		return err
	case res.Outcome == backend.Failed || res.Outcome == backend.Unsupported:
		slog.Warn("a draft's old copy was not removed from the server", "account", acc.ID, "folder", at.Folder, "reason", res.Reason)
	}
	return nil
}

// checkServerCopies marks the copies the sync has stored here as seen, and
// looks into those seen once and now gone from the Drafts folder: another
// app changed the draft (its new message carries the draft's
// DraftIDHeader) or removed it (sent or deleted it). Last writer wins, and
// the other version is kept as a draft of its own (serverDraftConflict).
// A removed copy's draft stays, and is stored again only once it changes.
func (s *Service) checkServerCopies(ctx context.Context, recv backend.Receiver, acc Account) error {
	if _, err := s.db.Execute(ctx, "UPDATE server_drafts AS sd SET seen = 1 WHERE sd.account_id = ? AND sd.state = 'stored' AND sd.seen = 0 AND "+serverCopyHere, acc.ID); err != nil {
		return err
	}
	rows, err := s.db.Query(ctx, "SELECT "+serverDraftCols+" FROM server_drafts sd WHERE sd.account_id = ? AND sd.state = 'stored' AND sd.seen = 1 AND sd.draft_id IS NOT NULL AND NOT "+serverCopyHere+" ORDER BY sd.id", acc.ID)
	if err != nil || len(rows.Values) == 0 {
		return err
	}
	var edits map[string]foreignDraft
	for _, r := range rows.Values {
		sd := scanServerDraft(r)
		newer, err := s.db.Query(ctx, "SELECT 1 FROM server_drafts WHERE draft_id = ? AND account_id = ? AND id > ? AND state IN ('stored', 'appending') LIMIT 1", sd.draftID, acc.ID, sd.id)
		if err != nil {
			return err
		}
		if len(newer.Values) > 0 {
			continue // replaced by Godwit itself: removeStaleCopies forgets it
		}
		if edits == nil && acc.ServerDrafts {
			if edits, err = s.foreignDrafts(ctx, recv, acc.ID, sd.folder); err != nil {
				return err
			}
		}
		f, edited := edits[sd.token]
		if !edited {
			if _, err := s.db.Execute(ctx, "UPDATE server_drafts SET state = 'gone' WHERE id = ?", sd.id); err != nil {
				return err
			}
			s.bus.Publish(events.Event{Type: events.DraftConflict, AccountID: acc.ID, Kind: "server_gone"})
			continue
		}
		delete(edits, sd.token)
		if err := s.serverDraftConflict(ctx, recv, acc, sd, f); err != nil {
			return err
		}
	}
	return nil
}

// foreignDraft is a message in a Drafts folder that is no draft's copy yet.
type foreignDraft struct {
	messageID int64
	mid, ref  string
	date      time.Time
}

// foreignDrafts reads the DraftIDHeader of the messages in folder that are
// no draft's copy, the newest 200, by its value.
func (s *Service) foreignDrafts(ctx context.Context, recv backend.Receiver, accountID int64, folder string) (map[string]foreignDraft, error) {
	rows, err := s.db.Query(ctx, `SELECT m.id, IFNULL(m.message_id, ''), l.server_ref, IFNULL(m.date, 0) FROM messages m
		JOIN message_locations l ON l.message_id = m.id JOIN folders f ON f.id = l.folder_id
		WHERE m.account_id = ? AND f.account_id = ? AND f.server_ref = ? AND l.server_ref IS NOT NULL AND `+notServerDraft+`
		ORDER BY m.date DESC, m.id DESC LIMIT 200`, accountID, accountID, folder)
	if err != nil || len(rows.Values) == 0 {
		return map[string]foreignDraft{}, err
	}
	byRef := map[backend.MessageRef]foreignDraft{}
	refs := make([]backend.MessageRef, 0, len(rows.Values))
	for _, r := range rows.Values {
		ref := backend.MessageRef(store.Str(r[2]))
		byRef[ref] = foreignDraft{messageID: store.Int(r[0]), mid: store.Str(r[1]), ref: string(ref), date: time.Unix(store.Int(r[3]), 0)}
		refs = append(refs, ref)
	}
	hdrs, err := recv.Headers(ctx, backend.FolderRef(folder), refs, []string{DraftIDHeader})
	if err != nil {
		return nil, err
	}
	out := map[string]foreignDraft{}
	for ref, h := range hdrs {
		if tok := headerValue(h, DraftIDHeader); tok != "" {
			if f, ok := out[tok]; !ok || byRef[ref].date.After(f.date) {
				out[tok] = byRef[ref]
			}
		}
	}
	return out, nil
}

// headerValue is field's value in a raw header block, "" if it has none.
func headerValue(raw []byte, field string) string {
	m, err := netmail.ReadMessage(bytes.NewReader(append(bytes.TrimRight(raw, "\r\n"), "\r\n\r\n"...)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(m.Header.Get(field))
}

// serverDraftConflict settles a draft another app saved again as message f
// while its copy sd was stored: the version saved last wins, the draft's
// (updated_at) or the message's (its date). The other is kept as a new draft,
// so no text is lost, and the interface is told (DraftConflict). f becomes
// the draft's copy: the one it holds if the server's won, else one the next
// copy replaces.
func (s *Service) serverDraftConflict(ctx context.Context, recv backend.Receiver, acc Account, sd serverDraft, f foreignDraft) error {
	d, err := s.GetDraft(ctx, sd.draftID)
	if err != nil {
		return err
	}
	raw, err := fetchAll(ctx, recv, sd.at(f.ref))
	if errors.Is(err, backend.ErrNotFound) {
		return nil // gone again: the next round looks once more
	}
	if err != nil {
		return err
	}
	theirs, files, err := s.parseServerDraft(ctx, acc.ID, raw)
	if err != nil {
		return err
	}
	serverWins := f.date.After(d.UpdatedAt)
	kind, rev := "local_newer", int64(0)
	if serverWins {
		kind = "server_newer"
		if _, err := s.copyDraft(ctx, d); err != nil {
			return err
		}
		if rev, err = s.replaceDraft(ctx, d.ID, theirs, files); err != nil {
			return err
		}
	} else {
		theirs.Mode, theirs.SourceMessageID = d.Mode, d.SourceMessageID
		if _, err := s.createDraft(ctx, theirs, files, d.FromProgram); err != nil {
			return err
		}
		s.serverDraftFlush.Store(d.ID, true) // the draft replaces their copy now
	}
	if _, err := s.db.Transaction(ctx, []sqlite.Statement{
		{SQL: "UPDATE server_drafts SET state = 'removed' WHERE id = ?", Args: []any{sd.id}},
		{SQL: `INSERT INTO server_drafts(account_id, draft_id, token, message_id_hdr, message_id, revision, folder, server_ref, state, seen, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'stored', 1, ?)`,
			Args: []any{acc.ID, d.ID, sd.token, f.mid, nullID(f.messageID), rev, sd.folder, f.ref, s.clk().Now().Unix()}},
	}); err != nil {
		return err
	}
	s.bus.Publish(events.Event{Type: events.DraftConflict, AccountID: acc.ID, Kind: kind})
	s.bus.Publish(events.Event{Type: events.DraftsChanged, AccountID: acc.ID})
	return nil
}

// fetchAll reads the whole message at at, up to attachLimit.
func fetchAll(ctx context.Context, recv backend.Receiver, at backend.Location) ([]byte, error) {
	rc, err := recv.Fetch(ctx, backend.ContentRequest{Location: at})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, attachLimit+1))
	if err == nil && int64(len(raw)) > attachLimit {
		err = ErrTooLarge
	}
	return raw, err
}

// parseServerDraft reads a draft another app saved: its addresses (Bcc
// included), subject, body (HTML, else the text as paragraphs) and
// attachments. A reply whose original is here (by DraftSourceHeader,
// In-Reply-To or the last of References) becomes a reply to it, a reply-all
// or forward when DraftModeHeader says so; anything else is a new message.
// Inline images other than the account's signature images are dropped, as
// a draft cannot show them.
func (s *Service) parseServerDraft(ctx context.Context, accountID int64, raw []byte) (Draft, []mail.Attachment, error) {
	p, err := mail.Parse(raw)
	if err != nil {
		return Draft{}, nil, err
	}
	h, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Draft{}, nil, err
	}
	d := Draft{AccountID: accountID, Mode: "new", To: p.To, Cc: p.Cc, ReplyTo: p.ReplyTo, Subject: p.Subject, HTML: p.HTML}
	if bcc, err := h.Header.AddressList("Bcc"); err == nil {
		for _, a := range bcc {
			d.Bcc = append(d.Bcc, mail.FormatAddress(a.Name, a.Address))
		}
	}
	if d.HTML == "" && p.Text != "" {
		d.HTML = "<p>" + escapeLines(p.Text) + "</p>"
	}
	ids := []string{strings.Trim(strings.TrimSpace(h.Header.Get(DraftSourceHeader)), "<>"), p.InReplyTo}
	if len(p.References) > 0 {
		ids = append(ids, p.References[len(p.References)-1])
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		rows, err := s.db.Query(ctx, "SELECT m.id FROM messages m WHERE m.account_id = ? AND m.message_id = ? AND "+notServerDraft+" ORDER BY m.id LIMIT 1", accountID, id)
		if err != nil {
			return Draft{}, nil, err
		}
		if len(rows.Values) > 0 {
			d.SourceMessageID, d.Mode = store.Int(rows.Values[0][0]), "reply"
			if m := strings.TrimSpace(h.Header.Get(DraftModeHeader)); m == "reply_all" || m == "forward" {
				d.Mode = m
			}
			break
		}
	}
	var files []mail.Attachment
	for _, a := range p.Attachments {
		if !a.Inline && len(a.Data) > 0 {
			files = append(files, a)
		}
	}
	return d, files, nil
}

// createDraft saves d as a new draft with files attached, marked as a
// program's when program is set; a failed attachment deletes it again.
func (s *Service) createDraft(ctx context.Context, d Draft, files []mail.Attachment, program bool) (Draft, error) {
	d.ID, d.Revision = 0, 0
	nd, err := s.saveDraft(ctx, d)
	if err != nil {
		return Draft{}, err
	}
	for _, f := range files {
		if _, _, err := s.addAttachment(ctx, nd.ID, -1, cmp.Or(f.Filename, "attachment"), cmp.Or(f.ContentType, "application/octet-stream"), f.Data); err != nil {
			return Draft{}, errors.Join(err, s.deleteDraft(context.WithoutCancel(ctx), nd.ID))
		}
	}
	if program && !nd.FromProgram {
		if _, err := s.db.Execute(ctx, "UPDATE drafts SET from_program = 1 WHERE id = ?", nd.ID); err != nil {
			return Draft{}, err
		}
	}
	return s.GetDraft(ctx, nd.ID)
}

// copyDraft keeps a copy of d, attachments included, as a new draft: the
// version a conflict did not keep.
func (s *Service) copyDraft(ctx context.Context, d Draft) (Draft, error) {
	files, err := s.draftFiles(ctx, d.ID)
	if err != nil {
		return Draft{}, err
	}
	return s.createDraft(ctx, d, files, d.FromProgram)
}

// replaceDraft overwrites the addresses, subject, body and attachments of
// draft id with d's and files, whatever its revision: a compose window
// editing it then finds it changed elsewhere (ErrStaleDraft). A draft in the
// outbox stays (ErrStaleDraft). It returns the draft's new revision.
func (s *Service) replaceDraft(ctx context.Context, id int64, d Draft, files []mail.Attachment) (int64, error) {
	html, err := s.sanitizeDraft(ctx, d.HTML)
	if err != nil {
		return 0, err
	}
	s.editMu.Lock()
	if err := s.checkWritable(ctx, id); err != nil {
		s.editMu.Unlock()
		return 0, err
	}
	old, err := s.db.Query(ctx, "SELECT path FROM draft_attachments WHERE draft_id = ?", id)
	if err == nil {
		_, err = s.db.DurableTransaction(ctx, []sqlite.Statement{
			{SQL: `UPDATE drafts SET to_addrs = ?, cc_addrs = ?, bcc_addrs = ?, reply_to = ?, subject = ?, body_html = ?,
				updated_at = ?, revision = revision + 1 WHERE id = ?`,
				Args: []any{encodeList(d.To), encodeList(d.Cc), encodeList(d.Bcc), d.ReplyTo, d.Subject, html, time.Now().Unix(), id}},
			{SQL: "DELETE FROM draft_attachments WHERE draft_id = ?", Args: []any{id}},
		})
	}
	s.editMu.Unlock()
	if err != nil {
		return 0, err
	}
	for _, r := range old.Values {
		if p := store.Str(r[0]); p != "" {
			os.Remove(p) // a file left behind is only an orphan (removeDraftDir)
		}
	}
	for _, f := range files {
		if _, _, err := s.addAttachment(ctx, id, -1, cmp.Or(f.Filename, "attachment"), cmp.Or(f.ContentType, "application/octet-stream"), f.Data); err != nil {
			return 0, err
		}
	}
	return s.draftRevision(ctx, id)
}

// OpenServerDraft makes the message, a draft another app saved in its
// account's Drafts folder, a Godwit draft and returns its ID, so it opens in
// a compose window. The message becomes the draft's server copy: hidden from
// the lists, replaced when the draft is stored (with ServerDrafts on) and
// removed when it is sent or deleted. A message already opened, or Godwit's
// own copy of a draft, gives that draft. A message in no Drafts folder on the
// server is ErrNotServerDraft. See parseServerDraft for what is kept.
func (s *Service) OpenServerDraft(ctx context.Context, messageID int64) (int64, error) {
	messageID, err := s.resolveID(ctx, messageID)
	if err != nil {
		return 0, err
	}
	rows, err := s.db.Query(ctx, "SELECT account_id, IFNULL(message_id, '') FROM messages WHERE id = ?", messageID)
	if err != nil {
		return 0, err
	}
	if len(rows.Values) == 0 {
		return 0, fmt.Errorf("message %d not found", messageID)
	}
	accountID, mid := store.Int(rows.Values[0][0]), store.Str(rows.Values[0][1])
	folder, err := s.draftsFolder(ctx, accountID)
	if err != nil {
		return 0, err
	}
	loc, err := s.db.Query(ctx, `SELECT l.server_ref FROM message_locations l JOIN folders f ON f.id = l.folder_id
		WHERE l.message_id = ? AND f.account_id = ? AND f.server_ref = ? AND l.server_ref IS NOT NULL`, messageID, accountID, string(folder))
	if err != nil {
		return 0, err
	}
	if folder == "" || len(loc.Values) == 0 {
		return 0, ErrNotServerDraft
	}
	ref := store.Str(loc.Values[0][0])
	if id, err := s.serverDraftOf(ctx, accountID, messageID, mid); id != 0 || err != nil {
		return id, err
	}
	raw, err := s.GetRawMessage(ctx, messageID)
	if err != nil {
		return 0, err
	}
	s.serverDraftMu.Lock()
	defer s.serverDraftMu.Unlock()
	if id, err := s.serverDraftOf(ctx, accountID, messageID, mid); id != 0 || err != nil {
		return id, err // opened meanwhile
	}
	d, files, err := s.parseServerDraft(ctx, accountID, raw)
	if err != nil {
		return 0, err
	}
	d, err = s.createDraft(ctx, d, files, false)
	if err != nil {
		return 0, err
	}
	token, err := s.draftToken(ctx, d.ID)
	if err == nil {
		_, err = s.db.Execute(ctx, `INSERT INTO server_drafts(account_id, draft_id, token, message_id_hdr, message_id, revision, folder, server_ref, state, seen, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'stored', 1, ?)`, accountID, d.ID, token, mid, messageID, d.Revision, string(folder), ref, s.clk().Now().Unix())
	}
	if err != nil {
		return 0, errors.Join(err, s.deleteDraft(context.WithoutCancel(ctx), d.ID))
	}
	s.bus.Publish(events.Event{Type: events.MessagesChanged, AccountID: accountID}) // the message leaves the lists
	return d.ID, nil
}

// serverDraftOf is the draft whose copy the message is, 0 for none.
func (s *Service) serverDraftOf(ctx context.Context, accountID, messageID int64, mid string) (int64, error) {
	rows, err := s.db.Query(ctx, `SELECT draft_id FROM server_drafts WHERE account_id = ? AND draft_id IS NOT NULL
		AND (message_id = ? OR message_id_hdr != '' AND message_id_hdr = ?) ORDER BY id DESC LIMIT 1`, accountID, messageID, mid)
	if err != nil || len(rows.Values) == 0 {
		return 0, err
	}
	return store.Int(rows.Values[0][0]), nil
}
