package core

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/godwit-email/godwit/core/store"
)

// MessageRef is a message found by its Message-ID (FindMessagesByMessageID):
// its summary, which carries its thread and the folder it shows in, and the
// account it is in.
type MessageRef struct {
	MessageSummary
	AccountID int64
	MessageID string // the RFC 5322 Message-ID, without angle brackets
}

// maxMessageIDMatches caps FindMessagesByMessageID: one Message-ID names one
// message, stored once per account that has it.
const maxMessageIDMatches = 50

// errNoMessageID: FindMessagesByMessageID was given no Message-ID.
var errNoMessageID = errors.New("invalid argument: messageID (expected an RFC 5322 Message-ID)")

// messageIDCond is the condition on messages alias m that its Message-ID is
// the argument. The account list lets SQLite search messages_account_mid
// (account_id, message_id) once per account rather than scan every message.
const messageIDCond = "m.account_id IN (SELECT id FROM accounts) AND m.message_id = ?"

// NormalizeMessageID is a Message-ID as Godwit stores it: without the angle
// brackets of the header (<id@host>) or the mid: URL form (RFC 2392, its
// content-ID part dropped), and without surrounding space. "" when nothing
// is left.
func NormalizeMessageID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 4 && strings.EqualFold(s[:4], "mid:") {
		s, _, _ = strings.Cut(s[4:], "/")
		if u, err := url.PathUnescape(s); err == nil {
			s = u
		}
	}
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "<>"))
}

// bracketedMessageID is the Message-ID of text that is exactly one, in
// angle brackets as a header writes it (<id@host>); false for anything else,
// such as words around it.
func bracketedMessageID(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if len(t) < 5 || t[0] != '<' || t[len(t)-1] != '>' {
		return "", false
	}
	inner := t[1 : len(t)-1]
	if !strings.Contains(inner, "@") || strings.ContainsAny(inner, "<> \t\r\n") {
		return "", false
	}
	return inner, true
}

// FindMessagesByMessageID finds the synced messages whose RFC 5322
// Message-ID is messageID, given with or without its angle brackets (or as
// a mid: URL), in every account: the same message is stored once in each
// account that has it. Newest first; mail being purged is left out. It
// reads stored data only.
func (s *Service) FindMessagesByMessageID(ctx context.Context, messageID string) ([]MessageRef, error) {
	mid := NormalizeMessageID(messageID)
	if mid == "" {
		return nil, errNoMessageID
	}
	rows, err := s.db.Query(ctx, "SELECT "+msgCols+", m.account_id FROM messages m WHERE "+messageIDCond+" AND "+notPurgedCond+
		" ORDER BY m.date DESC, m.id DESC LIMIT ?", mid, maxMessageIDMatches)
	if err != nil {
		return nil, err
	}
	sums := make([]MessageSummary, 0, len(rows.Values))
	for _, r := range rows.Values {
		sums = append(sums, scanSummary(r))
	}
	if err := s.fillFromNames(ctx, sums); err != nil {
		return nil, err
	}
	out := make([]MessageRef, len(sums))
	for i, r := range rows.Values {
		out[i] = MessageRef{MessageSummary: sums[i], AccountID: store.Int(r[msgColCount]), MessageID: mid}
	}
	return out, nil
}
