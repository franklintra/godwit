package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// What the interface opens for OpenDraft and OpenMessage, as `godwit
// --open` takes it (app.App.Open): a draft in a compose window, a message
// in the main window.
const (
	OpenDraftPrefix   = "draft:"
	OpenMessagePrefix = "message:"
)

// OpenTarget is what `godwit --open` opens for prefix (OpenDraftPrefix,
// OpenMessagePrefix) and id.
func OpenTarget(prefix string, id int64) string { return prefix + strconv.FormatInt(id, 10) }

// ParseOpenTarget reads an OpenTarget of prefix; false for anything else.
func ParseOpenTarget(what, prefix string) (int64, bool) {
	num, ok := strings.CutPrefix(what, prefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(num, 10, 64)
	return id, err == nil && id > 0
}

// ErrNoWindow: OpenDraft or OpenMessage while the backend can show no
// Godwit window, as `godwit --headless` without a desktop session (over
// SSH, a Linux server).
var ErrNoWindow = errors.New("Godwit cannot show a window here: its backend runs without a desktop session")

// OpenDraft shows draft draftID to the user in a compose window of Godwit's
// interface, as the user's own click would, and brings Godwit to the front:
// a connected interface is asked on its events, else the backend starts one
// (Options.ShowUI). It sends nothing, and changes neither the draft nor its
// program mark (ADR 0043): the user reviews it there and sends it, or not.
// A draft in the outbox is ErrStaleDraft, as for SaveDraft.
func (s *Service) OpenDraft(ctx context.Context, draftID int64) error {
	if _, err := s.GetDraft(ctx, draftID); err != nil {
		return err
	}
	if err := s.checkWritable(ctx, draftID); err != nil {
		return err
	}
	return s.showUI(OpenTarget(OpenDraftPrefix, draftID))
}

// OpenMessage shows message messageID to the user in Godwit's main window,
// as a click on its notification does: its conversation in the folder it
// shows in. It reads nothing for the caller and changes no mail beyond what
// showing it in the reader does for the user.
func (s *Service) OpenMessage(ctx context.Context, messageID int64) error {
	id, err := s.resolveID(ctx, messageID)
	if err != nil {
		return err
	}
	rows, err := s.db.Query(ctx, "SELECT 1 FROM messages m WHERE m.id = ? AND "+notPurgedCond, id)
	if err != nil {
		return err
	}
	if len(rows.Values) == 0 {
		return fmt.Errorf("message %d not found", messageID)
	}
	return s.showUI(OpenTarget(OpenMessagePrefix, id))
}

// showUI hands what to Options.ShowUI: ErrNoWindow without one.
func (s *Service) showUI(what string) error {
	if s.opts.ShowUI == nil {
		return ErrNoWindow
	}
	return s.opts.ShowUI(what)
}
