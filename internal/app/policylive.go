package app

import (
	"context"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/settings"
)

const (
	pollTick       = 30 * time.Second
	pollParallel   = 2
	pollFirstSpan  = 10 * time.Minute // first reads are spread over this window after the start
	pollRetryAfter = 10 * time.Minute // a failed or skipped device is looked at again after this
)

// startPolicyLive starts the background reads and the MQTT listener that keep
// the live policy values fresh.
func (a *runtime) startPolicyLive() {
	rep := a.policyLive
	if a.bus != nil {
		ch := make(chan policyUpdate, 64)
		a.bus.Subscribe([]mqttbus.Kind{mqttbus.KindStatus, mqttbus.KindResult}, func(m mqttbus.Message) {
			if m.Retained { // a retained message can be arbitrarily old
				return
			}
			vals := settings.ParseReply(m.Payload)
			if len(vals) == 0 {
				return
			}
			select {
			case ch <- policyUpdate{topic: m.Topic, vals: vals, at: m.At}:
			default: // a burst of replies: the hourly read catches up
			}
		})
		a.goBG("policy-mqtt", func() error { rep.applyUpdates(a.bgContext, ch, a.svc.Store.GetDeviceByTopic); return nil })
	}
	// The interval is a system setting: a change applies at the next tick, 0 pauses the reads.
	// Pause / Resume on the policy page (N21) stops the background reads too;
	// the on-demand check and the MQTT updates stay.
	p := &policyPoller{rep: rep, interval: func() time.Duration { return a.svc.Sys.Snapshot().PolicyLiveInterval }, offline: func(id model.DeviceID) bool {
		return a.tracker != nil && a.tracker.Get(id).State == model.HealthOffline
	}, paused: a.livePause.Paused}
	a.goBG("policy-poll", func() error { p.run(a.bgContext, pollTick); return nil })
}

type policyUpdate struct {
	topic string
	vals  map[string]string
	at    time.Time
}

// applyUpdates stores values the devices announced themselves. Only settings
// that some rule names are kept (the store stays small).
func (p *policyReporter) applyUpdates(ctx context.Context, ch <-chan policyUpdate, byTopic func(context.Context, string) (model.Device, error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case u := <-ch:
			d, err := byTopic(ctx, u.topic)
			if err != nil {
				continue // not one of ours
			}
			// A device spells a generic reply as it likes; a rule's value is stored under the
			// rule's own spelling.
			want := map[string]string{}
			for _, c := range ruleCommands(p.currentRules()) {
				want[strings.ToLower(c)] = c
			}
			keep := map[string]string{}
			for k, v := range u.vals {
				if c, ok := want[strings.ToLower(k)]; ok {
					keep[c] = v
				}
			}
			if len(keep) > 0 {
				at := u.at
				if at.IsZero() {
					at = p.clock()
				}
				p.vals.put(d.ID, keep, at)
			}
		}
	}
}

// policyPoller reads each device about once per interval (jittered by
// +-20 %), a few at a time, and never an offline one.
type policyPoller struct {
	rep      *policyReporter
	interval func() time.Duration // current value of the setting; 0 = no background reads
	offline  func(model.DeviceID) bool
	paused   func() bool // the live check is paused (N21); nil = never
	due      map[model.DeviceID]time.Time
	jitter   func() float64 // 0..1, default rand
}

func (p *policyPoller) run(ctx context.Context, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.once(ctx)
		}
	}
}

func (p *policyPoller) rnd() float64 {
	if p.jitter != nil {
		return p.jitter()
	}
	return rand.Float64() //nolint:gosec // G404: spreading load, not security
}

// once reads every device that is due.
func (p *policyPoller) once(ctx context.Context) {
	interval := p.interval()
	if interval <= 0 || (p.paused != nil && p.paused()) {
		return
	}
	devs, err := p.rep.devices.ListDevices(ctx)
	if err != nil {
		return
	}
	now := p.rep.clock()
	if p.due == nil {
		p.due = map[model.DeviceID]time.Time{}
	}
	live := map[model.DeviceID]bool{}
	var todo []model.Device
	for _, d := range devs {
		live[d.ID] = true
		due, known := p.due[d.ID]
		if !known {
			span := min(interval, pollFirstSpan)
			p.due[d.ID] = now.Add(time.Duration(p.rnd() * float64(span)))
			continue
		}
		if now.Before(due) {
			continue
		}
		if p.offline(d.ID) {
			p.due[d.ID] = now.Add(pollRetryAfter)
			continue
		}
		todo = append(todo, d)
	}
	for id := range p.due {
		if !live[id] {
			delete(p.due, id)
		}
	}
	if len(todo) == 0 {
		return
	}
	// parallel reads, results collected before the due times are set
	type res struct {
		id model.DeviceID
		ok bool
	}
	out := make(chan res, len(todo))
	sem := make(chan struct{}, pollParallel)
	for _, d := range todo {
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			out <- res{d.ID, p.rep.readDevice(ctx, d.ID)}
		}()
	}
	for range todo {
		r := <-out
		if r.ok {
			f := 0.8 + 0.4*p.rnd()
			p.due[r.id] = p.rep.clock().Add(time.Duration(f * float64(interval)))
		} else {
			p.due[r.id] = p.rep.clock().Add(min(pollRetryAfter, interval))
		}
	}
}
