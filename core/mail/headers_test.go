package mail_test

import (
	"bytes"
	"errors"
	"mime"
	netmail "net/mail"
	"strings"
	"testing"
	"time"

	"github.com/godwit-email/godwit/core/mail"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckHeaders(t *testing.T) {
	got, err := mail.CheckHeaders([]mail.Header{
		{"x-hermes-session", "  abc 123\t"},
		{"Keywords", "budget, q3"},
		{"X-Tag", "one"},
		{"X-Tag", "two"},
		{"X-Note", "café ☕"},
	})
	require.NoError(t, err)
	assert.Equal(t, []mail.Header{
		{"X-Hermes-Session", "abc 123"}, // canonical case, trimmed, the tab a space
		{"Keywords", "budget, q3"},
		{"X-Tag", "one"},
		{"X-Tag", "two"}, // a name may repeat; order kept
		{"X-Note", "café ☕"},
	}, got)
	got, err = mail.CheckHeaders(nil)
	assert.NoError(t, err)
	assert.Nil(t, got, "nil stays nil: SaveDraft keeps the stored ones")
	got, err = mail.CheckHeaders([]mail.Header{})
	assert.NoError(t, err)
	assert.Equal(t, []mail.Header{}, got)

	for _, c := range []struct {
		h       mail.Header
		problem string
	}{
		// Header injection: a line break would end the field and start another.
		{mail.Header{"X-A", "a\r\nBcc: eve@example.com"}, mail.HeaderBadValue},
		{mail.Header{"X-A", "a\nb"}, mail.HeaderBadValue},
		{mail.Header{"X-A", "a\rb"}, mail.HeaderBadValue},
		{mail.Header{"X-A", "a\x00b"}, mail.HeaderBadValue},
		{mail.Header{"X-A", "a b"}, mail.HeaderBadValue},
		{mail.Header{"X-A", "a\u0085b"}, mail.HeaderBadValue},
		{mail.Header{"X-A", "\xff"}, mail.HeaderBadValue},
		{mail.Header{"X-A", "   "}, mail.HeaderBadValue},
		{mail.Header{"X-A", strings.Repeat("a", mail.MaxHeaderValue+1)}, mail.HeaderBadValue},
		{mail.Header{"X-A\r\nBcc", "eve@example.com"}, mail.HeaderBadName},
		{mail.Header{"X A", "1"}, mail.HeaderBadName},
		{mail.Header{"X-A:", "1"}, mail.HeaderBadName},
		{mail.Header{"X-É", "1"}, mail.HeaderBadName},
		{mail.Header{"", "1"}, mail.HeaderBadName},
		{mail.Header{strings.Repeat("X", mail.MaxHeaderName+1), "1"}, mail.HeaderBadName},
		// Godwit's own fields, MIME's, and those that send mail elsewhere.
		{mail.Header{"from", "eve@example.com"}, mail.HeaderReserved},
		{mail.Header{"BCC", "eve@example.com"}, mail.HeaderReserved},
		{mail.Header{"Reply-To", "eve@example.com"}, mail.HeaderReserved},
		{mail.Header{"Message-ID", "<x@y>"}, mail.HeaderReserved},
		{mail.Header{"In-Reply-To", "<x@y>"}, mail.HeaderReserved},
		{mail.Header{"References", "<x@y>"}, mail.HeaderReserved},
		{mail.Header{"Date", "today"}, mail.HeaderReserved},
		{mail.Header{"Subject", "x"}, mail.HeaderReserved},
		{mail.Header{"MIME-Version", "1.0"}, mail.HeaderReserved},
		{mail.Header{"Content-Type", "text/plain"}, mail.HeaderReserved},
		{mail.Header{"content-transfer-encoding", "8bit"}, mail.HeaderReserved},
		{mail.Header{mail.RuleHeader, "1"}, mail.HeaderReserved},
		{mail.Header{"X-Godwit-Anything", "1"}, mail.HeaderReserved},
		{mail.Header{"Disposition-Notification-To", "eve@example.com"}, mail.HeaderReserved},
		{mail.Header{"Mail-Followup-To", "eve@example.com"}, mail.HeaderReserved},
		{mail.Header{"Resent-To", "eve@example.com"}, mail.HeaderReserved},
		{mail.Header{"Sender", "eve@example.com"}, mail.HeaderReserved},
		{mail.Header{"DKIM-Signature", "v=1"}, mail.HeaderReserved},
		{mail.Header{"Autocrypt", "addr=a@x; keydata=AA"}, mail.HeaderReserved},
	} {
		_, err := mail.CheckHeaders([]mail.Header{{"X-Ok", "1"}, c.h})
		var he *mail.HeaderError
		require.True(t, errors.As(err, &he), "%q", c.h)
		assert.Equal(t, c.problem, he.Problem, "%q", c.h)
		assert.NotContains(t, err.Error(), "eve@example.com", "a value is never echoed")
	}

	many := make([]mail.Header, mail.MaxHeaders+1)
	for i := range many {
		many[i] = mail.Header{Name: "X-N", Value: "1"}
	}
	_, err = mail.CheckHeaders(many)
	var he *mail.HeaderError
	require.True(t, errors.As(err, &he))
	assert.Equal(t, mail.HeaderTooMany, he.Problem)
}

func TestBuildHeaders(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("word ", 150)) // 749 bytes: folded
	raw, _, err := mail.Build(mail.Outgoing{From: "me@x.com", To: []string{"a@y.com"}, Subject: "s", HTML: "<p>hi</p>",
		Header: map[string]string{mail.RuleHeader: "3"},
		Headers: []mail.Header{
			{"X-Hermes-Session", "abc"}, {"X-Note", "café ☕"}, {"X-Long", long}, {"X-Tag", "one"}, {"X-Tag", "two"},
		}}, time.Now())
	require.NoError(t, err)
	head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	folds := 0
	for _, l := range strings.Split(head, "\r\n") {
		assert.LessOrEqual(t, len(l), 78, "folded: %q", l)
		if strings.HasPrefix(l, " word") {
			folds++
		}
	}
	assert.Greater(t, folds, 5, "X-Long spans lines")
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, "abc", m.Header.Get("X-Hermes-Session"))
	assert.Equal(t, "3", m.Header.Get(mail.RuleHeader), "Godwit's own fields stay")
	note := m.Header.Get("X-Note")
	assert.True(t, strings.HasPrefix(note, "=?utf-8?"), "not ASCII: RFC 2047 encoded, %q", note)
	dec, err := new(mime.WordDecoder).DecodeHeader(note)
	require.NoError(t, err)
	assert.Equal(t, "café ☕", dec)
	assert.Equal(t, long, strings.Join(strings.Fields(m.Header.Get("X-Long")), " "), "unfolds to the value")
	assert.Equal(t, []string{"one", "two"}, m.Header["X-Tag"], "in order")
	// The draft's fields come after Godwit's, in their order.
	assert.Less(t, strings.Index(head, "\r\nDate: "), strings.Index(head, "\r\nX-Hermes-Session: "))
	assert.Less(t, strings.Index(head, "X-Hermes-Session: "), strings.Index(head, "X-Note: "))
	assert.Less(t, strings.Index(head, "X-Note: "), strings.Index(head, "X-Long: "))

	// Build checks again: a field that would inject another is never written.
	for _, h := range []mail.Header{{"X-A", "a\r\nBcc: eve@example.com"}, {"Bcc", "eve@example.com"}, {"Content-Type", "text/plain"}} {
		_, _, err := mail.Build(mail.Outgoing{From: "me@x.com", To: []string{"a@y.com"}, HTML: "x", Headers: []mail.Header{h}}, time.Now())
		var he *mail.HeaderError
		assert.True(t, errors.As(err, &he), "%q", h)
	}
}

// A read receipt goes to From, the only address Build will put there.
func TestBuildReadReceipt(t *testing.T) {
	o := mail.Outgoing{From: "Alice Liddell <alice@x.com>", To: []string{"a@y.com"}, HTML: "<p>hi</p>", ReadReceipt: true}
	raw, _, err := mail.Build(o, time.Now())
	require.NoError(t, err)
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	got, err := m.Header.AddressList("Disposition-Notification-To")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "alice@x.com", got[0].Address)
	o.ReadReceipt = false
	raw, _, err = mail.Build(o, time.Now())
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "Disposition-Notification-To")
}
