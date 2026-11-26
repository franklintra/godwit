//go:build linux

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"

	"github.com/godwit-email/godwit/core"
)

// memKeyring is the generator's core.Keyring. save writes it in the format
// of cmd/godwit's file keyring (GODWIT_KEYRING_FILE: AES-256-GCM over a JSON
// map of "service\x00user" to secret), so the godwit binary reads the
// fixture's passwords without a Secret Service.
type memKeyring struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemKeyring() *memKeyring { return &memKeyring{m: map[string]string{}} }

func (k *memKeyring) Get(service, user string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[service+"\x00"+user]
	if !ok {
		return "", core.ErrSecretNotFound
	}
	return v, nil
}

func (k *memKeyring) Set(service, user, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[service+"\x00"+user] = secret
	return nil
}

func (k *memKeyring) Delete(service, user string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.m[service+"\x00"+user]; !ok {
		return core.ErrSecretNotFound
	}
	delete(k.m, service+"\x00"+user)
	return nil
}

func (k *memKeyring) save(path, keyPath string) error {
	k.mu.Lock()
	plain, err := json.Marshal(k.m)
	k.mu.Unlock()
	if err != nil {
		return err
	}
	key := make([]byte, 32)
	rand.Read(key)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(path, aead.Seal(nonce, nonce, plain, nil), 0o600)
}
