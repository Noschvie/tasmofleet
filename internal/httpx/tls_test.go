package httpx

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func leaf(t *testing.T, cfg *tls.Config) *x509.Certificate {
	t.Helper()
	c, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// Why: a browser warning is bearable once; a new certificate at every
// restart means the user has to re-accept it (and cannot pin it).
func TestSelfSignedIsPersistedAndReused(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	o := TLSOptions{SelfSigned: true, DataDir: dir, Hosts: []string{"fleet.lan", "192.0.2.7"}, Now: func() time.Time { return now }}
	cfg1, _, err := TLSConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	c1 := leaf(t, cfg1)
	cfg2, _, err := TLSConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	if c2 := leaf(t, cfg2); c2.SerialNumber.Cmp(c1.SerialNumber) != 0 {
		t.Fatal("a restart generated a new certificate")
	}
	key := filepath.Join(dir, "tls", "key.pem")
	if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v (want 0600)", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "tls")); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("tls dir: %v, %v", fi, err)
	}
	if _, ok := c1.PublicKey.(*ecdsa.PublicKey); !ok || c1.PublicKey.(*ecdsa.PublicKey).Curve.Params().Name != "P-256" {
		t.Errorf("want ECDSA P-256, got %T", c1.PublicKey)
	}
	if c1.NotAfter.Sub(now) < 364*24*time.Hour || c1.NotAfter.Sub(now) > 367*24*time.Hour {
		t.Errorf("validity until %v", c1.NotAfter)
	}
	if err := c1.VerifyHostname("fleet.lan"); err != nil {
		t.Errorf("configured host: %v", err)
	}
	if err := c1.VerifyHostname("localhost"); err != nil {
		t.Errorf("localhost: %v", err)
	}
	if err := c1.VerifyHostname("192.0.2.7"); err != nil {
		t.Errorf("configured IP missing: %v", err)
	}
	// Only loopback and the configured IP: not every address of every interface.
	if len(c1.IPAddresses) != 3 {
		t.Errorf("SAN IPs %v, want 127.0.0.1, ::1 and the configured one", c1.IPAddresses)
	}
}

// Why: spec §15.3a renews 30 days before expiry, so an unattended install
// does not suddenly serve an expired certificate.
func TestSelfSignedIsRenewedBeforeExpiry(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	clock := &now
	o := TLSOptions{SelfSigned: true, DataDir: dir, Now: func() time.Time { return *clock }}
	cfg, _, err := TLSConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	first := leaf(t, cfg)
	later := now.Add(334 * 24 * time.Hour) // 31 days left
	*clock = later
	if leaf(t, cfg).SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatal("renewed too early")
	}
	later = now.Add(336 * 24 * time.Hour) // 29 days left
	*clock = later
	renewed := leaf(t, cfg)
	if renewed.SerialNumber.Cmp(first.SerialNumber) == 0 || !renewed.NotAfter.After(first.NotAfter) {
		t.Fatal("not renewed 29 days before expiry")
	}
	// The renewal is persisted: a restart reuses it.
	cfg2, _, err := TLSConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	if leaf(t, cfg2).SerialNumber.Cmp(renewed.SerialNumber) != 0 {
		t.Fatal("the renewed certificate was not persisted")
	}
}

func TestSelfSignedRefusesLooseKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	o := TLSOptions{SelfSigned: true, DataDir: dir}
	if _, _, err := TLSConfig(o); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "tls", "key.pem")
	if err := os.Chmod(key, 0o644); err != nil { //nolint:gosec // test: provoke the check
		t.Fatal(err)
	}
	if _, _, err := TLSConfig(o); err == nil {
		t.Fatal("a world-readable private key was accepted")
	}
}

// Why: certificate rotation (ACME client, cert-manager) must not need a
// restart: SIGHUP re-reads the files (spec §15.3a). A broken new pair keeps
// serving the old one.
func TestCertFilesReload(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	mk := func(n string) {
		// Reuse the self-signed generator to produce two different pairs.
		if _, _, err := TLSConfig(TLSOptions{SelfSigned: true, DataDir: filepath.Join(src, n)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(src, n), 0o700); err != nil {
			t.Fatal(err)
		}
		mk(n)
	}
	cp := func(n string) {
		for _, f := range []string{"cert.pem", "key.pem"} {
			b, err := os.ReadFile(filepath.Join(src, n, "tls", f))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, f), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cp("a")
	cfg, reload, err := TLSConfig(TLSOptions{CertFile: filepath.Join(dst, "cert.pem"), KeyFile: filepath.Join(dst, "key.pem")})
	if err != nil {
		t.Fatal(err)
	}
	a := leaf(t, cfg)
	cp("b")
	if err := reload(); err != nil {
		t.Fatal(err)
	}
	b := leaf(t, cfg)
	if a.SerialNumber.Cmp(b.SerialNumber) == 0 {
		t.Fatal("reload did not pick up the new certificate")
	}
	if err := os.WriteFile(filepath.Join(dst, "cert.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reload(); err == nil {
		t.Fatal("a broken pair was accepted")
	}
	if leaf(t, cfg).SerialNumber.Cmp(b.SerialNumber) != 0 {
		t.Fatal("a failed reload dropped the working certificate")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Error("TLS below 1.2 allowed")
	}
}

func TestTLSConfigNeedsASource(t *testing.T) {
	if _, _, err := TLSConfig(TLSOptions{}); err == nil {
		t.Fatal("no certificate source and no error")
	}
	if _, _, err := TLSConfig(TLSOptions{SelfSigned: true}); err == nil {
		t.Fatal("self-signed without a data directory")
	}
}

// Why: with a read-only data directory the renewal fails; it must not run
// (and log) on every handshake, and must keep serving the old certificate.
func TestRenewalFailureBacksOffAndKeepsServing(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	h := &certHolder{o: TLSOptions{SelfSigned: true, DataDir: t.TempDir(), Now: func() time.Time { return now }}}
	h.renew = h.generate
	if err := h.loadOrGenerate(); err != nil {
		t.Fatal(err)
	}
	first := h.leaf.SerialNumber
	attempts := 0
	h.renew = func() error { attempts++; return errors.New("read-only file system") }
	now = now.Add(340 * 24 * time.Hour) // inside the renewal window
	for range 5 {
		c, err := h.get(nil)
		if err != nil || c == nil || h.leaf.SerialNumber.Cmp(first) != 0 {
			t.Fatalf("the old certificate must keep being served: %v", err)
		}
	}
	if attempts != 1 {
		t.Fatalf("%d renewal attempts within one hour, want 1", attempts)
	}
	now = now.Add(61 * time.Minute)
	if _, err := h.get(nil); err != nil || attempts != 2 {
		t.Fatalf("after an hour the renewal is retried: attempts %d, %v", attempts, err)
	}
	h.renew = h.generate
	now = now.Add(61 * time.Minute)
	if _, err := h.get(nil); err != nil || h.leaf.SerialNumber.Cmp(first) == 0 {
		t.Fatalf("a working renewal replaces the certificate: %v", err)
	}
}

// Why (review F2): the fingerprint an operator compares must be the one served
// now. Every change of the served certificate (start, in-process renewal,
// reload) is announced with its fingerprint, and the live fingerprint follows.
func TestCertChangesAreAnnouncedWithTheirFingerprint(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	var got []string
	o := TLSOptions{SelfSigned: true, DataDir: dir, Now: func() time.Time { return now },
		OnCert: func(fp string, _ time.Time) { got = append(got, fp) }}
	cfg, _, err := TLSConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := CertFingerprint(cfg)
	if len(got) != 1 || got[0] != first {
		t.Fatalf("start: announced %v, served %s", got, first)
	}
	now = now.Add(336 * 24 * time.Hour) // inside the renewal window
	renewed, _ := CertFingerprint(cfg)  // the handshake path renews
	if renewed == first {
		t.Fatal("test setup: certificate not renewed")
	}
	if len(got) != 2 || got[1] != renewed {
		t.Errorf("renewal: announced %v, served %s", got, renewed)
	}
}
