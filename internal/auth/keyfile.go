package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// KeyFile is the name of the persisted signing key inside the data directory.
const KeyFile = "csrf.key"

const keyFileSize = 32

// LoadOrCreateKey reads <dataDir>/csrf.key or creates it with 32 random
// bytes (mode 0600). The result is Options.SecretKey: the CSRF, session and
// device-cookie keys are derived from it under separate purpose labels, so
// none of them changes at a restart. A file of the wrong size or with a mode
// broader than 0600 is an error, because silently replacing the key would end
// every device cookie and header-mode CSRF token, and a loose mode defeats it.
func LoadOrCreateKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, KeyFile)
	key, err := os.ReadFile(path) //nolint:gosec // <data dir>/csrf.key, operator configuration
	switch {
	case errors.Is(err, fs.ErrNotExist):
		key = make([]byte, keyFileSize)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("auth: generate key: %w", err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // see above
		if err != nil {
			return nil, fmt.Errorf("auth: create key file: %w", err)
		}
		if _, err := f.Write(key); err != nil {
			f.Close() //nolint:errcheck // already failing; the write error is the one to report
			return nil, fmt.Errorf("auth: write key file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("auth: write key file: %w", err)
		}
		return key, nil
	case err != nil:
		return nil, fmt.Errorf("auth: read key file: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("auth: stat key file: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("auth: %s has mode %04o, must be 0600", path, fi.Mode().Perm())
	}
	if len(key) != keyFileSize {
		return nil, fmt.Errorf("auth: %s has %d bytes, want %d", path, len(key), keyFileSize)
	}
	return key, nil
}
