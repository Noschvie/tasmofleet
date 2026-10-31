package tasmotacmd

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Set is the pinned catalogues, oldest version first.
type Set struct {
	cats []*Catalogue
	vers [][3]int
}

// NewSet orders catalogues by their Tasmota version. A version that is not X.Y.Z, or two
// catalogues of one version, are errors.
func NewSet(cats []*Catalogue) (*Set, error) {
	if len(cats) == 0 {
		return nil, errors.New("tasmotacmd: no catalogue")
	}
	type pair struct {
		c *Catalogue
		v [3]int
	}
	ps := make([]pair, 0, len(cats))
	for _, c := range cats {
		v, ok := ParseVersion(c.Tasmota)
		if !ok {
			return nil, fmt.Errorf("tasmotacmd: catalogue version %q is not X.Y.Z", c.Tasmota)
		}
		ps = append(ps, pair{c, v})
	}
	sort.Slice(ps, func(i, j int) bool { return cmpVer(ps[i].v, ps[j].v) < 0 })
	s := &Set{}
	for i, p := range ps {
		if i > 0 && p.v == ps[i-1].v {
			return nil, fmt.Errorf("tasmotacmd: two catalogues for %s", p.c.Tasmota)
		}
		s.cats = append(s.cats, p.c)
		s.vers = append(s.vers, p.v)
	}
	return s, nil
}

// ParseVersion reads "16.0.0" or "16.0.0.3" (a development or patch build); the fourth field is
// dropped, such a build is judged like its release.
func ParseVersion(v string) ([3]int, bool) {
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) < 3 || len(parts) > 4 {
		return [3]int{}, false
	}
	var out [3]int
	for i := range 3 {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	if len(parts) == 4 {
		if _, err := strconv.Atoi(parts[3]); err != nil {
			return [3]int{}, false
		}
	}
	return out, true
}

func cmpVer(a, b [3]int) int {
	for i := range 3 {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Versions lists the pinned versions, oldest first.
func (s *Set) Versions() []string {
	out := make([]string, len(s.cats))
	for i, c := range s.cats {
		out[i] = c.Tasmota
	}
	return out
}

// Newest is the catalogue of the newest pinned version.
func (s *Set) Newest() *Catalogue { return s.cats[len(s.cats)-1] }

// Match says how a firmware version is judged.
type Match int

// How a catalogue stands to the version it judges.
const (
	// MatchExact: a catalogue is pinned for this very version.
	MatchExact Match = iota + 1
	// MatchNewer: no catalogue is pinned for the version; the next newer pinned one is used. A
	// command present there may be newer than the version, and one missing there may have been
	// dropped since (SafePower is in 13.4.0 and gone in 14.4.1): a miss is certain only when the
	// next older pin lacks it too (Set.Older).
	MatchNewer
	// MatchBeyond: the version is newer than every pin. A command missing from the newest
	// catalogue may have been added since, so nothing can be said against it.
	MatchBeyond
)

// For picks the catalogue that judges a firmware version. ok is false for a version that cannot be read.
func (s *Set) For(version string) (cat *Catalogue, m Match, ok bool) {
	v, ok := ParseVersion(version)
	if !ok {
		return nil, 0, false
	}
	for i, pv := range s.vers {
		switch c := cmpVer(pv, v); {
		case c == 0:
			return s.cats[i], MatchExact, true
		case c > 0:
			return s.cats[i], MatchNewer, true
		}
	}
	return s.Newest(), MatchBeyond, true
}

// Older is the catalogue pinned just below the version of c, nil when c is the oldest pin.
func (s *Set) Older(c *Catalogue) *Catalogue {
	for i, x := range s.cats {
		if x == c && i > 0 {
			return s.cats[i-1]
		}
	}
	return nil
}
