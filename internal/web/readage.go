package web

import (
	"time"

	"github.com/hellwand/tasmofleet/internal/settings"
)

// staleAfter is how old a remembered reading may get before a page marks it as stale (#599). The
// settings cache and the GPIO templates service keep their own copy of this hour; a test ties the
// three together.
const staleAfter = settings.StaleAfter

// readAge says when a value shown from memory was read: the words ("3m ago"), the instant for the
// tooltip, and whether it is old enough to carry the stale marker.
type readAge struct {
	At    time.Time
	Words string
	Stale bool
}

func newReadAge(now, at time.Time) readAge {
	words := agoWords(now, at)
	if words == "now" {
		words = "just now"
	}
	return readAge{At: at, Words: words, Stale: now.Sub(at) > staleAfter}
}
