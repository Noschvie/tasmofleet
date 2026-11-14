package web

import (
	"crypto/rand"
	"crypto/subtle"
	"sync"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

const (
	pendingCmdTTL = 10 * time.Minute
	pendingCmdMax = 64
)

// pendingCmd is a command that waits for its confirmation step. The command
// text stays here, in memory, and never goes back into a page: its arguments
// may be a secret, and the confirmation only has to carry this token.
type pendingCmd struct {
	binding string // login session (or header subject) that asked
	cmd     string // canonical text
	ids     []model.DeviceID
	// cutOff: the preview named that the device is reached over MQTT only and the command can cut it
	// off; only then does the confirmation carry transport.WithCutOffConsent.
	cutOff bool
	// routes: the transport the confirmation named for each device, for a command whose arguments the audit hides
	// (#826); the sends carry it (transport.WithRoute) and the router keeps to it. Nil for every other command.
	routes map[model.DeviceID]transport.Path
	// notice: the sentences about the transport that confirmation showed (they depend on the broker URL); a send
	// whose sentences would read otherwise now asks again (#826).
	notice  string
	expires time.Time
}

type pendingCmds struct {
	mu sync.Mutex
	m  map[string]pendingCmd
}

// put stores the command for binding and returns its single-use token. Expired
// entries go first; when the store is still full the entry that expires first
// makes room (only admins get here).
func (p *pendingCmds) put(now time.Time, binding, cmd string, ids []model.DeviceID) string {
	return p.add(now, pendingCmd{binding: binding, cmd: cmd, ids: ids})
}

// add is put for a whole entry (its expiry is set here).
func (p *pendingCmds) add(now time.Time, v pendingCmd) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]pendingCmd{}
	}
	for k, v := range p.m {
		if !now.Before(v.expires) {
			delete(p.m, k)
		}
	}
	for len(p.m) >= pendingCmdMax {
		oldest, first := "", time.Time{}
		for k, v := range p.m {
			if first.IsZero() || v.expires.Before(first) {
				oldest, first = k, v.expires
			}
		}
		delete(p.m, oldest)
	}
	token := rand.Text()
	v.expires = now.Add(pendingCmdTTL)
	p.m[token] = v
	return token
}

// take removes and returns the command if it belongs to binding and is fresh.
// Someone else's token stays where it is.
func (p *pendingCmds) take(now time.Time, token, binding string) (pendingCmd, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[token]
	if !ok || subtle.ConstantTimeCompare([]byte(v.binding), []byte(binding)) != 1 {
		return pendingCmd{}, false
	}
	delete(p.m, token)
	if !now.Before(v.expires) {
		return pendingCmd{}, false
	}
	return v, true
}
