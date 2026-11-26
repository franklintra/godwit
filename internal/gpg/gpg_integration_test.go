package gpg_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/godwit-email/godwit/internal/gpg"
	"github.com/godwit-email/godwit/internal/gpg/gpgtest"
)

// Two keyrings, Alice's and Bob's, each with the other's public key.
func pair(t *testing.T) (alice, bob *gpgtest.Keyring, aliceFpr, bobFpr string) {
	alice, bob = gpgtest.New(t), gpgtest.New(t)
	aliceFpr, bobFpr = alice.Gen("Alice <alice@example.org>"), bob.Gen("Bob <bob@example.org>")
	alice.Import(bob.Export(bobFpr))
	bob.Import(alice.Export(aliceFpr))
	return
}

func TestVersionAndKeys(t *testing.T) {
	k := gpgtest.New(t)
	fpr := k.Gen("Alice <alice@example.org>")
	ctx := context.Background()
	v, err := k.GPG.Version(ctx)
	if err != nil || !strings.HasPrefix(v, "2.") {
		t.Fatalf("version %q, %v", v, err)
	}
	keys, err := k.GPG.SecretKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Fingerprint != fpr || !keys[0].CanSign() || !keys[0].Secret || !keys[0].EncryptsTo("alice@example.org") {
		t.Fatalf("keys = %+v", keys)
	}
	pub, err := k.GPG.PublicKeys(ctx, "<alice@example.org>", "<nobody@example.org>")
	if err != nil || len(pub) != 1 {
		t.Fatalf("a pattern matching nothing still lists the rest: %+v, %v", pub, err)
	}
}

func TestSignVerify(t *testing.T) {
	alice, bob, aliceFpr, _ := pair(t)
	ctx := context.Background()
	data := []byte("Content-Type: text/plain\r\n\r\nhello\r\n")
	sig, hash, err := alice.GPG.Sign(ctx, aliceFpr, data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sig, []byte("-----BEGIN PGP SIGNATURE-----")) || gpg.Micalg(hash) == "" {
		t.Fatalf("sig %q, hash %d", sig, hash)
	}
	r, err := alice.GPG.Verify(ctx, sig, data)
	if err != nil {
		t.Fatal(err)
	}
	if g := r.Signatures[0]; g.Status != gpg.SigGood || g.Trust != gpg.TrustUltimate || g.PrimaryFingerprint != aliceFpr {
		t.Errorf("own signature: %+v", g)
	}
	// Bob has Alice's key but has not certified it: good, of undefined validity.
	r, err = bob.GPG.Verify(ctx, sig, data)
	if err != nil {
		t.Fatal(err)
	}
	if g := r.Signatures[0]; g.Status != gpg.SigGood || g.Trust.Valid() {
		t.Errorf("uncertified: %+v", g)
	}
	bob.Certify(aliceFpr)
	if r, err = bob.GPG.Verify(ctx, sig, data); err != nil || !r.Signatures[0].Trust.Valid() {
		t.Errorf("certified: %+v, %v", r, err)
	}
	r, err = bob.GPG.Verify(ctx, sig, []byte("Content-Type: text/plain\r\n\r\nhellx\r\n"))
	if err != nil || r.Signatures[0].Status != gpg.SigBad {
		t.Errorf("changed data: %+v, %v", r, err)
	}
	stranger := gpgtest.New(t)
	stranger.Gen("Eve <eve@example.org>")
	r, err = stranger.GPG.Verify(ctx, sig, data)
	if err != nil || r.Signatures[0].Status != gpg.SigNoPublicKey {
		t.Errorf("unknown key: %+v, %v", r, err)
	}
	if _, err := alice.GPG.Verify(ctx, []byte("not a signature"), data); err == nil {
		t.Error("garbage must be an error, not a result")
	}
}

func TestEncryptDecrypt(t *testing.T) {
	alice, bob, aliceFpr, bobFpr := pair(t)
	ctx := context.Background()
	data := []byte("Content-Type: text/plain\r\n\r\nsecret\r\n")
	// Bob's key is not valid for Alice yet: gpg refuses it.
	_, err := alice.GPG.Encrypt(ctx, gpg.EncryptOptions{Recipients: []string{bobFpr, aliceFpr}}, data)
	var ge *gpg.Error
	if !errors.As(err, &ge) || !ge.Has("INV_RECP") {
		t.Fatalf("untrusted recipient: %v", err)
	}
	alice.Certify(bobFpr)
	enc, err := alice.GPG.Encrypt(ctx, gpg.EncryptOptions{Recipients: []string{aliceFpr}, Hidden: []string{bobFpr}, SignKey: aliceFpr}, data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(enc, []byte("-----BEGIN PGP MESSAGE-----")) {
		t.Fatalf("enc = %q", enc)
	}
	out, r, err := bob.GPG.Decrypt(ctx, enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) || !r.Encrypted || !r.Decrypted || len(r.Signatures) != 1 || r.Signatures[0].Status != gpg.SigGood {
		t.Errorf("decrypted %q, %+v", out, r)
	}
	out, r, err = alice.GPG.Decrypt(ctx, enc)
	if err != nil || !bytes.Equal(out, data) {
		t.Errorf("the sender reads her own copy: %q, %+v, %v", out, r, err)
	}
	stranger := gpgtest.New(t)
	stranger.Gen("Eve <eve@example.org>")
	if _, _, err := stranger.GPG.Decrypt(ctx, enc); !errors.Is(err, gpg.ErrNoSecretKey) {
		t.Errorf("not encrypted to her: %v", err)
	}
}

func TestClearsigned(t *testing.T) {
	alice, bob, aliceFpr, _ := pair(t)
	ctx := context.Background()
	// gpg --clearsign through the test keyring.
	signed := clearsign(t, alice, aliceFpr, "hello inline\n")
	r, err := bob.GPG.VerifyClearsigned(ctx, signed)
	if err != nil || r.Signatures[0].Status != gpg.SigGood {
		t.Fatalf("clearsigned: %+v, %v", r, err)
	}
	tampered := bytes.Replace(signed, []byte("hello inline"), []byte("hello inlinE"), 1)
	if r, err := bob.GPG.VerifyClearsigned(ctx, tampered); err != nil || r.Signatures[0].Status != gpg.SigBad {
		t.Errorf("tampered: %+v, %v", r, err)
	}
	if _, err := bob.GPG.VerifyClearsigned(ctx, []byte("no signature here")); err == nil {
		t.Error("plain text is no signature")
	}
}

func TestFindConfigured(t *testing.T) {
	if _, err := gpg.Find("/nonexistent/gpg"); !errors.Is(err, gpg.ErrNotFound) {
		t.Errorf("a missing configured path: %v", err)
	}
	if _, err := gpg.Find("relative/gpg"); !errors.Is(err, gpg.ErrNotFound) {
		t.Errorf("a relative path is refused: %v", err)
	}
}

func TestNoBinary(t *testing.T) {
	var g gpg.GPG
	if _, err := g.SecretKeys(context.Background()); !errors.Is(err, gpg.ErrNotFound) {
		t.Errorf("no path: %v", err)
	}
}
