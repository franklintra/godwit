//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

const benchPassword = "perfbench-password"

// loopMail is an in-memory IMAP server and a discarding SMTP server on
// loopback, for one account at a time: the fixture's sync and the sync
// benchmark. Nothing leaves the machine.
type loopMail struct {
	IMAPHost string
	IMAPPort int
	SMTPPort int
	imapLn   net.Listener
	imapSrv  *imapserver.Server
	smtpLn   net.Listener
	smtpSrv  *smtp.Server
	raw      int64 // raw bytes of the mail it holds
	n        int   // and messages
}

type quietLog struct{}

func (quietLog) Printf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, s := range []string{net.ErrClosed.Error(), "EOF", "broken pipe", "connection reset"} {
		if strings.Contains(msg, s) {
			return
		}
	}
	log.Print(msg)
}

type smtpBackend struct{}

func (smtpBackend) NewSession(*smtp.Conn) (smtp.Session, error) { return &smtpSession{}, nil }

type smtpSession struct{}

func (*smtpSession) AuthMechanisms() []string { return []string{sasl.Plain} }
func (*smtpSession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, _, pass string) error {
		if pass != benchPassword {
			return errors.New("invalid password")
		}
		return nil
	}), nil
}
func (*smtpSession) Mail(string, *smtp.MailOptions) error { return nil }
func (*smtpSession) Rcpt(string, *smtp.RcptOptions) error { return nil }
func (*smtpSession) Data(r io.Reader) error               { _, err := io.Copy(io.Discard, r); return err }
func (*smtpSession) Reset()                               {}
func (*smtpSession) Logout() error                        { return nil }

// standardFolders every fixture account has, with their special-use roles
// found by name.
var standardFolders = []string{"INBOX", "Sent", "Archive", "Trash", "Junk", "Drafts"}

// startLoopMail serves email's mailbox holding what gen emits (appended
// in order, so UIDs follow dates) in standardFolders plus extra. It
// returns how many messages and raw bytes it holds.
func startLoopMail(email string, extra []string, gen func(emit func(genMsg))) (*loopMail, int, int64, error) {
	mem := imapmemserver.New()
	u := imapmemserver.NewUser(email, benchPassword)
	for _, f := range append(append([]string{}, standardFolders...), extra...) {
		if err := u.Create(f, &imap.CreateOptions{}); err != nil {
			return nil, 0, 0, fmt.Errorf("create %s: %w", f, err)
		}
	}
	n, raw := 0, int64(0)
	var appendErr error
	gen(func(m genMsg) {
		if appendErr != nil {
			return
		}
		var flags []imap.Flag
		if m.Seen {
			flags = append(flags, imap.FlagSeen)
		}
		if m.Flag {
			flags = append(flags, imap.FlagFlagged)
		}
		if _, err := u.Append(m.Folder, bytes.NewReader(m.Raw), &imap.AppendOptions{Time: m.Date, Flags: flags}); err != nil {
			appendErr = fmt.Errorf("append to %s: %w", m.Folder, err)
		}
		n++
		raw += int64(len(m.Raw))
	})
	if appendErr != nil {
		return nil, 0, 0, appendErr
	}
	mem.AddUser(u)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}, imap.CapMove: {}, imap.CapUIDPlus: {}},
		InsecureAuth: true,
		Logger:       quietLog{},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, 0, err
	}
	go func() { _ = srv.Serve(ln) }()
	ss := smtp.NewServer(smtpBackend{})
	ss.AllowInsecureAuth = true
	sl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		ln.Close()
		return nil, 0, 0, err
	}
	go func() { _ = ss.Serve(sl) }()
	_, ip, _ := net.SplitHostPort(ln.Addr().String())
	_, sp, _ := net.SplitHostPort(sl.Addr().String())
	m := &loopMail{IMAPHost: "127.0.0.1", imapLn: ln, imapSrv: srv, smtpLn: sl, smtpSrv: ss}
	m.IMAPPort, _ = strconv.Atoi(ip)
	m.SMTPPort, _ = strconv.Atoi(sp)
	m.n, m.raw = n, raw
	return m, n, raw, nil
}

func (m *loopMail) Close() {
	m.imapLn.Close()
	m.imapSrv.Close()
	m.smtpLn.Close()
	m.smtpSrv.Close()
}
