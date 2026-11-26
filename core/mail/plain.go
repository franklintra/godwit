package mail

import (
	"html"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// FlowedWidth is the longest line, in characters, Flowed writes when it can
// break: RFC 3676 asks for at most 78 and recommends fewer.
const FlowedWidth = 72

// plainBlocks are the elements that start and end a line of PlainText.
var plainBlocks = map[atom.Atom]bool{atom.P: true, atom.Div: true, atom.H1: true, atom.H2: true, atom.H3: true,
	atom.H4: true, atom.H5: true, atom.H6: true, atom.Li: true, atom.Pre: true, atom.Blockquote: true,
	atom.Ul: true, atom.Ol: true, atom.Table: true, atom.Tr: true, atom.Hr: true, atom.Dl: true, atom.Dt: true,
	atom.Dd: true, atom.Center: true, atom.Section: true, atom.Article: true, atom.Header: true,
	atom.Footer: true, atom.Address: true, atom.Figure: true}

// plainItems are the blocks that are lines of a list or a table: never a
// blank line between two of them. A <div> is a line too (as mail clients
// write one per line) but keeps a blank line that comes before it; every
// other block is a paragraph, a blank line apart from what follows it.
var plainItems = map[atom.Atom]bool{atom.Li: true, atom.Tr: true, atom.Dt: true, atom.Dd: true}

// plainContainers hold blocks: white space right inside one is layout, not text.
var plainContainers = map[atom.Atom]bool{atom.Body: true, atom.Blockquote: true, atom.Ul: true, atom.Ol: true,
	atom.Table: true, atom.Tbody: true, atom.Thead: true, atom.Tfoot: true, atom.Tr: true, atom.Div: true, atom.Dl: true}

// brk stands for a run of white space holding a line break, the source's own
// layout: one space inside a line, nothing at either end.
const brk = rune(0xE000) // a private-use character, in no mail

type plainLine struct {
	depth int
	text  string
}

// plainWriter collects PlainText's lines.
type plainWriter struct {
	lines  []plainLine
	cur    strings.Builder
	open   bool // a line is open
	byBr   bool // the open line was opened by a <br> and holds nothing yet
	sep    bool // a paragraph ended: a blank line before the next line
	depth  int  // the open line's quote depth
	prefix string
}

// start opens a line at depth, after the blank line a paragraph left.
func (w *plainWriter) start(depth int) {
	if w.open {
		return
	}
	if w.sep && len(w.lines) > 0 {
		w.lines = append(w.lines, plainLine{depth: depth})
	}
	w.open, w.sep, w.depth = true, false, depth
	w.cur.Reset()
}

// write adds s to the line, opening one at depth (after a list item's prefix).
func (w *plainWriter) write(s string, depth int) {
	if s == "" {
		return
	}
	w.start(depth)
	if w.prefix != "" {
		w.cur.WriteString(w.prefix)
		w.prefix = ""
	}
	w.byBr = false
	w.cur.WriteString(s)
}

// emit ends the open line, keeping it even if empty.
func (w *plainWriter) emit() {
	s := strings.Trim(w.cur.String(), string(brk))
	w.lines = append(w.lines, plainLine{w.depth, strings.ReplaceAll(s, string(brk), " ")})
	w.open, w.byBr = false, false
	w.cur.Reset()
}

// end ends the open line at a block's edge. A line a <br> opened and left
// empty is dropped: that <br> only held the block's last line open.
func (w *plainWriter) end() {
	switch {
	case !w.open:
	case w.byBr && w.cur.Len() == 0:
		w.open, w.byBr = false, false
	default:
		w.emit()
	}
}

// br breaks the line: an empty one if none is open.
func (w *plainWriter) br(depth int) {
	w.start(depth)
	w.emit()
	w.start(depth)
	w.byBr = true
}

// collapseBreaks turns each run of white space holding a line break into brk.
func collapseBreaks(s string) string {
	if !strings.ContainsAny(s, "\n\r") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && strings.IndexByte(" \t\n\r\f", s[j]) >= 0 {
			j++
		}
		if j == i {
			b.WriteByte(s[i])
			i++
			continue
		}
		if run := s[i:j]; strings.ContainsAny(run, "\n\r") {
			b.WriteRune(brk)
		} else {
			b.WriteString(run)
		}
		i = j
	}
	return b.String()
}

func (w *plainWriter) walk(n *xhtml.Node, depth int, pre bool) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case xhtml.TextNode:
			s := strings.ReplaceAll(c.Data, "\u00a0", " ") // no-break spaces read as spaces
			if pre {
				s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
				for i, l := range strings.Split(s, "\n") {
					if i > 0 {
						w.br(depth)
					}
					w.write(l, depth)
				}
				continue
			}
			s = collapseBreaks(s)
			if !w.open && strings.Trim(s, " \t\f"+string(brk)) == "" &&
				(strings.ContainsRune(s, brk) || plainContainers[n.DataAtom]) {
				continue // layout between blocks
			}
			w.write(s, depth)
		case xhtml.ElementNode:
			w.element(c, depth, pre)
		}
	}
}

func (w *plainWriter) element(n *xhtml.Node, depth int, pre bool) {
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Head, atom.Template:
		return
	case atom.Br:
		w.br(depth)
		return
	case atom.Td, atom.Th: // a row's cells stay apart
		if w.open && w.cur.Len() > 0 && !strings.HasSuffix(w.cur.String(), " ") {
			w.cur.WriteByte(' ')
		}
	case atom.A:
		href, _ := attr(n, "href")
		if href == "" {
			href, _ = attr(n, "data-href")
		}
		before := w.cur.String()
		w.walk(n, depth, pre)
		text, _ := strings.CutPrefix(w.cur.String(), before)
		if text = strings.TrimSpace(text); href != "" && openable(href) && text != href && "mailto:"+text != href {
			w.write(" ("+href+")", depth)
		}
		return
	}
	if !plainBlocks[n.DataAtom] {
		w.walk(n, depth, pre)
		return
	}
	w.end()
	if plainItems[n.DataAtom] && prevElement(n) != nil && plainItems[prevElement(n).DataAtom] {
		w.sep = false // the next item of a list or row of a table
	}
	from, sep := len(w.lines), w.sep
	switch n.DataAtom {
	case atom.Blockquote:
		w.sep = false                              // a quote follows its attribution line
		if _, marked := attr(n, marker); !marked { // the draft's quote marker only marks it
			depth++
		}
	case atom.Ol:
		i := 0
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == xhtml.ElementNode && c.DataAtom == atom.Li {
				i++
				w.prefix = strconv.Itoa(i) + ". "
				w.element(c, depth, pre)
			}
		}
		w.end()
		w.sep = true
		return
	case atom.Li:
		if w.prefix == "" {
			w.prefix = "- "
		}
	}
	w.walk(n, depth, pre || n.DataAtom == atom.Pre)
	w.end()
	w.prefix = ""
	if added := w.lines[from:]; n.DataAtom == atom.P && (len(added) == 1 || len(added) == 2 && sep) &&
		!slices.ContainsFunc(added, func(l plainLine) bool { return l.text != "" }) {
		w.lines, w.sep = w.lines[:from], sep // an empty paragraph only parts the others, as the editor shows it
	}
	if !plainItems[n.DataAtom] && n.DataAtom != atom.Div {
		w.sep = true
	}
}

// prevElement is the element before n among its siblings, nil if none.
func prevElement(n *xhtml.Node) *xhtml.Node {
	for p := n.PrevSibling; p != nil; p = p.PrevSibling {
		if p.Type == xhtml.ElementNode {
			return p
		}
	}
	return nil
}

// PlainText is src, compose HTML, as plain text. Paragraphs are a blank line
// apart, an empty one adding none; a <br> breaks the line; a list item, a
// table row and a <div> are a line each, a list item after "- " or its
// number; a link's address follows its text; each line of a blockquote has
// one more ">" (">> text" two deep), and no blank line parts a quote from
// the attribution line before it. Spaces stay as written, except that a run
// of white space holding a line break (the source's layout) reads as one
// space; inside <pre> everything stays. PlainText(PlainHTML(t)) is t without
// the blank lines at its ends.
func PlainText(src string) string {
	w := &plainWriter{}
	w.walk(parseBody(src), 0, false)
	w.end()
	var b strings.Builder
	for i, l := range w.lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if l.depth > 0 {
			b.WriteString(strings.Repeat(">", l.depth))
			if l.text != "" && l.text[0] != '>' { // a quoted quote line goes one deeper: ">> text"
				b.WriteByte(' ')
			}
		}
		b.WriteString(l.text)
	}
	return b.String()
}

// PlainHTML is text as the compose HTML of a plain-text draft, as the
// editor writes paragraphs: lines a blank line apart are paragraphs, a line
// break inside one is a <br>, and each more blank line between two is one
// more <br> at the end of the first. Blank lines at either end go.
func PlainHTML(text string) string {
	lines := strings.Split(trimBlank(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")), "\n")
	var b strings.Builder
	for i := 0; i < len(lines); {
		if lines[i] == "" && i == 0 {
			break // no text
		}
		b.WriteString("<p>")
		for first := true; i < len(lines) && lines[i] != ""; i, first = i+1, false {
			if !first {
				b.WriteString("<br>")
			}
			b.WriteString(html.EscapeString(lines[i]))
		}
		blank := 0
		for ; i < len(lines) && lines[i] == ""; i++ {
			blank++
		}
		if blank > 1 {
			b.WriteString(strings.Repeat("<br>", blank))
		}
		b.WriteString("</p>")
	}
	return b.String()
}

// trimBlank drops the blank lines at either end of s.
func trimBlank(s string) string {
	lines := strings.Split(s, "\n")
	blank := func(l string) bool { return strings.TrimSpace(l) == "" }
	for len(lines) > 0 && blank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && blank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// PlainBody is a draft body (sanitized compose HTML, SplitDraft's parts) as
// the text of a plain-text message: the new text, the signature, then the
// quote with its attribution line, in JoinDraft's order, a blank line apart.
func PlainBody(src string) string {
	ed, quote, sig := SplitDraft(src)
	var parts []string
	for _, p := range []string{ed, sig, quote} {
		if t := trimBlank(PlainText(p)); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Flowed encodes text, lines quoted with ">" as PlainText writes them, as a
// format=flowed body (RFC 3676) with CRLF line ends: a line longer than width
// breaks after a space, the space left at the end of the line (a soft break);
// trailing spaces of a line are dropped (they would read as one); a line that
// starts with a space, ">" or "From " is space-stuffed; a quoted line has its
// ">"s then a space. The signature separator "-- " stays as it is.
func Flowed(text string, width int) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var b strings.Builder
	for line := range strings.SplitSeq(text, "\n") {
		if line == "-- " {
			b.WriteString("-- \r\n")
			continue
		}
		depth := 0
		for depth < len(line) && line[depth] == '>' {
			depth++
		}
		rest := line[depth:]
		if depth > 0 {
			rest = strings.TrimPrefix(rest, " ") // the space after the quote marks
		}
		rest = strings.TrimRight(rest, " ")
		quote := strings.Repeat(">", depth)
		for _, chunk := range wrapFlowed(rest, width-depth-1) {
			b.WriteString(quote)
			if (depth > 0 && chunk != "") || (depth == 0 && needsStuffing(chunk)) {
				b.WriteByte(' ')
			}
			b.WriteString(chunk)
			b.WriteString("\r\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\r\n") + "\r\n"
}

func needsStuffing(s string) bool {
	return strings.HasPrefix(s, " ") || strings.HasPrefix(s, ">") || strings.HasPrefix(s, "From ")
}

// wrapFlowed splits s into chunks of at most width characters where it can,
// each break after a space that stays at the chunk's end; a word longer than
// width stays whole. An empty s is one empty chunk.
func wrapFlowed(s string, width int) []string {
	width = max(width, 20)
	var out []string
	for utf8.RuneCountInString(s) > width {
		cut := -1 // the byte offset after the last space within width, else after the first space
		n := 0
		for i, r := range s {
			if n >= width && cut > 0 {
				break
			}
			if r == ' ' && i > 0 {
				cut = i + 1
				if n >= width {
					break
				}
			}
			n++
		}
		if cut <= 0 || cut >= len(s) {
			break
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return append(out, s)
}
