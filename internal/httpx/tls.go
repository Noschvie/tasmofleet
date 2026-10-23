package httpx

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// TLSOptions select the certificate of the UI listener (spec §15.3a).
type TLSOptions struct {
	// CertFile and KeyFile (TLS_CERT_FILE, TLS_KEY_FILE) are re-read by the
	// reload function that TLSConfig returns.
	CertFile, KeyFile string
	// SelfSigned (TLS_SELF_SIGNED) generates an ECDSA P-256 certificate once
	// and persists it in <DataDir>/tls/ (cert.pem, key.pem 0600). It is
	// valid one year and renewed 30 days before expiry.
	SelfSigned bool
	DataDir    string
	// Hosts are the DNS names and IPs of the self-signed certificate
	// (TLS_SANS, plus e.g. the host of PUBLIC_URL). localhost, the loopback
	// addresses and the machine's host name are always included; interface
	// addresses are not (a container's bridge IPs say nothing a client needs).
	Hosts []string
	Now   func() time.Time // default time.Now
	// OnCert, if set, is called with the SHA-256 fingerprint (see
	// CertFingerprint) and expiry of every certificate that becomes the one
	// served: at start, after an in-process renewal, after a reload. It runs
	// under the holder's lock; keep it to logging.
	OnCert func(fingerprint string, notAfter time.Time)
}

const (
	selfSignedValidity = 365 * 24 * time.Hour
	renewBefore        = 30 * 24 * time.Hour
	renewRetry         = time.Hour
)

type certHolder struct {
	o    TLSOptions
	mu   sync.Mutex
	cert *tls.Certificate
	leaf *x509.Certificate
	// retryAt is the earliest next renewal attempt after a failed one, so a
	// read-only data directory costs one attempt per hour, not one per handshake.
	retryAt time.Time
	renew   func() error // generate; replaced in tests
}

func (h *certHolder) now() time.Time {
	if h.o.Now != nil {
		return h.o.Now()
	}
	return time.Now()
}

// TLSConfig returns the server configuration and a function that re-reads
// the certificate files (for SIGHUP). With SelfSigned the function is a
// no-op: renewal happens on its own at the handshake. A reload that fails
// keeps serving the previous certificate.
func TLSConfig(o TLSOptions) (*tls.Config, func() error, error) {
	h := &certHolder{o: o}
	h.renew = h.generate
	switch {
	case o.CertFile != "" || o.KeyFile != "":
		if o.CertFile == "" || o.KeyFile == "" {
			return nil, nil, errors.New("httpx: TLS_CERT_FILE and TLS_KEY_FILE must be set together")
		}
		if err := h.loadFiles(); err != nil {
			return nil, nil, err
		}
		return h.config(), h.loadFiles, nil
	case o.SelfSigned:
		if o.DataDir == "" {
			return nil, nil, errors.New("httpx: self-signed TLS needs a data directory")
		}
		if err := h.loadOrGenerate(); err != nil {
			return nil, nil, err
		}
		return h.config(), func() error { return nil }, nil
	}
	return nil, nil, errors.New("httpx: no TLS certificate source configured")
}

// CertFingerprint returns the SHA-256 of the certificate cfg serves now, as
// upper-case hex pairs separated by colons (what browsers show), so an
// operator can compare it with the log before accepting a self-signed one.
func CertFingerprint(cfg *tls.Config) (string, error) {
	c, err := cfg.GetCertificate(nil)
	if err != nil || c == nil || len(c.Certificate) == 0 {
		return "", errors.New("httpx: no certificate to fingerprint")
	}
	return fingerprintDER(c.Certificate[0]), nil
}

func fingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// announce reports the certificate that is now served. Caller holds h.mu or
// is single-threaded (start).
func (h *certHolder) announce() {
	if h.o.OnCert != nil && h.cert != nil && h.leaf != nil {
		h.o.OnCert(fingerprintDER(h.cert.Certificate[0]), h.leaf.NotAfter)
	}
}

func (h *certHolder) config() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: h.get}
}

func (h *certHolder) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if now := h.now(); h.o.SelfSigned && h.leaf != nil && h.leaf.NotAfter.Sub(now) < renewBefore && !now.Before(h.retryAt) {
		if err := h.renew(); err != nil {
			h.retryAt = now.Add(renewRetry)
			slog.Error("renew self-signed certificate; keeping the current one, retrying in an hour", "err", err)
		}
	}
	return h.cert, nil
}

func (h *certHolder) loadFiles() error {
	pair, err := tls.LoadX509KeyPair(h.o.CertFile, h.o.KeyFile)
	if err != nil {
		return fmt.Errorf("httpx: load TLS key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("httpx: parse TLS certificate: %w", err)
	}
	h.mu.Lock()
	h.cert, h.leaf = &pair, leaf
	h.announce()
	h.mu.Unlock()
	return nil
}

func (h *certHolder) paths() (dir, cert, key string) {
	dir = filepath.Join(h.o.DataDir, "tls")
	return dir, filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

func (h *certHolder) loadOrGenerate() error {
	dir, certPath, keyPath := h.paths()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("httpx: create %s: %w", dir, err)
	}
	if fi, err := os.Stat(keyPath); err == nil {
		if fi.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("httpx: %s has mode %04o, must be 0600", keyPath, fi.Mode().Perm())
		}
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err == nil {
			if leaf, perr := x509.ParseCertificate(pair.Certificate[0]); perr == nil && leaf.NotAfter.Sub(h.now()) >= renewBefore {
				h.cert, h.leaf = &pair, leaf
				h.announce()
				return nil
			}
		}
		// Unreadable pair or due for renewal: replace it below.
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("httpx: stat %s: %w", keyPath, err)
	}
	slog.Warn("TLS_SELF_SIGNED: generating a self-signed certificate; browsers will show a warning until it is trusted", "dir", dir)
	return h.generate()
}

func (h *certHolder) generate() error {
	_, certPath, keyPath := h.paths()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := h.now()
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "tasmofleet", Organization: []string{"tasmofleet (self-signed)"}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(selfSignedValidity),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, name := range append(localNames(), h.o.Hosts...) {
		if ip := net.ParseIP(name); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if name != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	// Key first (0600 from the start), certificate second: a crash between the
	// two leaves a pair that does not match, which loadOrGenerate replaces.
	if err := writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil { //nolint:gosec // certificates are public
		return err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	h.cert, h.leaf = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
	h.announce()
	return nil
}

// writeFile replaces path atomically with the given mode.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("httpx: write %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, mode); err != nil { // WriteFile keeps the mode of a leftover file
		return fmt.Errorf("httpx: chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("httpx: replace %s: %w", path, err)
	}
	return nil
}

func localNames() []string {
	names := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil && h != "" {
		names = append(names, h)
	}
	return names
}

// ReloadOnSIGHUP calls reload on every SIGHUP until ctx ends, logging
// failures (the old certificate stays active).
func ReloadOnSIGHUP(ctx context.Context, reload func() error) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		defer signal.Stop(hup)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := reload(); err != nil {
					slog.ErrorContext(ctx, "reload TLS certificate", "err", err)
				} else {
					slog.InfoContext(ctx, "reloaded TLS certificate")
				}
			}
		}
	}()
}
