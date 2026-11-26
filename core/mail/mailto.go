package mail

import (
	"errors"
	"net/url"
	"strings"
)

// Mailto is a mailto: URI's draft (RFC 6068).
type Mailto struct {
	To, Cc, Bcc   []string
	Subject, Body string
	// Headers are its other fields a draft may carry (MailtoHeader).
	Headers []Header
}

// ParseMailto reads a mailto: URI: the addresses of its path and its to
// field are To; cc, bcc, subject and body are kept, and so are the other
// fields MailtoHeader accepts, in order (at most MaxHeaders); the rest are
// ignored. Values are percent-decoded and + stays + (it is not form
// encoding); one with a bad escape ("50% off") is kept as written. The
// body's line breaks become \n, the subject's spaces.
func ParseMailto(link string) (Mailto, error) {
	scheme, rest, ok := strings.Cut(link, ":")
	if !ok || !strings.EqualFold(scheme, "mailto") {
		return Mailto{}, errors.New("not a mailto link")
	}
	rest, _, _ = strings.Cut(rest, "#")
	path, query, _ := strings.Cut(rest, "?")
	var m Mailto
	addrs := func(dst *[]string, s string) {
		for _, a := range strings.Split(unescape(s), ",") {
			if a = strings.TrimSpace(a); a != "" {
				*dst = append(*dst, a)
			}
		}
	}
	addrs(&m.To, path)
	for _, kv := range strings.Split(query, "&") {
		k, v, _ := strings.Cut(kv, "=")
		switch strings.ToLower(k) {
		case "to":
			addrs(&m.To, v)
		case "cc":
			addrs(&m.Cc, v)
		case "bcc":
			addrs(&m.Bcc, v)
		case "subject":
			m.Subject = unescape(v)
		case "body":
			m.Body = unescape(v)
		default:
			if h, ok := MailtoHeader(unescape(k), unescape(v)); ok && len(m.Headers) < MaxHeaders {
				m.Headers = append(m.Headers, h)
			}
		}
	}
	m.Subject = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(m.Subject)
	m.Body = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(m.Body)
	return m, nil
}

// MailtoHeader is the header field a mailto: URI's field name=value adds
// to its draft, if any: one CheckHeader accepts whose name starts with X-,
// or is Keywords or Comments. A link on a web page or in a list's
// List-Unsubscribe is a stranger's, and its query often carries tracking
// parameters (utm_source=…) that are no header (RFC 6068 section 6 warns
// against taking fields blindly); a program tagging its drafts uses X-.
func MailtoHeader(name, value string) (Header, bool) {
	n := strings.ToLower(name)
	if !strings.HasPrefix(n, "x-") && n != "keywords" && n != "comments" {
		return Header{}, false
	}
	h, err := CheckHeader(Header{Name: name, Value: value})
	return h, err == nil
}

func unescape(s string) string {
	if v, err := url.PathUnescape(s); err == nil {
		return v
	}
	return s
}
