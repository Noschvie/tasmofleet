package firmware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

type opener interface {
	Open(ctx context.Context, a model.Artifact) (io.ReadCloser, error)
}

// Why: the push upload sends the cached file itself, so Open must return exactly
// the bytes the artifact row describes (same size and SHA-256).
func TestOpenReturnsTheCachedFile(t *testing.T) {
	u := newUpstream(t)
	c, _, _ := newTestCache(t, u, nil)
	ctx := context.Background()
	a, err := c.Ensure(ctx, "16.0.0", "tasmota32c3.bin")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.(opener).Open(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close() //nolint:errcheck // test
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if int64(len(b)) != a.Size || hex.EncodeToString(sum[:]) != a.SHA256 {
		t.Fatalf("got %d bytes sha %x, artifact says %d %s", len(b), sum, a.Size, a.SHA256)
	}
}

// Why: a cache file that changed on disk (same size, other content) must never
// be flashed to a device; the artifact's SHA-256 is the proof.
func TestOpenRefusesAFileThatChangedOnDisk(t *testing.T) {
	u := newUpstream(t)
	c, _, o := newTestCache(t, u, nil)
	ctx := context.Background()
	a, err := c.Ensure(ctx, "16.0.0", "tasmota32c3.bin")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(o.Dir, "16.0.0", "tasmota32c3.bin")
	b, _ := os.ReadFile(p)
	b[len(b)-1] ^= 0xFF
	if err := os.WriteFile(p, b, 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	if _, err := c.(opener).Open(ctx, a); err == nil {
		t.Fatal("Open accepted a file whose content no longer matches the artifact")
	}
}

// Why: a missing file is an error, not an empty upload.
func TestOpenMissingFileFails(t *testing.T) {
	u := newUpstream(t)
	c, _, o := newTestCache(t, u, nil)
	ctx := context.Background()
	a, err := c.Ensure(ctx, "16.0.0", "tasmota32c3.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(o.Dir, "16.0.0", "tasmota32c3.bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.(opener).Open(ctx, a); err == nil {
		t.Fatal("Open of a missing file succeeded")
	}
}
