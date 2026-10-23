package sysconfig

import (
	"context"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Why: only an explicit "1" pauses. A damaged or foreign value must read as
// running, or the automatic checks could stop for good without anyone pausing.
func TestLivePauseReadsOnlyExplicitPause(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "0": false, "true": false, "": false} {
		m := newMem()
		m.rows[LivePausedKey] = store.SystemSetting{Key: LivePausedKey, Value: v, UpdatedBy: "ann"}
		p, err := NewLivePause(context.Background(), m, &memAudit{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Paused() != want {
			t.Errorf("%q: paused=%v", v, p.Paused())
		}
	}
	p, _ := NewLivePause(context.Background(), newMem(), &memAudit{})
	if s := p.State(); s.Paused || s.By != "" {
		t.Fatalf("nothing stored must mean running: %+v", s)
	}
}

// Why: a pause that could not be stored would be lost at the next restart; it
// must fail loud, change nothing and leave no audit entry claiming it happened.
func TestLivePauseStoreFailureChangesNothing(t *testing.T) {
	m, aud := newMem(), &memAudit{}
	p, _ := NewLivePause(context.Background(), m, aud)
	m.fail = true
	if _, changed, err := p.Set(context.Background(), "ann", true, nil); err == nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if p.Paused() || len(aud.ev) != 0 {
		t.Fatalf("paused=%v audit=%v", p.Paused(), aud.ev)
	}
	m.fail = false
	st, changed, err := p.Set(context.Background(), "ann", true, nil)
	if err != nil || !changed || !st.Paused || st.By != "ann" || m.rows[LivePausedKey].Value != "1" {
		t.Fatalf("state %+v changed=%v err=%v", st, changed, err)
	}
	if ev := aud.last(); ev.Action != "policy.live.pause" || ev.Actor != "ann" || ev.Detail != `{"paused":true}` {
		t.Fatalf("audit %+v", ev)
	}
}
