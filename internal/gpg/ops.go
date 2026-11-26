package gpg

import (
	"context"
	"errors"
	"strconv"
)

// Sign makes an ASCII-armored detached signature of data with key (a
// fingerprint; gpg picks its signing subkey) and returns it with the OpenPGP
// ID of the hash algorithm it used (for RFC 3156's micalg). gpg-agent asks
// for the passphrase if it has none cached, so this may wait for the user.
func (g *GPG) Sign(ctx context.Context, key string, data []byte) (sig []byte, hashAlgo int, err error) {
	if key == "" {
		return nil, 0, errors.New("gpg sign: no key")
	}
	out, status, msgs, err := g.run(ctx, "sign", UserTimeout, data, nil, "--armor", "--detach-sign", "--local-user", key)
	if err != nil {
		return nil, 0, err
	}
	for _, s := range status {
		// SIG_CREATED <type> <pk_algo> <hash_algo> <class> <timestamp> <fpr>
		if s.Keyword == "SIG_CREATED" && s.arg(0) == "D" {
			hashAlgo, _ = strconv.Atoi(s.arg(2))
		}
	}
	if hashAlgo == 0 || len(out) == 0 {
		return nil, 0, &Error{Op: "sign", Status: status, Stderr: msgs, Err: errors.New("no signature made")}
	}
	return out, hashAlgo, nil
}

// EncryptOptions are the keys Encrypt encrypts to, by fingerprint.
type EncryptOptions struct {
	Recipients []string // named in the encrypted message (their key IDs show)
	Hidden     []string // not named (gpg --hidden-recipient): Bcc
	// SignKey, when set, also signs with that key, in one OpenPGP message
	// (RFC 3156 6.2); gpg-agent may ask for its passphrase.
	SignKey string
}

// Encrypt encrypts data to the options' keys and returns it ASCII-armored.
// gpg applies the user's trust model: a key it does not consider valid is
// refused (an *Error with INV_RECP).
func (g *GPG) Encrypt(ctx context.Context, o EncryptOptions, data []byte) ([]byte, error) {
	if len(o.Recipients)+len(o.Hidden) == 0 {
		return nil, errors.New("gpg encrypt: no recipients")
	}
	args := []string{"--armor", "--encrypt"}
	timeout := QuickTimeout
	if o.SignKey != "" {
		args = append(args, "--sign", "--local-user", o.SignKey)
		timeout = UserTimeout
	}
	for _, r := range o.Recipients {
		args = append(args, "--recipient", r)
	}
	for _, r := range o.Hidden {
		args = append(args, "--hidden-recipient", r)
	}
	out, status, msgs, err := g.run(ctx, "encrypt", timeout, data, nil, args...)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, &Error{Op: "encrypt", Status: status, Stderr: msgs, Err: errors.New("nothing encrypted")}
	}
	return out, nil
}

// okVerify accepts gpg's exits 1 (a bad signature) and 2 (a key missing):
// the status lines say which.
func okVerify(code int) bool { return code == 1 || code == 2 }

// Verify checks a detached signature of data. A signature that does not
// verify is a Result, not an error; an error means gpg found no signature
// to check or could not run.
func (g *GPG) Verify(ctx context.Context, sig, data []byte) (Result, error) {
	path, cleanup, err := tempFile(sig)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	_, status, msgs, err := g.run(ctx, "verify", QuickTimeout, data, okVerify, "--verify", "--", path, "-")
	return verified("verify", status, msgs, err)
}

// VerifyClearsigned checks a cleartext-signed text ("-----BEGIN PGP SIGNED
// MESSAGE-----"), as Verify does.
func (g *GPG) VerifyClearsigned(ctx context.Context, text []byte) (Result, error) {
	_, status, msgs, err := g.run(ctx, "verify", QuickTimeout, text, okVerify, "--verify")
	return verified("verify", status, msgs, err)
}

func verified(op string, status []Status, msgs string, err error) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	r := ParseResult(status)
	if len(r.Signatures) == 0 {
		return Result{}, &Error{Op: op, Status: status, Stderr: msgs, Err: errors.New("no signature found")}
	}
	return r, nil
}

// ErrNoSecretKey: the message is encrypted to none of the user's secret keys.
var ErrNoSecretKey = errors.New("encrypted to none of your keys")

// Decrypt decrypts an OpenPGP message (and checks the signatures inside it)
// and returns its plaintext. An OpenPGP message that is only signed comes out
// too, with Result.Encrypted false. gpg-agent may ask for the passphrase.
// The plaintext is returned, never written to disk.
func (g *GPG) Decrypt(ctx context.Context, data []byte) ([]byte, Result, error) {
	out, status, msgs, err := g.run(ctx, "decrypt", UserTimeout, data, okVerify, "--decrypt")
	r := ParseResult(status)
	var ge *Error
	switch {
	case err != nil && errors.As(err, &ge) && r.Encrypted && len(r.NoSecretKey) > 0 && len(r.NoSecretKey) == len(r.EncryptedTo):
		return nil, r, ErrNoSecretKey
	case err != nil:
		return nil, r, err
	case r.Encrypted && !r.Decrypted:
		if len(r.NoSecretKey) > 0 && len(r.NoSecretKey) == len(r.EncryptedTo) {
			return nil, r, ErrNoSecretKey
		}
		return nil, r, &Error{Op: "decrypt", Status: status, Stderr: msgs, Err: errors.New("decryption failed")}
	case !r.Encrypted && len(r.Signatures) == 0:
		return nil, r, &Error{Op: "decrypt", Status: status, Stderr: msgs, Err: errors.New("not an OpenPGP message")}
	}
	return out, r, nil
}
