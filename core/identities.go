package core

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	netmail "net/mail"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/godwit-email/godwit/core/events"
	"github.com/godwit-email/godwit/core/mail"
	"github.com/godwit-email/godwit/core/store"
)

// Identity is a 发件身份 (ADR 0048): another name and address an account
// sends as, such as a Gmail send-as address, a shared mailbox or a second
// name on the account's own address. Mail still leaves through the account:
// the server decides whether it lets the account send as Email.
type Identity struct {
	ID, AccountID int64
	// Name is the From header's display name, "" for the account's.
	Name string
	// Email is the From header's address.
	Email string
	// ReplyTo is the Reply-To a draft takes when it switches to the
	// identity, "" for none.
	ReplyTo string
	// SignatureID is the signature its drafts end with, 0 for the account's.
	SignatureID int64
	// IsDefault: new mail from the account starts from this identity. At most
	// one of an account's identities is; with none, new mail starts from the
	// account's own address.
	IsDefault bool
}

// IdentityInput is a new identity's fields, as Identity's.
type IdentityInput struct {
	Name, Email, ReplyTo string
	SignatureID          int64
	IsDefault            bool
}

// IdentityPatch changes the identity's fields that are not nil.
type IdentityPatch struct {
	Name, Email, ReplyTo *string
	SignatureID          *int64
	IsDefault            *bool
}

// MaxIdentityName is the most characters an identity's display name has.
const MaxIdentityName = 128

var (
	// ErrIdentityNotFound is an identity id that does not exist, or that is
	// not one of the account it was given with.
	ErrIdentityNotFound = errors.New("identity not found")
	// ErrIdentityExists refuses a second identity with the same name and
	// address in one account.
	ErrIdentityExists = errors.New("the account already has an identity with this name and address")
	// ErrIdentityInvalid is an identity whose address or Reply-To is not one
	// email address, or whose name is too long or spans lines.
	ErrIdentityInvalid = errors.New("invalid identity")
)

const identityCols = "id, account_id, name, email, reply_to, COALESCE(signature_id, 0), is_default"

func scanIdentity(r []any) Identity {
	return Identity{ID: store.Int(r[0]), AccountID: store.Int(r[1]), Name: store.Str(r[2]), Email: store.Str(r[3]),
		ReplyTo: store.Str(r[4]), SignatureID: store.Int(r[5]), IsDefault: store.Int(r[6]) != 0}
}

// ListIdentities returns the account's identities, or every account's for
// accountID 0, by account and then in the order they were added.
func (s *Service) ListIdentities(ctx context.Context, accountID int64) ([]Identity, error) {
	rows, err := s.db.Query(ctx, "SELECT "+identityCols+" FROM identities WHERE ?1 = 0 OR account_id = ?1 ORDER BY account_id, id", accountID)
	if err != nil {
		return nil, err
	}
	out := make([]Identity, 0, len(rows.Values))
	for _, r := range rows.Values {
		out = append(out, scanIdentity(r))
	}
	return out, nil
}

// identity is the identity id; accountID > 0 also requires it to be that
// account's (ErrIdentityNotFound otherwise).
func (s *Service) identity(ctx context.Context, id, accountID int64) (Identity, error) {
	rows, err := s.db.Query(ctx, "SELECT "+identityCols+" FROM identities WHERE id = ? AND (?2 = 0 OR account_id = ?2)", id, accountID)
	if err != nil {
		return Identity{}, err
	}
	if len(rows.Values) == 0 {
		return Identity{}, fmt.Errorf("identity %d: %w", id, ErrIdentityNotFound)
	}
	return scanIdentity(rows.Values[0]), nil
}

// cleanIdentity checks and normalizes in: a bare address (a display name
// given with it becomes Name if Name is empty), a trimmed one-line name, a
// valid Reply-To and an existing signature.
func (s *Service) cleanIdentity(ctx context.Context, in IdentityInput) (IdentityInput, error) {
	a, err := netmail.ParseAddress(strings.TrimSpace(in.Email))
	if err != nil {
		return in, fmt.Errorf("%w: address %q", ErrIdentityInvalid, in.Email)
	}
	in.Email = a.Address
	in.Name = strings.TrimSpace(cmp.Or(in.Name, a.Name))
	if utf8.RuneCountInString(in.Name) > MaxIdentityName || strings.ContainsAny(in.Name, "\r\n") {
		return in, fmt.Errorf("%w: name", ErrIdentityInvalid)
	}
	if in.ReplyTo = strings.TrimSpace(in.ReplyTo); in.ReplyTo != "" {
		r, err := netmail.ParseAddress(in.ReplyTo)
		if err != nil {
			return in, fmt.Errorf("%w: Reply-To %q", ErrIdentityInvalid, in.ReplyTo)
		}
		in.ReplyTo = r.String()
	}
	if in.SignatureID != 0 {
		if _, err := s.signature(ctx, in.SignatureID); err != nil {
			return in, err
		}
	}
	return in, nil
}

// identityAccount is the account that can have identities: one that exists
// and is not a local archive, which never sends.
func (s *Service) identityAccount(ctx context.Context, accountID int64) error {
	acc, err := s.account(ctx, accountID)
	if err != nil {
		return err
	}
	if acc.Kind == KindLocalArchive {
		return ErrLocalArchive
	}
	return nil
}

// checkIdentityUnique is ErrIdentityExists if another identity of the
// account (not skip) has in's name and address, the address ignoring case.
func (s *Service) checkIdentityUnique(ctx context.Context, accountID, skip int64, in IdentityInput) error {
	ids, err := s.ListIdentities(ctx, accountID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(ids, func(i Identity) bool {
		return i.ID != skip && i.Name == in.Name && mail.AddrKey(i.Email) == mail.AddrKey(in.Email)
	}) {
		return ErrIdentityExists
	}
	return nil
}

// identityWrite runs stmts, then clears the other defaults of the account
// when id became its default.
func (s *Service) identityWrite(ctx context.Context, accountID, id int64, isDefault bool, stmts []sqlite.Statement) error {
	if isDefault {
		stmts = append(stmts, sqlite.Statement{SQL: "UPDATE identities SET is_default = 0 WHERE account_id = ? AND id != ?", Args: []any{accountID, id}})
	}
	if _, err := s.db.Transaction(ctx, stmts); err != nil {
		return err
	}
	s.bus.Publish(events.Event{Type: events.IdentitiesChanged, AccountID: accountID})
	return nil
}

// AddIdentity gives the account another name and address to send as.
// Nothing checks that the account's server lets it send as the address: a
// server that does not refuses the mail or rewrites its From (ADR 0048).
func (s *Service) AddIdentity(ctx context.Context, accountID int64, in IdentityInput) (Identity, error) {
	ctx, end, err := s.begin(ctx, accountID)
	if err != nil {
		return Identity{}, err
	}
	defer end()
	if err := s.identityAccount(ctx, accountID); err != nil {
		return Identity{}, err
	}
	if in, err = s.cleanIdentity(ctx, in); err != nil {
		return Identity{}, err
	}
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	if err := s.checkIdentityUnique(ctx, accountID, 0, in); err != nil {
		return Identity{}, err
	}
	// The next id, so the default's statement can name it in the same transaction.
	rows, err := s.db.Query(ctx, `SELECT max(COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'identities'), 0),
		COALESCE((SELECT max(id) FROM identities), 0)) + 1`)
	if err != nil {
		return Identity{}, err
	}
	id := store.Int(rows.Values[0][0])
	if err := s.identityWrite(ctx, accountID, id, in.IsDefault, []sqlite.Statement{{
		SQL:  "INSERT INTO identities(id, account_id, name, email, reply_to, signature_id, is_default) VALUES (?, ?, ?, ?, ?, ?, ?)",
		Args: []any{id, accountID, in.Name, in.Email, in.ReplyTo, nullID(in.SignatureID), b2i(in.IsDefault)},
	}}); err != nil {
		return Identity{}, err
	}
	return s.identity(ctx, id, 0)
}

// UpdateIdentity writes p's fields. Drafts already using the identity keep
// their signature and Reply-To; they send with its new name and address.
func (s *Service) UpdateIdentity(ctx context.Context, id int64, p IdentityPatch) (Identity, error) {
	ctx, end, err := s.begin(ctx, 0)
	if err != nil {
		return Identity{}, err
	}
	defer end()
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	cur, err := s.identity(ctx, id, 0)
	if err != nil {
		return Identity{}, err
	}
	in := IdentityInput{Name: cur.Name, Email: cur.Email, ReplyTo: cur.ReplyTo, SignatureID: cur.SignatureID, IsDefault: cur.IsDefault}
	if p.Name != nil {
		in.Name = *p.Name
	}
	if p.Email != nil {
		in.Email = *p.Email
	}
	if p.ReplyTo != nil {
		in.ReplyTo = *p.ReplyTo
	}
	if p.SignatureID != nil {
		in.SignatureID = *p.SignatureID
	}
	if p.IsDefault != nil {
		in.IsDefault = *p.IsDefault
	}
	if p.Name == nil && p.Email != nil { // an address typed with a name takes it; the old name otherwise
		in.Name = ""
		if a, err := netmail.ParseAddress(strings.TrimSpace(*p.Email)); err == nil && a.Name == "" {
			in.Name = cur.Name
		}
	}
	if in, err = s.cleanIdentity(ctx, in); err != nil {
		return Identity{}, err
	}
	if err := s.checkIdentityUnique(ctx, cur.AccountID, id, in); err != nil {
		return Identity{}, err
	}
	if err := s.identityWrite(ctx, cur.AccountID, id, in.IsDefault, []sqlite.Statement{{
		SQL:  "UPDATE identities SET name = ?, email = ?, reply_to = ?, signature_id = ?, is_default = ? WHERE id = ?",
		Args: []any{in.Name, in.Email, in.ReplyTo, nullID(in.SignatureID), b2i(in.IsDefault), id},
	}}); err != nil {
		return Identity{}, err
	}
	return s.identity(ctx, id, 0)
}

// RemoveIdentity deletes the identity. Its drafts then send as their
// account's own address, keeping their signature and Reply-To; mail already
// in the outbox goes as it was built.
func (s *Service) RemoveIdentity(ctx context.Context, id int64) error {
	ctx, end, err := s.begin(ctx, 0)
	if err != nil {
		return err
	}
	defer end()
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	cur, err := s.identity(ctx, id, 0)
	if err != nil {
		return err
	}
	// Its drafts' identity_id becomes NULL (ON DELETE SET NULL).
	return s.identityWrite(ctx, cur.AccountID, id, false, []sqlite.Statement{{SQL: "DELETE FROM identities WHERE id = ?", Args: []any{id}}})
}

// defaultIdentity is the account's default identity, nil for none.
func (s *Service) defaultIdentity(ctx context.Context, accountID int64) (*Identity, error) {
	ids, err := s.ListIdentities(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for _, i := range ids {
		if i.IsDefault {
			return &i, nil
		}
	}
	return nil, nil
}

// replyIdentity is the identity a reply to orig from acc is sent as, as
// MailMate and Thunderbird pick it: the user's own mail (From one of the
// account's addresses) keeps the identity it was sent from; other mail is
// answered from the address it was sent to, To before Cc. On a match the
// default identity wins, then the account's own address, then the identity
// added first. Mail sent to none of them is answered from the default
// identity, nil being the account's own address.
func replyIdentity(acc Account, ids []Identity, orig *mail.Parsed) *Identity {
	var def *Identity
	for i := range ids {
		if ids[i].IsDefault {
			def = &ids[i]
		}
	}
	candidates := append([]string{orig.From}, append(slices.Clone(orig.To), orig.Cc...)...)
	own := mail.AddrKey(acc.Email)
	for _, addr := range candidates {
		k := mail.AddrKey(addr)
		var match []*Identity
		for j := range ids {
			if mail.AddrKey(ids[j].Email) == k {
				match = append(match, &ids[j])
			}
		}
		switch {
		case k == "" || len(match) == 0 && k != own: // not the user's: From someone else, or another recipient
			continue
		case def != nil && slices.Contains(match, def):
			return def
		case k == own:
			return nil
		}
		return match[0]
	}
	return def
}

// signatureOf is the stored signature drafts from acc as id end with: the
// identity's own, else the account's (Account.Signature).
func (s *Service) signatureOf(ctx context.Context, acc Account, id *Identity) (string, error) {
	if id == nil || id.SignatureID == 0 {
		return acc.Signature, nil
	}
	sig, err := s.signature(ctx, id.SignatureID)
	if errors.Is(err, ErrSignatureNotFound) {
		return acc.Signature, nil
	}
	return sig.HTML, err
}

// fromHeader is the From header of mail from acc as id (nil: as itself).
func fromHeader(accName, accEmail, idName, idEmail string) string {
	if idEmail == "" {
		return (&netmail.Address{Name: accName, Address: accEmail}).String()
	}
	return (&netmail.Address{Name: cmp.Or(idName, accName), Address: idEmail}).String()
}

// ownEmails selects every address that is the user's: each account's and
// each 发件身份's. A local archive's placeholder matches no real mail.
const ownEmails = "SELECT email FROM accounts UNION ALL SELECT email FROM identities"

// accountAddresses are the account's own address and its identities'
// addresses, its own first: the addresses mail from the user in it is from.
func (s *Service) accountAddresses(ctx context.Context, accountIDs ...int64) ([]string, error) {
	var out []string
	for _, id := range accountIDs {
		rows, err := s.db.Query(ctx, "SELECT email FROM accounts WHERE id = ?1 UNION ALL SELECT email FROM identities WHERE account_id = ?1", id)
		if err != nil {
			return nil, err
		}
		for _, r := range rows.Values {
			if a := store.Str(r[0]); !slices.ContainsFunc(out, func(o string) bool { return mail.AddrKey(o) == mail.AddrKey(a) }) {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// addressSets are, by account ID, the AddrKeys of its own address and its
// identities' addresses.
func (s *Service) addressSets(ctx context.Context) (map[int64]map[string]bool, error) {
	rows, err := s.db.Query(ctx, "SELECT id, email FROM accounts UNION ALL SELECT account_id, email FROM identities")
	if err != nil {
		return nil, err
	}
	out := map[int64]map[string]bool{}
	for _, r := range rows.Values {
		id := store.Int(r[0])
		if out[id] == nil {
			out[id] = map[string]bool{}
		}
		out[id][mail.AddrKey(store.Str(r[1]))] = true
	}
	return out, nil
}

// isOneOf reports whether addr's AddrKey is one of addrs'.
func isOneOf(addr string, addrs []string) bool {
	k := mail.AddrKey(addr)
	return k != "" && slices.ContainsFunc(addrs, func(a string) bool { return mail.AddrKey(a) == k })
}
