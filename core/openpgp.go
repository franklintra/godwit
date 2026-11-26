package core

import (
	"context"
	"errors"
	"fmt"
	netmail "net/mail"
	"slices"
	"strings"
	"time"

	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/core/store"
	"github.com/godwit-email/godwit/internal/gpg"
)

// OpenPGP runs through the user's own GnuPG (ADR 0045): keys stay in their
// keyring and passphrases with gpg-agent's pinentry. Godwit stores only
// which key an account signs with and the account's and draft's choices.

// ErrGPGNotFound: no gpg binary was found (SetGPGPath, or install GnuPG).
var ErrGPGNotFound = errors.New("GnuPG (gpg) was not found")

// ErrOpenPGPNoKey: signing or encrypting was asked of a draft whose account
// has no OpenPGP key chosen (SetAccountOpenPGP).
var ErrOpenPGPNoKey = errors.New("the account has no OpenPGP key")

// ErrOpenPGPDowngrade: a program (godwit mcp, a plugin, Wit) tried to turn a
// draft's signing or encryption off or down; only the user can.
var ErrOpenPGPDowngrade = errors.New("only the user can turn a draft's signing or encryption off")

// OpenPGPError is a gpg run that failed; Message is gpg's own last words
// ("Operation cancelled", "No secret key"), never secret.
type OpenPGPError struct {
	Op, Message string
}

func (e *OpenPGPError) Error() string { return "OpenPGP " + e.Op + " failed: " + e.Message }

// OpenPGPKeysMissingError: a draft that must be encrypted has recipients
// without a valid key in the user's keyring.
type OpenPGPKeysMissingError struct {
	Addresses []string
}

func (e *OpenPGPKeysMissingError) Error() string {
	return "no valid OpenPGP key for " + strings.Join(e.Addresses, ", ")
}

// Draft encryption choices (DraftOpenPGP.Encrypt).
const (
	OpenPGPEncryptOff  = "off"  // never
	OpenPGPEncryptAuto = "auto" // when every recipient has a valid key, else sent unencrypted
	OpenPGPEncryptOn   = "on"   // always: sending fails while a recipient has no valid key
)

var encryptRank = map[string]int{OpenPGPEncryptOff: 0, OpenPGPEncryptAuto: 1, OpenPGPEncryptOn: 2}

// DraftOpenPGP is how a draft is sent: signed with its account's key, and
// encrypted to its recipients' keys. A new draft follows its account
// (AccountOpenPGP) until SetDraftOpenPGP sets it.
type DraftOpenPGP struct {
	Sign    bool
	Encrypt string // OpenPGPEncryptOff, OpenPGPEncryptAuto or OpenPGPEncryptOn
}

// AccountOpenPGP is an account's OpenPGP settings.
type AccountOpenPGP struct {
	// Key is the fingerprint of the secret key the account signs with and
	// encrypts its own copies to; "" is none, and OpenPGP is off for it.
	Key string
	// SignByDefault signs its new drafts; EncryptWhenPossible encrypts them
	// when every recipient has a valid key (OpenPGPEncryptAuto).
	SignByDefault, EncryptWhenPossible bool
}

// OpenPGPKey is a secret key of the user's keyring.
type OpenPGPKey struct {
	Fingerprint string
	UserIDs     []string
	Algorithm   string
	Created     time.Time
	Expires     time.Time // zero: never
	CanSign     bool      // not expired, revoked or disabled, and has a signing key
}

// GPGInfo is the gpg Godwit runs.
type GPGInfo struct {
	Path, Version string
	Configured    string // the path the user chose (SetGPGPath), "" to find it
	Error         string // why none runs, "" when Path works
}

// gpgRunner is the gpg to run: the path the user chose, else gpg found on
// PATH or where it usually installs.
func (s *Service) gpgRunner(ctx context.Context) (*gpg.GPG, error) {
	conf, err := s.gpgConfigured(ctx)
	if err != nil {
		return nil, err
	}
	path, err := gpg.Find(conf)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGPGNotFound, err)
	}
	return &gpg.GPG{Path: path}, nil
}

func (s *Service) gpgConfigured(ctx context.Context) (string, error) {
	rows, err := s.db.Query(ctx, "SELECT value FROM meta WHERE key = 'gpg_path'")
	if err != nil || len(rows.Values) == 0 {
		return "", err
	}
	return store.Str(rows.Values[0][0]), nil
}

// OpenPGPStatus is the gpg Godwit runs and its version, or why none runs.
func (s *Service) OpenPGPStatus(ctx context.Context) (GPGInfo, error) {
	conf, err := s.gpgConfigured(ctx)
	if err != nil {
		return GPGInfo{}, err
	}
	info := GPGInfo{Configured: conf}
	g, err := s.gpgRunner(ctx)
	if err != nil {
		info.Error = err.Error()
		return info, nil
	}
	info.Path = g.Path
	if info.Version, err = g.Version(ctx); err != nil {
		info.Error = err.Error()
	}
	return info, nil
}

// SetGPGPath makes Godwit run the gpg binary at path (absolute); "" finds
// it again on PATH and in the usual places. Only Godwit's interface sets it
// (rpcbind uiOnly): it names a program Godwit runs.
func (s *Service) SetGPGPath(ctx context.Context, path string) (GPGInfo, error) {
	if path == "" {
		if _, err := s.db.Execute(ctx, "DELETE FROM meta WHERE key = 'gpg_path'"); err != nil {
			return GPGInfo{}, err
		}
		return s.OpenPGPStatus(ctx)
	}
	if _, err := gpg.Find(path); err != nil {
		return GPGInfo{}, fmt.Errorf("%w: %v", ErrGPGNotFound, err)
	}
	if _, err := s.db.Execute(ctx, "INSERT INTO meta(key, value) VALUES ('gpg_path', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", path); err != nil {
		return GPGInfo{}, err
	}
	return s.OpenPGPStatus(ctx)
}

// OpenPGPKeys lists the user's secret keys, the ones an account can sign with.
func (s *Service) OpenPGPKeys(ctx context.Context) ([]OpenPGPKey, error) {
	g, err := s.gpgRunner(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := g.SecretKeys(ctx)
	if err != nil {
		return nil, openpgpError("list keys", err)
	}
	out := make([]OpenPGPKey, 0, len(keys))
	for _, k := range keys {
		var uids []string
		for _, u := range k.UserIDs {
			if u.Validity != "r" {
				uids = append(uids, u.Text)
			}
		}
		out = append(out, OpenPGPKey{Fingerprint: k.Fingerprint, UserIDs: uids, Algorithm: k.Algorithm,
			Created: k.Created, Expires: k.Expires, CanSign: k.CanSign() && k.Secret})
	}
	return out, nil
}

// openpgpError is what gpg's failure tells the user.
func openpgpError(op string, err error) error {
	var ge *gpg.Error
	if errors.As(err, &ge) {
		msg := ge.Error()
		_, msg, _ = strings.Cut(msg, ": ")
		return &OpenPGPError{Op: op, Message: msg}
	}
	if errors.Is(err, gpg.ErrNotFound) {
		return fmt.Errorf("%w: %v", ErrGPGNotFound, err)
	}
	return err
}

// AccountOpenPGP is the account's OpenPGP settings; zero for one never set.
func (s *Service) AccountOpenPGP(ctx context.Context, accountID int64) (AccountOpenPGP, error) {
	if _, err := s.account(ctx, accountID); err != nil {
		return AccountOpenPGP{}, err
	}
	return s.accountOpenPGP(ctx, accountID)
}

func (s *Service) accountOpenPGP(ctx context.Context, accountID int64) (AccountOpenPGP, error) {
	rows, err := s.db.Query(ctx, "SELECT key_fpr, sign, encrypt FROM account_openpgp WHERE account_id = ?", accountID)
	if err != nil || len(rows.Values) == 0 {
		return AccountOpenPGP{}, err
	}
	r := rows.Values[0]
	return AccountOpenPGP{Key: store.Str(r[0]), SignByDefault: store.Int(r[1]) != 0, EncryptWhenPossible: store.Int(r[2]) != 0}, nil
}

// SetAccountOpenPGP saves the account's OpenPGP settings. Key must be a
// fingerprint of a secret key that can sign (OpenPGPKeys), or "" to turn
// OpenPGP off for the account. Drafts that follow the account (not set by
// SetDraftOpenPGP) follow the new defaults.
func (s *Service) SetAccountOpenPGP(ctx context.Context, accountID int64, o AccountOpenPGP) (AccountOpenPGP, error) {
	acc, err := s.account(ctx, accountID)
	if err != nil {
		return AccountOpenPGP{}, err
	}
	if acc.Kind == KindLocalArchive {
		return AccountOpenPGP{}, ErrLocalArchive
	}
	o.Key = strings.ToUpper(strings.TrimSpace(o.Key))
	if o.Key != "" {
		keys, err := s.OpenPGPKeys(ctx)
		if err != nil {
			return AccountOpenPGP{}, err
		}
		if !slices.ContainsFunc(keys, func(k OpenPGPKey) bool { return k.Fingerprint == o.Key && k.CanSign }) {
			return AccountOpenPGP{}, fmt.Errorf("%w: %s is not a secret key that can sign", ErrOpenPGPNoKey, o.Key)
		}
	}
	if _, err := s.db.Execute(ctx, `INSERT INTO account_openpgp(account_id, key_fpr, sign, encrypt) VALUES (?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET key_fpr = excluded.key_fpr, sign = excluded.sign, encrypt = excluded.encrypt`,
		accountID, o.Key, b2i(o.SignByDefault), b2i(o.EncryptWhenPossible)); err != nil {
		return AccountOpenPGP{}, err
	}
	return s.accountOpenPGP(ctx, accountID)
}

// draftOpenPGP is the draft's choices: its own, else its account's defaults.
// sign and encrypt are the draft's columns, NULL when it follows the account.
func (s *Service) draftOpenPGP(ctx context.Context, accountID int64, sign, encrypt any) (DraftOpenPGP, error) {
	a, err := s.accountOpenPGP(ctx, accountID)
	if err != nil {
		return DraftOpenPGP{}, err
	}
	d := DraftOpenPGP{Sign: a.Key != "" && a.SignByDefault, Encrypt: OpenPGPEncryptOff}
	if a.Key != "" && a.EncryptWhenPossible {
		d.Encrypt = OpenPGPEncryptAuto
	}
	if sign != nil {
		d.Sign = store.Int(sign) != 0
	}
	if e := store.Str(encrypt); encryptRank[e] > 0 || e == OpenPGPEncryptOff {
		d.Encrypt = e
	}
	return d, nil
}

// SetDraftOpenPGP sets whether the draft is signed and encrypted when sent;
// the revision grows by one, and revision must be the stored one
// (ErrStaleDraft). A program (godwit mcp, a plugin or Wit) may turn signing
// or encryption on or up, never off or down (ErrOpenPGPDowngrade): only the
// user sends a draft less protected than it was.
func (s *Service) SetDraftOpenPGP(ctx context.Context, draftID, revision int64, o DraftOpenPGP) (out Draft, err error) {
	if err := s.auditMCPDraft(ctx, "SetDraftOpenPGP", draftID); err != nil {
		return Draft{}, err
	}
	if _, ok := encryptRank[o.Encrypt]; !ok {
		return Draft{}, fmt.Errorf("unknown encryption choice %q", o.Encrypt)
	}
	defer func() { s.programChanged(ctx, err) }()
	s.editMu.Lock()
	defer s.editMu.Unlock()
	d, err := s.GetDraft(ctx, draftID)
	if err != nil {
		return Draft{}, err
	}
	if err := s.checkWritable(ctx, draftID); err != nil {
		return Draft{}, err
	}
	if d.Revision != revision {
		return Draft{}, ErrStaleDraft
	}
	if fromProgram(ctx) == 1 && !s.fullRole(ctx) && ((d.OpenPGP.Sign && !o.Sign) || encryptRank[o.Encrypt] < encryptRank[d.OpenPGP.Encrypt]) {
		return Draft{}, ErrOpenPGPDowngrade
	}
	if o.Sign || o.Encrypt != OpenPGPEncryptOff {
		a, err := s.accountOpenPGP(ctx, d.AccountID)
		if err != nil {
			return Draft{}, err
		}
		if a.Key == "" {
			return Draft{}, ErrOpenPGPNoKey
		}
	}
	r, err := s.db.Execute(ctx, `UPDATE drafts SET openpgp_sign = ?, openpgp_encrypt = ?, updated_at = ?, revision = revision + 1,
		from_program = MAX(from_program, ?) WHERE id = ? AND revision = ?`, b2i(o.Sign), o.Encrypt, time.Now().Unix(), fromProgram(ctx), draftID, revision)
	if err != nil {
		return Draft{}, err
	}
	if r.Changes == 0 {
		return Draft{}, ErrStaleDraft
	}
	return s.GetDraft(ctx, draftID)
}

// fullRole: the call comes from the user's own program over the Local API's
// full role, which may do what the user can.
func (s *Service) fullRole(ctx context.Context) bool {
	e, ok := apiAudit(ctx, "", 0)
	return ok && e.Source == AuditLocalAPI && ctx.Value(programKey{}) == nil
}

// OpenPGPRecipient is a recipient and the key a draft would be encrypted to.
type OpenPGPRecipient struct {
	Address string
	Key     string // the fingerprint of a valid encryption key, "" for none
}

// OpenPGPRecipientKeys tells, for each address, the valid key in the user's
// keyring a message to it is encrypted to, if any. It reads the local
// keyring only: no key is looked up anywhere.
func (s *Service) OpenPGPRecipientKeys(ctx context.Context, addrs []string) ([]OpenPGPRecipient, error) {
	bare := make([]string, 0, len(addrs))
	for _, a := range addrs {
		p, err := netmail.ParseAddress(a)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q", a)
		}
		bare = append(bare, p.Address)
	}
	if len(bare) == 0 {
		return nil, nil
	}
	g, err := s.gpgRunner(ctx)
	if err != nil {
		return nil, err
	}
	keys, err := encryptionKeys(ctx, g, bare)
	if err != nil {
		return nil, err
	}
	out := make([]OpenPGPRecipient, len(bare))
	for i, a := range bare {
		out[i] = OpenPGPRecipient{Address: a, Key: keys[strings.ToLower(a)]}
	}
	return out, nil
}

// encryptionKeys maps each lower-cased address of addrs that has a valid
// encryption key to that key's fingerprint.
func encryptionKeys(ctx context.Context, g *gpg.GPG, addrs []string) (map[string]string, error) {
	patterns := make([]string, len(addrs))
	for i, a := range addrs {
		patterns[i] = "<" + a + ">" // gpg's exact-address match
	}
	keys, err := g.PublicKeys(ctx, patterns...)
	if err != nil {
		return nil, openpgpError("list keys", err)
	}
	out := map[string]string{}
	for _, a := range addrs {
		a = strings.ToLower(a)
		for _, k := range keys {
			if k.EncryptsTo(a) {
				out[a] = k.Fingerprint
				break
			}
		}
	}
	return out, nil
}

// outgoingOpenPGP is how enqueue seals the draft d, nil to send it as it is.
// It fails while d must be encrypted and a recipient has no valid key, or d
// is to be signed or encrypted and its account has no key.
func (s *Service) outgoingOpenPGP(ctx context.Context, d Draft) (*mail.OpenPGP, error) {
	if !d.OpenPGP.Sign && d.OpenPGP.Encrypt == OpenPGPEncryptOff {
		return nil, nil
	}
	a, err := s.accountOpenPGP(ctx, d.AccountID)
	if err != nil {
		return nil, err
	}
	if a.Key == "" {
		return nil, ErrOpenPGPNoKey
	}
	g, err := s.gpgRunner(ctx)
	if err != nil {
		return nil, err
	}
	sign := func(data []byte) ([]byte, string, error) {
		sig, hash, err := g.Sign(ctx, a.Key, data)
		if err != nil {
			return nil, "", openpgpError("signing", err)
		}
		return sig, gpg.Micalg(hash), nil
	}
	p := &mail.OpenPGP{}
	if d.OpenPGP.Sign {
		p.Sign = sign
	}
	if d.OpenPGP.Encrypt == OpenPGPEncryptOff {
		return p, nil
	}
	visible, hidden, err := splitRecipients(d)
	if err != nil {
		return nil, err
	}
	keys, err := encryptionKeys(ctx, g, append(slices.Clone(visible), hidden...))
	if err != nil {
		return nil, err
	}
	var missing []string
	o := gpg.EncryptOptions{Recipients: []string{a.Key}} // the sender reads the sent copy
	if d.OpenPGP.Sign {
		o.SignKey = a.Key
	}
	for i, l := range [][]string{visible, hidden} {
		for _, addr := range l {
			k := keys[strings.ToLower(addr)]
			switch {
			case k == "":
				missing = append(missing, addr)
			case i == 0:
				o.Recipients = append(o.Recipients, k)
			default:
				o.Hidden = append(o.Hidden, k) // a Bcc stays unnamed in the encrypted message
			}
		}
	}
	if len(missing) > 0 {
		if d.OpenPGP.Encrypt == OpenPGPEncryptOn {
			return nil, &OpenPGPKeysMissingError{Addresses: missing}
		}
		if p.Sign == nil { // auto: sent unencrypted, signed if asked
			return nil, nil
		}
		return p, nil
	}
	p.Encrypt = func(data []byte) ([]byte, error) {
		out, err := g.Encrypt(ctx, o, data)
		if err != nil {
			return nil, openpgpError("encryption", err)
		}
		return out, nil
	}
	return p, nil
}

// splitRecipients returns the draft's To and Cc addresses (visible) and its
// Bcc ones (hidden), bare and without duplicates; an address in both is visible.
func splitRecipients(d Draft) (visible, hidden []string, err error) {
	seen := map[string]bool{}
	for i, l := range [][]string{d.To, d.Cc, d.Bcc} {
		for _, a := range l {
			p, err := netmail.ParseAddress(a)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid address %q", a)
			}
			if k := strings.ToLower(p.Address); !seen[k] {
				seen[k] = true
				if i < 2 {
					visible = append(visible, p.Address)
				} else {
					hidden = append(hidden, p.Address)
				}
			}
		}
	}
	return visible, hidden, nil
}
