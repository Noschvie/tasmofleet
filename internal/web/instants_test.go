package web

import (
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/firmware"
	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#1104): the sentences that name a moment hand it over as a typed instant, never as text with a date in it, so
// that the page can draw it as a <time> element. Each carrier below is one of the places that used to write the date
// itself with a Go layout.
func TestSentencesKeepTheirInstantTyped(t *testing.T) {
	added := time.Date(2026, 10, 22, 9, 41, 0, 0, time.UTC)
	ny := notYetWords(model.Device{CreatedAt: added, Address: "10.0.0.5"})
	if !ny.At.Equal(added) || !strings.HasPrefix(ny.Lead, "This device has not answered yet (added") || !strings.Contains(ny.Tail, "10.0.0.5") {
		t.Errorf("the 'has not answered yet' banner: %+v", ny)
	}
	if nw := notYetWords(model.Device{Address: "10.0.0.5"}); !nw.IsZero() || !strings.Contains(nw.Lead, "has not answered yet") {
		t.Errorf("a device with no creation time says so without a date: %+v", nw)
	}

	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	bk := ValueSource{Kind: "backup", At: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)}
	if got := valueSource(bk, true, now); !got.At.Equal(bk.At) || got.Lead != "backup · " || got.Stamp.Key != "month-day" {
		t.Errorf("a value from a backup this year: %+v", got)
	}
	bk.At = time.Date(2025, 10, 28, 9, 0, 0, 0, time.UTC)
	if got := valueSource(bk, true, now); got.Stamp.Key != "date" {
		t.Errorf("a value from a backup of an earlier year names the year: %+v", got)
	}
	live := valueSource(ValueSource{Kind: "live", At: now.Add(-6 * time.Minute)}, true, now)
	if live.Lead != "live · 6 min" || !live.IsZero() {
		t.Errorf("a live value is an age in words: %+v", live)
	}
	if got := sourceNote(ValueSource{Kind: "backup", At: bk.At}, now); got.Lead != "from backup of " || got.Stamp.Key != "date" || !got.At.Equal(bk.At) {
		t.Errorf("the per-value note of a backup value: %+v", got)
	}
}

// Why (#1104): a release card's date is the card's own <time> element; the older releases fold uses the same card.
func TestReleaseCardsCarryTheirDateAsAnInstant(t *testing.T) {
	pub := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	cards, older := releaseOptions([]firmware.Release{{Version: "16.2.0", Published: pub}, {Version: "16.1.0", Published: pub}, {Version: "16.0.0"}, {Version: "15.9.0", Published: pub}}, nil, "")
	if len(cards) != 2 || len(older) != 2 {
		t.Fatalf("cards %d, older %d", len(cards), len(older))
	}
	if !cards[0].TextStamp.At.Equal(pub) || cards[0].TextStamp.Lead != "Latest · " || cards[0].TextStamp.Stamp.Key != "date" || cards[0].Text != "" {
		t.Errorf("the latest card: %+v", cards[0])
	}
	if !older[0].TextStamp.IsZero() || older[0].Text != "" {
		t.Errorf("a release without a date has neither: %+v", older[0])
	}
}
