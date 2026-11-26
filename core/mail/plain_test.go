package mail_test

import (
	"encoding/json"
	"io"
	"mime/quotedprintable"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message"
	"github.com/godwit-email/godwit/core/mail"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlainTextRoundTrip(t *testing.T) {
	for _, text := range []string{
		"Hi Bob,\n\nSee below:\n    indented  code\twith tab\n> not a quote, typed\n\n\n\nThree blank lines above.\n\n-- \nAlice",
		"",
		"one line",
		"<b>not markup</b> & \"quotes\"",
		"a\n \nb", // a line of a space is text
	} {
		assert.Equal(t, text, mail.PlainText(mail.PlainHTML(text)), "%q", text)
	}
	assert.Equal(t, "<p>a<br>b &amp; c</p><p>d<br><br></p><p>e</p>", mail.PlainHTML("\r\na\r\nb & c\r\n\r\nd\n\n\ne\n\n"),
		"paragraphs, <br> breaks, one more <br> per extra blank line, the ends' blank lines dropped")
	assert.Equal(t, "end", mail.PlainText(mail.PlainHTML("\n\nend\n\n")))
}

// The cases web/src/compose/editor/plain.ts keeps too: the plain body as
// the compose page edits it.
func TestPlainSharedCases(t *testing.T) {
	data, err := os.ReadFile("testdata/plain.json")
	require.NoError(t, err)
	var cases []struct{ Text, HTML string }
	require.NoError(t, json.Unmarshal(data, &cases))
	require.NotEmpty(t, cases)
	for _, c := range cases {
		assert.Equal(t, c.HTML, mail.PlainHTML(c.Text), "%q", c.Text)
		assert.Equal(t, c.Text, mail.PlainText(c.HTML), "%q", c.HTML)
	}
}

func TestPlainTextFromHTML(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"editor paragraphs", `<p>Hello</p><p>Bye<br>now</p>`, "Hello\n\nBye\nnow"},
		{"an empty paragraph adds no line", `<p>Hello</p><p><br></p><p>Bye</p>`, "Hello\n\nBye"},
		{"trailing break", `<p>a<br><br></p><p>b</p>`, "a\n\n\nb"},
		{"lines as divs", `<div>a</div><div><br></div><div>b</div><div>c</div>`, "a\n\nb\nc"},
		{"lists", `<p>Items:</p><ul><li>x</li><li><p>y</p></li></ul><ol><li>one</li><li>two</li></ol><p>after</p>`,
			"Items:\n\n- x\n- y\n\n1. one\n2. two\n\nafter"},
		{"links", `<p><a href="https://e.com/a">site</a>, <a href="https://e.com/b">https://e.com/b</a>, <a href="mailto:a@b.c">a@b.c</a></p>`,
			"site (https://e.com/a), https://e.com/b, a@b.c"},
		{"source layout", "<div>\n  <p>\n    Hello\n    there\n  </p>\n</div>", "Hello there"},
		{"pre", "<pre>  a\n    b</pre>", "  a\n    b"},
		{"marks", `<p><strong>bold</strong> and <em>it</em></p>`, "bold and it"},
		{"nbsp", `<p>a&nbsp;&nbsp;b</p>`, "a  b"},
		{"table", `<table><tr><td>Total</td><td>12</td></tr><tr><td>Tax</td><td>1</td></tr></table>`, "Total 12\nTax 1"},
		{"quote", `<p>On Mon, Bob wrote:</p><blockquote>Hi<br>there<blockquote><p>older</p><p>x</p></blockquote></blockquote><p>mine</p>`,
			"On Mon, Bob wrote:\n> Hi\n> there\n>> older\n>>\n>> x\n\nmine"},
		{"quoted text quote", `<blockquote>a<br>&gt; older<br>&gt;&gt; oldest</blockquote>`, "> a\n>> older\n>>> oldest"},
		{"quote marker is no level", `<blockquote data-godwit="quote" style="margin:0"><p>lead</p><blockquote>q</blockquote></blockquote>`, "lead\n> q"},
	} {
		assert.Equal(t, c.want, mail.PlainText(c.in), c.name)
	}
}

func TestPlainBody(t *testing.T) {
	draft := mail.JoinDraft(mail.PlainHTML("Thanks, will do.\n\n"),
		`<blockquote data-godwit="quote" style="margin:0;padding:0;border:0"><p>On Tue, Bob wrote:</p><blockquote>Can you send it?<br><br>Bob</blockquote></blockquote>`,
		`<div data-godwit="signature"><p>-- <br>Alice Liddell<br>Godwit</p></div>`)
	assert.Equal(t, "Thanks, will do.\n\n-- \nAlice Liddell\nGodwit\n\nOn Tue, Bob wrote:\n> Can you send it?\n>\n> Bob", mail.PlainBody(draft))
}

func TestFlowed(t *testing.T) {
	long := strings.Repeat("word ", 30) + "end"
	got := mail.Flowed(long+"\nshort  \n> "+long+"\n>\n-- \nFrom here\n >odd\nverylong"+strings.Repeat("x", 100), 72)
	lines := strings.Split(strings.TrimSuffix(got, "\r\n"), "\r\n")
	// Soft breaks end in a space, every broken line fits.
	var i int
	for ; strings.HasSuffix(lines[i], " "); i++ {
		assert.LessOrEqual(t, len(lines[i]), 72, lines[i])
		assert.False(t, strings.HasPrefix(lines[i], ">"))
	}
	assert.True(t, strings.HasSuffix(lines[i], "end"), "hard break after the paragraph")
	i++
	assert.Equal(t, "short", lines[i], "trailing spaces dropped: they would make a soft break")
	i++
	for ; strings.HasSuffix(lines[i], " "); i++ {
		assert.True(t, strings.HasPrefix(lines[i], "> "), "each wrapped line keeps the quote: %q", lines[i])
		assert.LessOrEqual(t, len(lines[i]), 72)
	}
	assert.True(t, strings.HasPrefix(lines[i], "> ") && strings.HasSuffix(lines[i], "end"))
	assert.Equal(t, []string{">", "-- ", " From here", "  >odd", "verylong" + strings.Repeat("x", 100)}, lines[i+1:],
		"empty quoted line has no space, the signature separator stays, From and > are stuffed, a long word stays whole")

	// Unwrapping per RFC 3676 (joining soft-broken lines) gives the paragraph back.
	assert.Equal(t, long, strings.Join(lines[:countSoft(lines)+1], ""))
}

// countSoft is the index of the first line not ending in a soft break.
func countSoft(lines []string) int {
	for i, l := range lines {
		if !strings.HasSuffix(l, " ") {
			return i
		}
	}
	return len(lines)
}

func TestBuildPlain(t *testing.T) {
	draft := mail.JoinDraft(mail.PlainHTML("Grüße,\n"+strings.Repeat("lorem ipsum ", 12)),
		`<blockquote data-godwit="quote"><p>On Tue, Bob wrote:</p><blockquote>Can you send it?</blockquote></blockquote>`,
		`<div data-godwit="signature"><p>-- <br>Alice</p><p><img src="cid:logo@godwit"></p></div>`)
	o := mail.Outgoing{From: "me@x.com", To: []string{"a@y.com"}, Subject: "Re: plan", HTML: draft, Plain: true,
		Related: []mail.Attachment{{ContentID: "logo@godwit", ContentType: "image/png", Data: []byte("png")}}}
	raw, _, err := mail.Build(o, time.Now())
	require.NoError(t, err)
	e, err := message.Read(strings.NewReader(string(raw)))
	require.NoError(t, err)
	assert.Equal(t, "text/plain; charset=utf-8; format=flowed", e.Header.Get("Content-Type"))
	assert.Equal(t, "quoted-printable", e.Header.Get("Content-Transfer-Encoding"), "non-ASCII and soft breaks")
	assert.Equal(t, "1.0", e.Header.Get("MIME-Version"))
	assert.NotContains(t, string(raw), "image/png", "no HTML shows the signature image")
	body, err := io.ReadAll(e.Body)
	require.NoError(t, err)
	text := string(body)
	assert.True(t, strings.HasPrefix(text, "Grüße,\r\nlorem ipsum "), text)
	assert.Contains(t, text, "\r\n\r\n-- \r\nAlice\r\n\r\nOn Tue, Bob wrote:\r\n> Can you send it?\r\n")
	for l := range strings.SplitSeq(text, "\r\n") {
		assert.LessOrEqual(t, len([]rune(l)), 72)
	}

	p, err := mail.Parse(raw)
	require.NoError(t, err)
	assert.Empty(t, p.HTML)
	assert.Contains(t, p.Text, "Grüße,")

	// ASCII without soft breaks goes 7bit; attachments put it first in multipart/mixed.
	o = mail.Outgoing{From: "me@x.com", To: []string{"a@y.com"}, HTML: mail.PlainHTML("hi"), Plain: true,
		Attachments: []mail.Attachment{{Filename: "a.txt", ContentType: "text/plain", Data: []byte("x")}}}
	raw, _, err = mail.Build(o, time.Now())
	require.NoError(t, err)
	e, err = message.Read(strings.NewReader(string(raw)))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(e.Header.Get("Content-Type"), "multipart/mixed"))
	mr := e.MultipartReader()
	first, err := mr.NextPart()
	require.NoError(t, err)
	assert.Equal(t, "text/plain; charset=utf-8; format=flowed", first.Header.Get("Content-Type"))
	assert.Equal(t, "7bit", first.Header.Get("Content-Transfer-Encoding"))
	b, _ := io.ReadAll(first.Body)
	assert.Equal(t, "hi\r\n", string(b))
	second, err := mr.NextPart()
	require.NoError(t, err)
	assert.Contains(t, second.Header.Get("Content-Disposition"), "attachment")
}

// A soft break's trailing space survives the transfer encoding.
func TestBuildPlainKeepsSoftBreaks(t *testing.T) {
	raw, _, err := mail.Build(mail.Outgoing{From: "me@x.com", To: []string{"a@y.com"}, Plain: true,
		HTML: mail.PlainHTML(strings.Repeat("abc ", 40))}, time.Now())
	require.NoError(t, err)
	i := strings.Index(string(raw), "\r\n\r\n")
	dec, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(raw[i+4:]))))
	require.NoError(t, err)
	first, _, _ := strings.Cut(string(dec), "\r\n")
	assert.True(t, strings.HasSuffix(first, " "), "%q", first)
}
