package auth

import (
	"strconv"
	"sync"
	"time"
)

// stepUpTTL is how long a re-entered password authorises account
// management (spec §15.1).
const stepUpTTL = 5 * time.Minute

// maxStepUps bounds the map; each entry belongs to a live admin session, so
// this is only a guard.
const maxStepUps = 1024

// stepUps remembers, per session, when the password was last re-entered.
// It lives in memory on purpose: after a restart an admin simply re-enters
// the password. Entries are keyed by the session's ID hash, so a step-up
// cannot outlive or cross sessions.
type stepUps struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newStepUps() *stepUps { return &stepUps{m: map[string]time.Time{}} }

// key binds a step-up to the session and the password generation it was
// validated against.
func stepKey(session string, gen int64) string { return session + "/" + strconv.FormatInt(gen, 10) }

func (s *stepUps) grant(session string, gen int64, now time.Time) {
	session = stepKey(session, gen)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= maxStepUps {
		for k, t := range s.m {
			if now.Sub(t) >= stepUpTTL {
				delete(s.m, k)
			}
		}
		if len(s.m) >= maxStepUps {
			clear(s.m) // only reachable with 1024 live admin sessions; they re-enter the password
		}
	}
	s.m[session] = now
}

func (s *stepUps) valid(session string, gen int64, now time.Time) bool {
	session = stepKey(session, gen)
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[session]
	if !ok || now.Sub(t) >= stepUpTTL || now.Before(t) {
		delete(s.m, session)
		return false
	}
	return true
}

func (s *stepUps) revoke(session string, gen int64) {
	session = stepKey(session, gen)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, session)
}
