package secretbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"", "hunter2", "päss wörd ✓", strings.Repeat("x", 4096)} {
		s, err := b.Seal(in)
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.Open(s)
		if err != nil || got != in {
			t.Fatalf("round trip of %q: got %q, %v", in, got, err)
		}
	}
}

// A fresh nonce per Seal: equal plaintexts must not give equal ciphertexts,
// otherwise a leaked database reveals which devices share a password.
func TestSealIsRandomised(t *testing.T) {
	b, _ := LoadOrCreate(t.TempDir())
	a, _ := b.Seal("same")
	c, _ := b.Seal("same")
	if a == c {
		t.Fatal("two seals of the same plaintext are identical")
	}
	if strings.Contains(a, "same") {
		t.Fatal("ciphertext contains plaintext")
	}
}

// GCM authenticates: a flipped byte or a different key must fail loudly
// instead of returning garbage as a password.
func TestOpenRejectsTamperedAndForeignData(t *testing.T) {
	dir := t.TempDir()
	b, _ := LoadOrCreate(dir)
	s, _ := b.Seal("secret")
	flipped := []byte(s)
	if flipped[len(flipped)/2] == 'A' {
		flipped[len(flipped)/2] = 'B'
	} else {
		flipped[len(flipped)/2] = 'A'
	}
	for name, in := range map[string]string{"tampered": string(flipped), "not base64": "%%%", "too short": "QUJD", "empty": ""} {
		if _, err := b.Open(in); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	other, _ := LoadOrCreate(t.TempDir())
	if _, err := other.Open(s); err == nil {
		t.Error("opened with a different key")
	}
}

// The key must survive a restart, or every stored password becomes unreadable.
func TestLoadOrCreateIsStableAndPrivate(t *testing.T) {
	dir := t.TempDir()
	b1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := b1.Seal("pw")
	b2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b2.Open(s); err != nil || got != "pw" {
		t.Fatalf("second load cannot open: %q %v", got, err)
	}
	fi, err := os.Stat(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Size() != 32 {
		t.Fatalf("key file mode %v size %d", fi.Mode().Perm(), fi.Size())
	}
}

// A damaged or world-readable key must stop startup: silently creating a new
// key would orphan stored passwords, and a loose mode defeats the box.
func TestLoadRefusesBadKeyFile(t *testing.T) {
	for name, tc := range map[string]struct {
		data []byte
		mode os.FileMode
	}{
		"short":          {make([]byte, 16), 0o600},
		"long":           {make([]byte, 64), 0o600},
		"group readable": {make([]byte, 32), 0o640},
		"world readable": {make([]byte, 32), 0o644},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "secret.key")
			if err := os.WriteFile(p, tc.data, tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreate(dir); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadAcceptsStricterMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(p, make([]byte, 32), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFailsWhenDirMissing(t *testing.T) {
	if _, err := LoadOrCreate(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error")
	}
}
