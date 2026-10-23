package auth

import (
	"crypto/sha256"
	"sync"
	"time"
)

// Limiter is the break-glass login rate limit. Every attempt is counted
// when it is admitted (before the password is hashed), so parallel requests
// cannot slip past a budget. Three tiers apply:
//
//   - per (client network, username): 5 attempts free, then an exponential
//     delay since the last attempt (1, 2, 4, 8 min, capped at 15 min), over
//     a 1 h memory. A successful login clears only this pair. Always applies.
//   - per client network, across usernames: 20 attempts per 15 min, then 1
//     per minute, so rotating usernames does not help.
//   - per username, across networks: 30 attempts per 15 min, then 1 per
//     10 s, bounding distributed guessing.
//
// Recognised clients (a valid device cookie for that username, or an
// address in the configured admin networks) skip the two global tiers, so
// an attacker who keeps the username throttled cannot lock out the real
// admin (OWASP "device cookies"). Instead they have their own pair table
// and their own per-username tier (10 per 15 min, then 1 per minute), which
// unrecognised clients cannot touch; it bounds a stolen device cookie used
// from many networks. See docs/security/login-rate-limiting.md.
//
// Memory is bounded: keys are fixed-size hashes, every table has a maximum
// size, and when a table is full new keys share one overflow bucket per
// tier (still limited by the same rules; nothing grows without bound).
// Expired entries are pruned at most once a minute.
type Limiter struct {
	// MaxKeys bounds each table; default 100000 (a few MB at most).
	MaxKeys int

	mu        sync.Mutex
	tiers     [5]tier // pair, net, user, recognised pair, recognised user
	lastPrune time.Time
}

// Limits (spec §15.1, as amended by the WP6 security reviews).
const (
	pairFree   = 5
	pairMemory = time.Hour
	pairBase   = time.Minute
	pairCap    = 15 * time.Minute

	netBudget = 20
	netWindow = 15 * time.Minute
	netSlow   = time.Minute

	userBudget = 30
	userWindow = 15 * time.Minute
	userSlow   = 10 * time.Second

	recUserBudget = 10
	recUserWindow = 15 * time.Minute
	recUserSlow   = time.Minute

	defaultMaxKeys    = 100000
	recognisedMaxKeys = 4096
	pruneEvery        = time.Minute
)

const (
	tPair = iota
	tNet
	tUser
	tRecognised
	tRecognisedUser
)

type key [16]byte

type tier struct {
	m        map[key][]time.Time
	overflow []time.Time
	window   time.Duration
}

func hashKey(parts ...string) key {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	var k key
	copy(k[:], h.Sum(nil))
	return k
}

func (l *Limiter) init() {
	if l.tiers[0].m != nil {
		return
	}
	for i, w := range []time.Duration{pairMemory, netWindow, userWindow, pairMemory, recUserWindow} {
		l.tiers[i] = tier{m: map[key][]time.Time{}, window: w}
	}
}

func (l *Limiter) maxKeys(t int) int {
	if t == tRecognised || t == tRecognisedUser {
		return recognisedMaxKeys
	}
	if l.MaxKeys > 0 {
		return l.MaxKeys
	}
	return defaultMaxKeys
}

// slot returns the recent attempts of k in tier t and a function that
// stores an updated list (in the table, or in the overflow bucket when the
// table is full and k is new).
func (l *Limiter) slot(t int, k key, now time.Time) ([]time.Time, func([]time.Time)) {
	tr := &l.tiers[t]
	if ts, ok := tr.m[k]; ok || len(tr.m) < l.maxKeys(t) {
		return trim(ts, now, tr.window), func(v []time.Time) { tr.m[k] = v }
	}
	return trim(tr.overflow, now, tr.window), func(v []time.Time) { tr.overflow = v }
}

func trim(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= window {
		i++
	}
	return ts[i:]
}

// Allow reports whether an attempt by network for user may be checked now;
// recognised clients skip the global tiers. If allowed, the attempt is
// recorded; otherwise retry is the earliest time to try again.
func (l *Limiter) Allow(now time.Time, network, user string, recognised bool) (ok bool, retry time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	l.prune(now)

	pairTier := tPair
	if recognised {
		pairTier = tRecognised
	}
	p, setP := l.slot(pairTier, hashKey("pair", network, user), now)
	wait := time.Duration(0)
	if len(p) >= pairFree {
		d := pairCap
		if shift := len(p) - pairFree; shift < 5 {
			d = min(pairBase<<shift, pairCap)
		}
		wait = max(wait, d-now.Sub(p[len(p)-1]))
	}
	var n, u []time.Time
	var setN, setU func([]time.Time)
	if recognised {
		u, setU = l.slot(tRecognisedUser, hashKey("user", user), now)
		if len(u) >= recUserBudget {
			wait = max(wait, recUserSlow-now.Sub(u[len(u)-1]))
		}
	} else {
		n, setN = l.slot(tNet, hashKey("net", network), now)
		u, setU = l.slot(tUser, hashKey("user", user), now)
		if len(n) >= netBudget {
			wait = max(wait, netSlow-now.Sub(n[len(n)-1]))
		}
		if len(u) >= userBudget {
			wait = max(wait, userSlow-now.Sub(u[len(u)-1]))
		}
	}
	if wait > 0 {
		return false, wait
	}
	setP(append(p, now))
	setU(append(u, now))
	if !recognised {
		setN(append(n, now))
	}
	return true, 0
}

// Remaining is how many more attempts by network for user Allow would admit
// right now before it starts to delay them ("attempts left before lock" on
// the sign-in page, N11). It is a count only: it never reveals how long a
// delay lasts. It records nothing.
func (l *Limiter) Remaining(now time.Time, network, user string, recognised bool) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	pairTier := tPair
	if recognised {
		pairTier = tRecognised
	}
	p, _ := l.slot(pairTier, hashKey("pair", network, user), now)
	left := pairFree - len(p)
	if recognised {
		u, _ := l.slot(tRecognisedUser, hashKey("user", user), now)
		left = min(left, recUserBudget-len(u))
	} else {
		n, _ := l.slot(tNet, hashKey("net", network), now)
		u, _ := l.slot(tUser, hashKey("user", user), now)
		left = min(left, netBudget-len(n), userBudget-len(u))
	}
	return max(left, 0)
}

// Success clears the (network, user) pair after a successful login. The
// global tiers keep the attempt, and other pairs are untouched.
func (l *Limiter) Success(network, user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.init()
	k := hashKey("pair", network, user)
	delete(l.tiers[tPair].m, k)
	delete(l.tiers[tRecognised].m, k)
}

// prune drops expired entries, at most once per pruneEvery, so a flood
// cannot turn pruning into a CPU sink.
func (l *Limiter) prune(now time.Time) {
	if now.Sub(l.lastPrune) < pruneEvery {
		return
	}
	l.lastPrune = now
	for i := range l.tiers {
		tr := &l.tiers[i]
		for k, ts := range tr.m {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= tr.window {
				delete(tr.m, k)
			}
		}
		tr.overflow = trim(tr.overflow, now, tr.window)
	}
}

// size returns the number of keys per tier (tests).
func (l *Limiter) size() [5]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	var s [5]int
	for i := range l.tiers {
		s[i] = len(l.tiers[i].m)
	}
	return s
}
