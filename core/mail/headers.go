package mail

import (
	"fmt"
	"net/textproto"
	"strings"
	"unicode/utf8"
)

// Header is a header field a draft adds to the message it sends, such as a
// program's X-Tracking-ID. Godwit writes the fields it owns itself; see
// CheckHeaders.
type Header struct{ Name, Value string }

// Limits on a draft's headers: every field fits one line of RFC 5322's 998
// unless its value is folded or encoded.
const (
	MaxHeaders     = 32
	MaxHeaderName  = 76
	MaxHeaderValue = 900
)

// HeaderError is a header CheckHeaders refuses: Problem is one of the
// HeaderProblem codes, Name the field as given (empty for too many).
type HeaderError struct{ Name, Problem string }

// The HeaderError problems.
const (
	HeaderBadName  = "invalid_name"  // empty, longer than MaxHeaderName, or not RFC 5322 field-name characters
	HeaderBadValue = "invalid_value" // empty, longer than MaxHeaderValue, not UTF-8, or holding a control character
	HeaderReserved = "reserved"      // a field Godwit writes itself or one that adds or redirects recipients
	HeaderTooMany  = "too_many"      // more than MaxHeaders fields
)

func (e *HeaderError) Error() string {
	switch e.Problem {
	case HeaderBadName: // not echoed: it may be anything
		return "a header field name is empty, too long or not RFC 5322 field-name characters"
	case HeaderBadValue:
		return fmt.Sprintf("header %q: the value is empty, too long or holds a line break or control character", e.Name)
	case HeaderReserved:
		return fmt.Sprintf("header %q: Godwit writes this field itself or it changes who gets mail; use the draft's own fields", e.Name)
	case HeaderTooMany:
		return fmt.Sprintf("more than %d header fields", MaxHeaders)
	}
	return fmt.Sprintf("header %q: %s", e.Name, e.Problem)
}

// reservedHeaders are the fields a draft cannot set, lower case: those Build
// writes (addresses, subject, date, threading, MIME structure), those a
// relay or signer adds, and those that send mail somewhere the draft's
// recipients do not say (replies, receipts, bounces). Reply-To is the
// draft's ReplyTo.
var reservedHeaders = map[string]bool{
	"from": true, "sender": true, "to": true, "cc": true, "bcc": true, "reply-to": true,
	"subject": true, "date": true, "message-id": true, "in-reply-to": true, "references": true,
	"mime-version": true, "return-path": true, "received": true, "received-spf": true,
	"dkim-signature": true, "authentication-results": true,
	"mail-reply-to": true, "mail-followup-to": true, "disposition-notification-to": true,
	"return-receipt-to": true, "errors-to": true, "autocrypt": true, "openpgp": true,
}

// reservedPrefixes are reserved families of fields, lower case: Godwit's
// own (RuleHeader), MIME's, resent blocks, ARC seals and Autocrypt's.
var reservedPrefixes = []string{"x-godwit-", "content-", "resent-", "arc-", "autocrypt-"}

// ReservedHeader reports whether a draft may not set the field name.
func ReservedHeader(name string) bool {
	n := strings.ToLower(name)
	if reservedHeaders[n] {
		return true
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// CheckHeaders returns hs as a draft stores and sends them, or the first
// field it refuses as a *HeaderError. A name is RFC 5322's field-name
// (printable ASCII but the colon) and comes back in canonical case; a value
// is UTF-8 text without line breaks or control characters (tabs become
// spaces), trimmed, that Build encodes (RFC 2047) and folds. Fields keep
// their order, and a name may repeat. Nil stays nil.
func CheckHeaders(hs []Header) ([]Header, error) {
	if hs == nil {
		return nil, nil
	}
	if len(hs) > MaxHeaders {
		return nil, &HeaderError{Problem: HeaderTooMany}
	}
	out := make([]Header, 0, len(hs))
	for _, h := range hs {
		c, err := CheckHeader(h)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// CheckHeader is CheckHeaders for one field.
func CheckHeader(h Header) (Header, error) {
	if h.Name == "" || len(h.Name) > MaxHeaderName {
		return Header{}, &HeaderError{Name: h.Name, Problem: HeaderBadName}
	}
	for i := 0; i < len(h.Name); i++ {
		if c := h.Name[i]; c < 33 || c > 126 || c == ':' {
			return Header{}, &HeaderError{Name: h.Name, Problem: HeaderBadName}
		}
	}
	if ReservedHeader(h.Name) {
		return Header{}, &HeaderError{Name: h.Name, Problem: HeaderReserved}
	}
	v := strings.ReplaceAll(h.Value, "\t", " ")
	bad := !utf8.ValidString(v) || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == ' ' || r == ' ' })
	if v = strings.Trim(v, " "); bad || v == "" || len(v) > MaxHeaderValue {
		return Header{}, &HeaderError{Name: h.Name, Problem: HeaderBadValue}
	}
	return Header{Name: textproto.CanonicalMIMEHeaderKey(h.Name), Value: v}, nil
}
