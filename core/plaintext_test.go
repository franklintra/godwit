package core

import (
	"strings"
	"testing"

	"github.com/godwit-email/godwit/core/mail"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A new draft takes the account's format, else the settings', else HTML;
// unknown formats are refused everywhere.
func TestDraftFormatDefaults(t *testing.T) {
	e := newSendEnv(t, false)
	format := func() string {
		t.Helper()
		d, err := e.s.NewDraft(bg, e.acc.ID, "new", 0)
		require.NoError(t, err)
		saved, err := e.s.SaveDraft(bg, Draft{AccountID: e.acc.ID, Mode: "new", HTML: "<p>x</p>"})
		require.NoError(t, err)
		assert.Equal(t, d.Format, saved.Format, "SaveDraft of a new draft without a format takes the same default")
		return d.Format
	}
	st, err := e.s.GetSettings(bg)
	require.NoError(t, err)
	assert.Equal(t, "", st.ComposeFormat)
	assert.Equal(t, FormatHTML, format())

	st.ComposeFormat = FormatPlain
	st, err = e.s.SetSettings(bg, st)
	require.NoError(t, err)
	assert.Equal(t, FormatPlain, format())

	html := FormatHTML
	acc, err := e.s.UpdateAccount(bg, e.acc.ID, AccountPatch{ComposeFormat: &html})
	require.NoError(t, err)
	assert.Equal(t, FormatHTML, acc.ComposeFormat)
	assert.Equal(t, FormatHTML, format(), "the account's own default comes first")

	rtf := "rtf"
	_, err = e.s.UpdateAccount(bg, e.acc.ID, AccountPatch{ComposeFormat: &rtf})
	require.Error(t, err)
	st.ComposeFormat = rtf
	_, err = e.s.SetSettings(bg, st)
	require.Error(t, err)
	_, err = e.s.SaveDraft(bg, Draft{AccountID: e.acc.ID, Mode: "new", Format: rtf})
	require.Error(t, err)
}

// A plain draft keeps its editable part as text whoever writes it; saving it
// without a format keeps it plain, and as HTML keeps its paragraphs.
func TestPlainDraftSave(t *testing.T) {
	e := newSendEnv(t, false)
	plain := FormatPlain
	_, err := e.s.UpdateAccount(bg, e.acc.ID, AccountPatch{ComposeFormat: &plain})
	require.NoError(t, err)
	d, err := e.s.NewDraft(bg, e.acc.ID, "new", 0)
	require.NoError(t, err)
	require.Equal(t, FormatPlain, d.Format)
	_, _, sig := mail.SplitDraft(d.HTML)
	assert.Contains(t, sig, "Alice Liddell", "the signature block stays, sent as text")

	d.HTML = mail.JoinDraft(`<p><b>Hi</b> <a href="https://e.com">there</a></p><ul><li>one</li></ul>`, "", sig)
	d, err = e.s.SaveDraft(bg, Draft{ID: d.ID, To: []string{"bob@example.com"}, HTML: d.HTML})
	require.NoError(t, err)
	assert.Equal(t, FormatPlain, d.Format, "an update without a format keeps it")
	ed, _, _ := mail.SplitDraft(d.HTML)
	assert.Equal(t, mail.PlainHTML("Hi there (https://e.com)\n\n- one"), ed)

	d.Format = FormatHTML
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	assert.Equal(t, FormatHTML, d.Format)
	ed, _, _ = mail.SplitDraft(d.HTML)
	assert.Equal(t, mail.PlainHTML("Hi there (https://e.com)\n\n- one"), ed, "the text's paragraphs, now HTML")
}

// With ReplyInKind, a reply takes the format of the mail it answers; a plain
// reply quotes the mail's own text. A forward keeps the default.
func TestReplyInKind(t *testing.T) {
	e := newSendEnv(t, false)
	e.put(t, "INBOX", eml("t@x", "Bob <bob@example.com>", "alice@example.com", "plan", day, "the  plan\r\n\r\n> older"))
	e.put(t, "INBOX", htmlOnly)
	e.sync(t)
	reply := func(mode, id string) Draft {
		t.Helper()
		d, err := e.s.NewDraft(bg, e.acc.ID, mode, e.idOf(t, id))
		require.NoError(t, err)
		return d
	}
	assert.Equal(t, FormatHTML, reply("reply", "t@x").Format, "off by default")

	st, err := e.s.GetSettings(bg)
	require.NoError(t, err)
	st.ReplyInKind = true
	_, err = e.s.SetSettings(bg, st)
	require.NoError(t, err)
	d := reply("reply", "t@x")
	assert.Equal(t, FormatPlain, d.Format)
	_, quote, _ := mail.SplitDraft(d.HTML)
	assert.Equal(t, "> the  plan\n>\n>> older", strings.SplitN(mail.PlainText(quote), "\n", 2)[1])
	assert.Equal(t, FormatHTML, reply("reply_all", "h@x").Format)
	assert.Equal(t, FormatHTML, reply("forward", "t@x").Format)
}

// A plain draft goes out as one text/plain, format=flowed part: new text,
// signature after "-- ", then the quote as "> " lines.
func TestSendPlainDraft(t *testing.T) {
	e := newSendEnv(t, false)
	st, err := e.s.GetSettings(bg)
	require.NoError(t, err)
	st.ComposeFormat, st.SignatureDelimiter = FormatPlain, true
	_, err = e.s.SetSettings(bg, st)
	require.NoError(t, err)
	e.put(t, "INBOX", eml("o@x", "Bob <bob@example.com>", "alice@example.com", "plan", day, "Can you send it?"))
	e.sync(t)
	d, err := e.s.NewDraft(bg, e.acc.ID, "reply", e.idOf(t, "o@x"))
	require.NoError(t, err)
	_, quote, sig := mail.SplitDraft(d.HTML)
	d.HTML = mail.JoinDraft(mail.PlainHTML("Thanks, will do.\n"+strings.Repeat("More words here. ", 8)), quote, sig)
	d, err = e.s.SaveDraft(bg, d)
	require.NoError(t, err)
	require.NoError(t, e.s.Send(bg, d.ID))
	e.clock.Advance(UndoSendWindow)
	e.sync(t)
	got := e.smtp.Messages()
	require.Len(t, got, 1)
	raw := string(got[0].Data)
	assert.Contains(t, raw, "Content-Type: text/plain; charset=utf-8; format=flowed")
	assert.NotContains(t, raw, "text/html")
	p, err := mail.Parse(got[0].Data)
	require.NoError(t, err)
	assert.Empty(t, p.HTML)
	text := strings.ReplaceAll(p.Text, "\r\n", "\n")
	assert.Contains(t, text, "Thanks, will do.\nMore words here. ")
	assert.Contains(t, text, "\n-- \nAlice Liddell\nGodwit\n\n")
	assert.Regexp(t, `(?m)^> Can you send it\?$`, text)
	assert.Equal(t, "o@x", p.InReplyTo)
}
