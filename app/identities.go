package app

import (
	"strings"

	"github.com/godwit-email/godwit/core"
)

// accountIdentities are the account's 发件身份 (ADR 0048), in the order added.
func (a *App) accountIdentities(id int64) []core.Identity {
	var out []core.Identity
	for _, i := range a.identities {
		if i.AccountID == id {
			out = append(out, i)
		}
	}
	return out
}

// ownAddress reports whether addr is the user's: an account's address or
// one of its identities'.
func (a *App) ownAddress(addr string) bool {
	for _, acc := range a.accounts {
		if strings.EqualFold(acc.Email, addr) {
			return true
		}
	}
	for _, i := range a.identities {
		if strings.EqualFold(i.Email, addr) {
			return true
		}
	}
	return false
}

// identityAddresses are the account's identities' addresses.
func (a *App) identityAddresses(id int64) []string {
	var out []string
	for _, i := range a.accountIdentities(id) {
		out = append(out, i.Email)
	}
	return out
}
