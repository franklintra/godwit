//go:build linux

package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"mime"
	"strings"
	"time"
)

// The synthetic mail: deterministic for a seed, shaped like a real mailbox
// with years of history. Three kinds of message carry most of the bytes:
// newsletters (large HTML), notifications (medium HTML) and personal threads
// (short text, quoted replies). The mix and sizes are calibrated on
// Franklin's database: ~44,700 messages in ~34,000 conversations, 2.8 GB,
// of which the messages table (bodies inline) is 1.8 GB.

// genMsg is one message to put in a folder.
type genMsg struct {
	Folder string
	Raw    []byte
	Date   time.Time
	Seen   bool
	Flag   bool
	Kind   string // newsletter, notification, personal, attachment
}

// accountPlan sizes one account.
type accountPlan struct {
	Email    string
	Messages int
	Extra    []string // custom folders
}

// accountPlans is the fixture's eight accounts, 45,000 messages in all,
// skewed as Franklin's are: one big Gmail-like mailbox with ten years of
// history, then a tail.
func accountPlans(scale float64) []accountPlan {
	sizes := []int{15000, 9000, 6500, 5000, 4000, 3000, 1800, 700}
	extras := [][]string{
		{"Projects/Alpha", "Projects/Beta", "Receipts", "Travel", "Newsletters", "Family", "Admin", "Old/2019"},
		{"Clients/Acme", "Clients/Globex", "Invoices", "Newsletters", "Team"},
		{"Receipts", "Travel", "Newsletters", "School"},
		{"Lists/golang", "Lists/sqlite", "Newsletters"},
		{"Receipts", "Archive/2020", "Archive/2021"},
		{"Bank", "Newsletters"},
		{"Notifications"},
		{},
	}
	var out []accountPlan
	for i, n := range sizes {
		m := int(float64(n) * scale)
		if m < 20 {
			m = 20
		}
		out = append(out, accountPlan{Email: fmt.Sprintf("bench%d@perfbench.invalid", i+1), Messages: m, Extra: extras[i]})
	}
	return out
}

var (
	firstNames = []string{"Alice", "Bruno", "Chloé", "David", "Emma", "Farid", "Giulia", "Hugo", "Inès", "Jonas", "Kenji", "Léa", "Marc", "Nora", "Olivier", "Paula", "Quentin", "Rosa", "Samir", "Tess", "Ugo", "Valérie", "Wen", "Yanis", "Zoé"}
	lastNames  = []string{"Martin", "Bernard", "Dubois", "Thomas", "Robert", "Richard", "Petit", "Durand", "Leroy", "Moreau", "Simon", "Laurent", "Lefebvre", "Michel", "Garcia", "Müller", "Rossi", "Tanaka", "Nguyen", "Smith"}
	domains    = []string{"example.org", "example.net", "mail.example.com", "corp.example", "uni.example.edu", "studio.example.fr"}
	senders    = []string{"newsletter@news.example.com", "digest@weekly.example.org", "offers@shop.example.net", "noreply@github.example.com", "alerts@bank.example.fr", "updates@travel.example.com", "info@events.example.org", "hello@saas.example.io"}
	notifiers  = []string{"noreply@ci.example.com", "notifications@tracker.example.org", "no-reply@calendar.example.com", "security@accounts.example.com", "orders@shop.example.net", "receipts@pay.example.com"}
	topics     = []string{"Quarterly roadmap", "Release checklist", "Invoice", "Design review", "Weekend plans", "Flight to Lisbon", "Apartment visit", "Budget 2026", "Team offsite", "Thesis draft", "Server migration", "Dinner on Friday", "Contract renewal", "Insurance claim", "Birthday party", "Course registration", "Kubernetes upgrade", "Photos from the trip", "Tax documents", "Moving boxes"}
	words      = buildVocab()
)

// buildVocab is a fixed vocabulary of English and French words plus
// pronounceable made-up ones, so trigram search sees a realistic spread.
func buildVocab() []string {
	base := strings.Fields(`the of and to in is that for it as with was on be at by this had not are but from or have an they which one you were all we when there can more if no out so said what up its about into than them only other new some could time these two may first then do any like my now over such our man me even most made after also did many before must through back years where much your way well down should because each just those people how too little state good very make world still own see men work long get here between both life being under never day same another know while last might us great old year off come since against go came right used take three
le la les de des du un une et en est que qui dans pour pas sur au avec plus par se ne ce il elle nous vous ils sont été être avoir fait faire dire comme mais ou donc car tout tous très bien aussi encore toujours jamais rien chose temps jour année monde vie homme femme enfant maison ville pays travail projet réunion demain hier semaine mois prochain dernier merci bonjour salut cordialement message courriel facture commande livraison rendez-vous voyage billet hôtel réservation`)
	rng := rand.New(rand.NewPCG(1, 2))
	syl := []string{"ka", "lo", "mi", "ten", "dra", "vel", "sor", "pli", "que", "zan", "ber", "tis", "mon", "gra", "fel", "rin", "das", "cho", "ple", "vua"}
	for range 2500 {
		n := 2 + rng.IntN(3)
		var b strings.Builder
		for range n {
			b.WriteString(syl[rng.IntN(len(syl))])
		}
		base = append(base, b.String())
	}
	return base
}

func pick[T any](rng *rand.Rand, xs []T) T { return xs[rng.IntN(len(xs))] }

// sentence is a few words with a capital and a full stop.
func sentence(rng *rand.Rand) string {
	n := 6 + rng.IntN(14)
	var b strings.Builder
	for i := range n {
		w := pick(rng, words)
		if i == 0 {
			w = strings.ToUpper(w[:1]) + w[1:]
		} else {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	b.WriteByte('.')
	return b.String()
}

// paragraphs fills about size bytes of plain text.
func paragraphs(rng *rand.Rand, size int) []string {
	var out []string
	total := 0
	for total < size {
		var b strings.Builder
		for range 2 + rng.IntN(5) {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(sentence(rng))
		}
		out = append(out, b.String())
		total += b.Len() + 2
	}
	return out
}

func person(rng *rand.Rand) (name, addr string) {
	f, l := pick(rng, firstNames), pick(rng, lastNames)
	return f + " " + l, strings.ToLower(asciiOnly(f)) + "." + strings.ToLower(asciiOnly(l)) + "@" + pick(rng, domains)
}

func asciiOnly(s string) string {
	r := strings.NewReplacer("é", "e", "è", "e", "ü", "u", "ï", "i", "ô", "o", "É", "E")
	return r.Replace(s)
}

// htmlNewsletter is a table layout with inline styles, as marketing mail is.
func htmlNewsletter(rng *rand.Rand, size int, title string) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>` + title + `</title><style>body{margin:0;padding:0;background:#f4f4f4}table{border-collapse:collapse}.btn{background:#0a66c2;color:#fff;padding:12px 24px;border-radius:4px;text-decoration:none}</style></head><body>`)
	b.WriteString(`<table width="100%" cellpadding="0" cellspacing="0" style="background:#f4f4f4"><tr><td align="center"><table width="600" style="background:#ffffff;font-family:Helvetica,Arial,sans-serif;color:#222">`)
	b.WriteString(`<tr><td style="padding:24px"><img src="https://img.example.com/logo.png" width="120" height="40" alt="Logo"><h1 style="font-size:24px;margin:16px 0">` + title + `</h1></td></tr>`)
	for b.Len() < size {
		p := paragraphs(rng, 300+rng.IntN(600))
		b.WriteString(`<tr><td style="padding:12px 24px;font-size:15px;line-height:22px">`)
		b.WriteString(`<h2 style="font-size:18px;margin:0 0 8px 0;color:#0a66c2">` + sentence(rng) + `</h2>`)
		for _, s := range p {
			b.WriteString(`<p style="margin:0 0 12px 0">` + s + `</p>`)
		}
		fmt.Fprintf(&b, `<img src="https://img.example.com/c/%d.jpg" width="552" style="display:block;width:100%%;max-width:552px" alt="">`, rng.IntN(100000))
		fmt.Fprintf(&b, `<p><a class="btn" href="https://click.example.com/t/%x?u=%x&amp;utm_source=newsletter&amp;utm_medium=email">Read more</a></p></td></tr>`, rng.Uint64(), rng.Uint64())
	}
	b.WriteString(`<tr><td style="padding:24px;font-size:11px;color:#888">You receive this because you subscribed. <a href="https://news.example.com/unsubscribe">Unsubscribe</a></td></tr></table></td></tr></table></body></html>`)
	return b.String()
}

func htmlFromText(paras []string, quoted string) string {
	var b strings.Builder
	b.WriteString(`<html><head><meta http-equiv="Content-Type" content="text/html; charset=utf-8"></head><body><div dir="ltr">`)
	for _, p := range paras {
		b.WriteString("<div>" + p + "</div><div><br></div>")
	}
	b.WriteString("</div>")
	if quoted != "" {
		b.WriteString(`<div class="gmail_quote"><blockquote class="gmail_quote" style="margin:0 0 0 .8ex;border-left:1px #ccc solid;padding-left:1ex">`)
		b.WriteString(strings.ReplaceAll(quoted, "\n", "<br>"))
		b.WriteString("</blockquote></div>")
	}
	b.WriteString("</body></html>")
	return b.String()
}

// qp is a quoted-printable body, lines folded at 76 as mailers send them.
func qp(s string) string {
	var b bytes.Buffer
	line := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var tok string
		switch {
		case c == '\n':
			b.WriteString("\r\n")
			line = 0
			continue
		case c == '=' || c >= 0x7f || (c < 0x20 && c != '\t'):
			tok = fmt.Sprintf("=%02X", c)
		default:
			tok = string(c)
		}
		if line+len(tok) > 75 {
			b.WriteString("=\r\n")
			line = 0
		}
		b.WriteString(tok)
		line += len(tok)
	}
	return b.String()
}

func b64lines(data []byte) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
	return b.String()
}

type header struct{ k, v string }

// encodeHeader puts non-ASCII header text in RFC 2047 words: the whole
// Subject, or an address's display name.
func encodeHeader(k, v string) string {
	ascii := true
	for i := 0; i < len(v); i++ {
		if v[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return v
	}
	if name, addr, ok := strings.Cut(v, " <"); ok && k != "Subject" {
		return mime.QEncoding.Encode("utf-8", strings.Trim(name, `"`)) + " <" + addr
	}
	return mime.QEncoding.Encode("utf-8", v)
}

// buildRaw assembles an RFC 5322 message: multipart/alternative of text
// and HTML, wrapped in multipart/mixed when it has an attachment.
func buildRaw(rng *rand.Rand, hdrs []header, text, html string, attach []byte, attachName string) []byte {
	var b bytes.Buffer
	for _, h := range hdrs {
		b.WriteString(h.k + ": " + encodeHeader(h.k, h.v) + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	alt := fmt.Sprintf("alt-%016x", rng.Uint64())
	writeAlt := func() {
		fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", alt)
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n%s\r\n", alt, qp(text))
		if html != "" {
			fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n%s\r\n", alt, qp(html))
		}
		fmt.Fprintf(&b, "--%s--\r\n", alt)
	}
	if attach == nil {
		writeAlt()
		return b.Bytes()
	}
	mixed := fmt.Sprintf("mix-%016x", rng.Uint64())
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n--%s\r\n", mixed, mixed)
	writeAlt()
	ct := "application/pdf"
	switch {
	case strings.HasSuffix(attachName, ".jpg"):
		ct = "image/jpeg"
	case strings.HasSuffix(attachName, ".ics"):
		ct = "text/calendar"
	case strings.HasSuffix(attachName, ".xlsx"):
		ct = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	}
	fmt.Fprintf(&b, "\r\n--%s\r\nContent-Type: %s; name=\"%s\"\r\nContent-Disposition: attachment; filename=\"%s\"\r\nContent-Transfer-Encoding: base64\r\n\r\n%s--%s--\r\n",
		mixed, ct, attachName, attachName, b64lines(attach), mixed)
	return b.Bytes()
}

// genAccount makes plan's messages, oldest first, spread over ten years
// with recent years denser. Personal threads alternate between INBOX and
// Sent; newsletters land in INBOX or a Newsletters folder.
func genAccount(seed uint64, idx int, plan accountPlan, now time.Time, emit func(genMsg)) {
	rng := rand.New(rand.NewPCG(seed, uint64(idx)+1))
	self := plan.Email
	selfName := fmt.Sprintf("Bench User %d", idx+1)
	hasNews := false
	for _, f := range plan.Extra {
		if f == "Newsletters" {
			hasNews = true
		}
	}
	span := 10 * 365 * 24 * time.Hour
	// date of the i-th of n: quadratic, so the last years hold most mail
	dateAt := func(i, n int) time.Time {
		x := float64(i) / float64(n)
		x = 1 - (1-x)*(1-x)
		return now.Add(-span + time.Duration(x*float64(span))).Add(-time.Duration(rng.IntN(3600)) * time.Second)
	}
	n := plan.Messages
	i := 0
	mid := func() string { return fmt.Sprintf("<%016x.%d@perfbench.invalid>", rng.Uint64(), idx) }
	folderFor := func(kind string, recent bool) string {
		r := rng.Float64()
		switch {
		case r < 0.03:
			return "Trash"
		case r < 0.05 && kind != "personal":
			return "Junk"
		}
		if kind == "newsletter" && hasNews && rng.Float64() < 0.6 {
			return "Newsletters"
		}
		if !recent && rng.Float64() < 0.55 {
			if len(plan.Extra) > 0 && rng.Float64() < 0.35 {
				return pick(rng, plan.Extra)
			}
			return "Archive"
		}
		return "INBOX"
	}
	seenFor := func(folder string, recent bool, kind string) bool {
		if folder == "Sent" || folder == "Trash" || folder == "Archive" {
			return true
		}
		if kind == "newsletter" {
			return rng.Float64() < 0.6
		}
		if recent {
			return rng.Float64() < 0.85
		}
		return rng.Float64() < 0.99
	}
	for i < n {
		recent := i > n*9/10
		r := rng.Float64()
		switch {
		case r < 0.40: // newsletter
			ts := dateAt(i, n)
			from := pick(rng, senders)
			title := pick(rng, topics) + " — " + sentence(rng)
			text := strings.Join(paragraphs(rng, 5000+rng.IntN(8000)), "\n\n")
			html := htmlNewsletter(rng, 30000+rng.IntN(80000), title)
			hdrs := []header{{"From", "\"Weekly\" <" + from + ">"}, {"To", self}, {"Subject", title}, {"Date", ts.Format(time.RFC1123Z)}, {"Message-ID", mid()},
				{"List-Unsubscribe", "<https://news.example.com/u/" + fmt.Sprint(rng.IntN(1e9)) + ">, <mailto:unsub@news.example.com>"}, {"List-Unsubscribe-Post", "List-Unsubscribe=One-Click"},
				{"List-Id", "<weekly.news.example.com>"}, {"Precedence", "bulk"}}
			f := folderFor("newsletter", recent)
			emit(genMsg{Folder: f, Raw: buildRaw(rng, hdrs, text, html, nil, ""), Date: ts, Seen: seenFor(f, recent, "newsletter"), Kind: "newsletter"})
			i++
		case r < 0.87: // notification, some with an attachment (receipts, invites)
			ts := dateAt(i, n)
			from := pick(rng, notifiers)
			subj := pick(rng, []string{"Your order has shipped", "Build failed on main", "New sign-in to your account", "Invitation: ", "Receipt for your payment", "[tracker] Issue updated: "}) + pick(rng, topics)
			text := strings.Join(paragraphs(rng, 800+rng.IntN(2200)), "\n\n")
			html := htmlNewsletter(rng, 10000+rng.IntN(30000), subj)
			var att []byte
			name := ""
			if rng.Float64() < 0.25 {
				att = make([]byte, 6000+rng.IntN(40000))
				for k := range att {
					att[k] = byte(rng.Uint32())
				}
				name = pick(rng, []string{"receipt.pdf", "invite.ics", "report.xlsx", "photo.jpg"})
			}
			hdrs := []header{{"From", "Notifications <" + from + ">"}, {"To", self}, {"Subject", subj}, {"Date", ts.Format(time.RFC1123Z)}, {"Message-ID", mid()}, {"Auto-Submitted", "auto-generated"}}
			f := folderFor("notification", recent)
			emit(genMsg{Folder: f, Raw: buildRaw(rng, hdrs, text, html, att, name), Date: ts, Seen: seenFor(f, recent, "notification"), Flag: rng.Float64() < 0.02, Kind: "notification"})
			i++
		default: // a personal thread of 1 to 14 messages, some long
			size := 1 + rng.IntN(4)
			if rng.Float64() < 0.15 {
				size = 5 + rng.IntN(10)
			}
			if rng.Float64() < 0.005 {
				size = 40 + rng.IntN(40) // a few very long threads
			}
			if i+size > n {
				size = n - i
			}
			otherName, other := person(rng)
			subj := pick(rng, topics)
			var refs []string
			quoted := ""
			base := dateAt(i, n)
			f := folderFor("personal", recent)
			for k := range size {
				ts := base.Add(time.Duration(k) * time.Duration(1+rng.IntN(20)) * time.Hour)
				id := mid()
				mine := k%2 == 1
				from, to := fmt.Sprintf("%s <%s>", otherName, other), fmt.Sprintf("%s <%s>", selfName, self)
				folder := f
				if mine {
					from, to = to, from
					folder = "Sent"
				}
				s := subj
				if k > 0 {
					s = "Re: " + subj
				}
				paras := paragraphs(rng, 300+rng.IntN(2500))
				text := strings.Join(paras, "\n\n")
				if quoted != "" {
					text += "\n\nOn " + ts.Add(-time.Hour).Format("Mon, 2 Jan 2006") + " wrote:\n> " + strings.ReplaceAll(quoted, "\n", "\n> ")
				}
				html := htmlFromText(paras, quoted)
				hdrs := []header{{"From", from}, {"To", to}, {"Subject", s}, {"Date", ts.Format(time.RFC1123Z)}, {"Message-ID", id}}
				if rng.Float64() < 0.3 {
					_, cc := person(rng)
					hdrs = append(hdrs, header{"Cc", cc})
				}
				if len(refs) > 0 {
					hdrs = append(hdrs, header{"In-Reply-To", refs[len(refs)-1]}, header{"References", strings.Join(refs, " ")})
				}
				var att []byte
				name := ""
				if rng.Float64() < 0.08 {
					att = make([]byte, 10000+rng.IntN(60000))
					for x := range att {
						att[x] = byte(rng.Uint32())
					}
					name = pick(rng, []string{"draft.pdf", "photo.jpg", "notes.pdf", "budget.xlsx"})
				}
				// quoted history grows but is capped, as clients trim it
				quoted = text
				if len(quoted) > 12000 {
					quoted = quoted[:12000]
				}
				refs = append(refs, id)
				emit(genMsg{Folder: folder, Raw: buildRaw(rng, hdrs, text, html, att, name), Date: ts, Seen: seenFor(folder, recent, "personal"), Flag: rng.Float64() < 0.03, Kind: "personal"})
				i++
			}
		}
	}
}
