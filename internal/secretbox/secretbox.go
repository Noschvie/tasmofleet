// Package secretbox encrypts small secrets (device HTTP passwords) at rest
// with AES-256-GCM. The key is a random 32-byte file in the data directory
// (<data>/secret.key, mode 0600), created on first start. A leaked database
// file alone therefore does not leak device passwords.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// KeyFile is the key's file name inside the data directory.
const KeyFile = "secret.key"

const keySize = 32

// Box seals and opens values with one key.
type Box interface {
	Seal(plaintext string) (string, error) // returns base64(nonce||ciphertext)
	Open(sealed string) (string, error)
}

type gcmBox struct{ aead cipher.AEAD }

// LoadOrCreate reads <dataDir>/secret.key or creates it with 32 random bytes.
// An existing file of the wrong size or with a mode broader than 0600 is an
// error: a new key would orphan every stored password.
func LoadOrCreate(dataDir string) (Box, error) {
	path := filepath.Join(dataDir, KeyFile)
	key, err := os.ReadFile(path) //nolint:gosec // path is <data dir>/secret.key, operator configuration
	switch {
	case errors.Is(err, fs.ErrNotExist):
		key = make([]byte, keySize)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("secretbox: generate key: %w", err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // see above
		if err != nil {
			return nil, fmt.Errorf("secretbox: create key file: %w", err)
		}
		if _, err := f.Write(key); err != nil {
			f.Close() //nolint:errcheck // already failing; the write error is the one to report
			return nil, fmt.Errorf("secretbox: write key file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("secretbox: write key file: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("secretbox: read key file: %w", err)
	default:
		fi, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("secretbox: stat key file: %w", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("secretbox: %s has mode %04o, must be 0600", path, fi.Mode().Perm())
		}
		if len(key) != keySize {
			return nil, fmt.Errorf("secretbox: %s has %d bytes, want %d", path, len(key), keySize)
		}
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	return &gcmBox{aead: aead}, nil
}

func (b *gcmBox) Seal(plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: nonce: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plaintext), nil)), nil
}

func (b *gcmBox) Open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", errors.New("secretbox: sealed value is not base64")
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns+b.aead.Overhead() {
		return "", errors.New("secretbox: sealed value too short")
	}
	pt, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", errors.New("secretbox: cannot open (wrong key or corrupted data)")
	}
	return string(pt), nil
}
