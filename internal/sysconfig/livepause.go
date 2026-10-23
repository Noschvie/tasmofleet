package sysconfig

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// LivePausedKey is the system_settings row of the policy live check pause
// (design v2, N21). It is not a Field: the policy page switches it, the
// system settings page does not list it, and no environment variable sets it.
const LivePausedKey = "policy_live_paused"

// LivePauseState is the pause of the automatic policy live check. By and At
// say who changed it last and when (zero before the first change).
type LivePauseState struct {
	Paused bool
	By     string
	At     time.Time
}

// LivePause holds the global pause flag of the automatic policy live check.
// While paused, the background reads stop; the on-demand check ("Run now")
// and MQTT updates stay. The flag is persisted, so it survives a restart, and
// every change is audited (actor and new state, no device data).
type LivePause struct {
	store Store
	audit audit.Recorder

	mu  sync.Mutex // serialises Set; guards cur
	cur LivePauseState
}

// NewLivePause loads the stored state.
func NewLivePause(ctx context.Context, st Store, rec audit.Recorder) (*LivePause, error) {
	if rec == nil {
		return nil, errors.New("sysconfig: an audit recorder is required")
	}
	p := &LivePause{store: st, audit: rec}
	cur, err := p.load(ctx)
	if err != nil {
		return nil, err
	}
	p.cur = cur
	return p, nil
}

func (p *LivePause) load(ctx context.Context) (LivePauseState, error) {
	rows, err := p.store.LoadSystemSettings(ctx)
	if err != nil {
		return LivePauseState{}, err
	}
	r, ok := rows[LivePausedKey]
	if !ok {
		return LivePauseState{}, nil
	}
	// Anything but "1" reads as running: a damaged row must not silently stop
	// the checks for good.
	return LivePauseState{Paused: r.Value == "1", By: r.UpdatedBy, At: r.UpdatedAt}, nil
}

// State returns the current state.
func (p *LivePause) State() LivePauseState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

// Paused reports whether the automatic live check is paused.
func (p *LivePause) Paused() bool { return p.State().Paused }

// Set pauses (paused=true) or resumes the automatic live check. Setting the
// state it already has stores and audits nothing (changed=false). rec, if not
// nil, receives the audit record instead of the service's recorder.
func (p *LivePause) Set(ctx context.Context, actor string, paused bool, rec audit.Recorder) (st LivePauseState, changed bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur.Paused == paused {
		return p.cur, false, nil
	}
	v, action := "0", "policy.live.resume"
	if paused {
		v, action = "1", "policy.live.pause"
	}
	if err := p.store.SaveSystemSettings(ctx, actor, []store.SystemSettingChange{{Key: LivePausedKey, Value: v}}); err != nil {
		return p.cur, false, err
	}
	cur, err := p.load(ctx) // the store sets the time
	if err != nil {
		return p.cur, false, err
	}
	p.cur = cur
	if rec == nil {
		rec = p.audit
	}
	d, _ := json.Marshal(map[string]bool{"paused": paused}) // a plain map always encodes
	rec.Record(ctx, model.AuditEvent{Actor: actor, Action: action, Target: "policy:live", Detail: string(d), Result: "ok"})
	return p.cur, true, nil
}
