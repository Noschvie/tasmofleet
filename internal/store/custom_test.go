package store

import (
	"errors"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: provenance of an uploaded image (who, which label) must survive a
// restart and must not outlive the image; a stale row would show a label for
// a file that no longer exists.
func TestCustomMetaRoundTripAndCascade(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.PutArtifact(ctx, model.Artifact{Release: "16.0.0.9", Filename: "tasmota.bin.gz", Family: model.ChipESP8266,
		Variant: "tasmota", Size: 10, SHA256: "ab", Source: "custom", FetchedAt: time.Unix(1000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	m := CustomMeta{ArtifactID: id, Label: "my build", OriginalName: "x.bin", UploadedBy: "admin", UploadedAt: time.Unix(2000, 0)}
	if err := s.PutCustomMeta(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListCustomMeta(ctx)
	if err != nil || len(got) != 1 || got[0].Label != "my build" || got[0].UploadedBy != "admin" || !got[0].UploadedAt.Equal(m.UploadedAt) {
		t.Fatalf("list = %+v, %v", got, err)
	}
	if err := s.DeleteArtifact(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ListCustomMeta(ctx); len(got) != 0 {
		t.Fatalf("meta outlived its artifact: %+v", got)
	}
	if err := s.PutCustomMeta(ctx, m); !errors.Is(err, ErrNotFound) {
		t.Fatalf("meta for a missing artifact must be refused, got %v", err)
	}
}

// Why: a self-built image carries the version and file name of the upstream
// release it was built from. Both must live in the table side by side, and the
// upstream lookup must never return the upload (it would be served as official).
func TestCustomArtifactCoexistsWithUpstreamOfTheSameReleaseAndName(t *testing.T) {
	s, _, _ := newStore(t)
	base := model.Artifact{Release: "16.0.0", Filename: "tasmota.bin.gz", Family: model.ChipESP8266, Variant: "tasmota", Size: 10, SHA256: "aa", FetchedAt: time.Unix(1000, 0)}
	up := base
	up.Source = "github"
	upID, err := s.PutArtifact(ctx, up)
	if err != nil {
		t.Fatal(err)
	}
	mine := base
	mine.Source, mine.Custom, mine.SHA256 = "custom", "abcdef0123", "bb"
	myID, err := s.PutArtifact(ctx, mine)
	if err != nil {
		t.Fatalf("same release and name as upstream: %v", err)
	}
	if _, err := s.PutArtifact(ctx, mine); !errors.Is(err, ErrConflict) {
		t.Errorf("the same upload twice: %v", err)
	}
	got, err := s.FindArtifact(ctx, "16.0.0", "tasmota.bin.gz")
	if err != nil || got.ID != upID || got.Custom != "" {
		t.Fatalf("FindArtifact = %+v, %v", got, err)
	}
	got, err = s.FindCustomArtifact(ctx, "abcdef0123", "16.0.0", "tasmota.bin.gz")
	if err != nil || got.ID != myID || got.Release != "16.0.0" || got.Custom != "abcdef0123" {
		t.Fatalf("FindCustomArtifact = %+v, %v", got, err)
	}
	if _, err := s.FindCustomArtifact(ctx, "0000000000", "16.0.0", "tasmota.bin.gz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another key: %v", err)
	}
	if _, err := s.FindCustomArtifact(ctx, "", "16.0.0", "tasmota.bin.gz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an empty key must not reach upstream rows: %v", err)
	}
	all, _ := s.ListArtifacts(ctx)
	for _, a := range all {
		if a.Release != "16.0.0" {
			t.Errorf("the namespace prefix leaked: %+v", a)
		}
	}
}
