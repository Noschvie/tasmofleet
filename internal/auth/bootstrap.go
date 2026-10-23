package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// InitialAdminName is the account created on first run.
const InitialAdminName = "admin"

const (
	generatedPasswordLen = 20
	// No look-alike characters (0/O, 1/l/I): the password is read from a log.
	passwordAlphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// GeneratePassword returns a random password of n characters.
func GeneratePassword(n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = passwordAlphabet[v.Int64()]
	}
	return string(out), nil
}

// InitialPasswordFile is the file in the data directory that holds a
// generated initial admin password until it has been changed.
const InitialPasswordFile = "initial-admin-password"

// EnsureInitialAdmin creates the first admin on a fresh installation: local
// login is on, no local account exists, and either AUTH_MODE=local or an
// INITIAL_ADMIN_PASSWORD was given (so an OIDC installation is not seeded
// with a surprise account). The account must change its password at the
// first login.
//
// Without initialPassword a random one is generated. It is never logged
// (container logs are shipped and kept): it is written once to
// <DataDir>/initial-admin-password (0600) and only the path is logged. The
// file is removed after the first password change, and at startup when the
// account has already changed its password or no longer exists. It returns
// whether an account was created.
func (s *Service) EnsureInitialAdmin(ctx context.Context, initialPassword string) (bool, error) {
	if !s.o.LocalLogin {
		return false, nil
	}
	has, err := s.o.LocalUsers.HasLocalUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("auth: check local users: %w", err)
	}
	if has {
		s.cleanupInitialPassword(ctx)
		if s.setupPending(ctx) {
			slog.WarnContext(ctx, "the initial admin account has no password of its own yet; open the setup link (valid until a password is set) or sign in with the generated password",
				"user", InitialAdminName, "setup", s.issueSetupToken())
		}
		return false, nil
	}
	if s.o.Mode != ModeLocal && initialPassword == "" {
		return false, nil
	}
	generated := initialPassword == ""
	pw := initialPassword
	var path string
	if generated {
		if s.o.DataDir == "" {
			return false, errors.New("auth: a generated initial admin password needs a data directory")
		}
		if pw, err = GeneratePassword(generatedPasswordLen); err != nil {
			return false, err
		}
		path = filepath.Join(s.o.DataDir, InitialPasswordFile)
		// A leftover file belongs to an account that no longer exists.
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("auth: remove stale initial password file: %w", err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // <data dir>/initial-admin-password, operator configuration
		if err != nil {
			return false, fmt.Errorf("auth: create initial password file: %w", err)
		}
		_, werr := f.WriteString(pw + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return false, fmt.Errorf("auth: write initial password file: %w", werr)
		}
	}
	if _, err := s.accounts.create(ctx, NewUser{Name: InitialAdminName, Role: model.RoleAdmin, Password: pw, initialSetup: true}, true, CreatedByFirstStart); err != nil {
		if path != "" {
			_ = os.Remove(path) // no account, so the password is useless; the Add error is the one to report
		}
		return false, fmt.Errorf("auth: create initial admin: %w", err)
	}
	// The setup link goes into the same line as the account (N12): one line
	// in the container log is all a new installation needs.
	link := s.issueSetupToken()
	if generated {
		slog.WarnContext(ctx, "created the initial admin account; open the setup link to set its password (valid until a password is set), or read the generated password from the file, sign in and change it at the first login (the file is then deleted)",
			"user", InitialAdminName, "setup", link, "file", path)
	} else {
		slog.WarnContext(ctx, "created the initial admin account from INITIAL_ADMIN_PASSWORD; open the setup link to set its password (valid until a password is set), or sign in and change it at the first login",
			"user", InitialAdminName, "setup", link)
	}
	return true, nil
}

// cleanupInitialPassword removes the password file once the initial admin
// has changed its password, or no longer exists.
func (s *Service) cleanupInitialPassword(ctx context.Context) {
	if s.o.DataDir == "" {
		return
	}
	u, err := s.o.LocalUsers.GetLocalUser(ctx, InitialAdminName)
	if err == nil && u.MustChangePassword {
		return
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.ErrorContext(ctx, "check initial admin", "err", err)
		return
	}
	s.removeInitialPasswordFile(ctx)
}

func (s *Service) removeInitialPasswordFile(ctx context.Context) {
	if s.o.DataDir == "" {
		return
	}
	if err := os.Remove(filepath.Join(s.o.DataDir, InitialPasswordFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.ErrorContext(ctx, "remove initial admin password file", "err", err)
	}
}

// Accounts returns the account manager bound to this service's stores.
func (s *Service) Accounts() *Accounts { return s.accounts }
