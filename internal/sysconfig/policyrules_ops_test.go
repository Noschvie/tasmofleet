package sysconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/policy"
)

func seedRules(t *testing.T, r *Rules, specs ...RuleSpec) int64 {
	t.Helper()
	id, err := r.Save(context.Background(), "admin", "", AnyVersion, specs, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func commands(specs []RuleSpec) string {
	var out []string
	for _, s := range specs {
		w := s.Command + "=" + s.Want
		if s.Disabled {
			w += "(off)"
		}
		out = append(out, w)
	}
	return strings.Join(out, ",")
}

var (
	tele = RuleSpec{Name: "Telemetry", Command: "TelePeriod", Want: "300", Tags: []string{"plug"}}
	pos  = RuleSpec{Command: "PowerOnState", Want: "3"}
)

// Why: the order is the precedence (N3), so a reorder is a real policy change:
// it must become a new version (revertable), be audited and reach the live
// check at once.
func TestReorderStoresNewVersion(t *testing.T) {
	var live []policy.Rule
	r, st, aud := newRules("", func(rs []policy.Rule) { live = rs })
	base := seedRules(t, r, flash, tele, pos)
	id, err := r.Reorder(context.Background(), "ann", base, []int{2, 0, 1}, nil)
	if err != nil || id != base+1 {
		t.Fatalf("id %d err %v", id, err)
	}
	s, _ := r.State(context.Background())
	if got := commands(s.Specs); got != "PowerOnState=3,SaveData=0,TelePeriod=300" {
		t.Fatalf("order %s", got)
	}
	if len(live) != 3 || live[0].Command != "PowerOnState" {
		t.Fatalf("live check got %+v", live)
	}
	ev := aud.last()
	if ev.Actor != "ann" || ev.Action != "policy.rules.save" || !strings.Contains(ev.Detail, `"op":"reorder"`) {
		t.Fatalf("audit %+v", ev)
	}
	if len(st.vs) != 2 {
		t.Fatalf("versions %d", len(st.vs))
	}
}

// Why: positions only mean something for the version the person looked at;
// an order built on an older version would move the wrong rules.
func TestListOperationsRefuseStaleVersion(t *testing.T) {
	r, st, _ := newRules("", nil)
	base := seedRules(t, r, flash, tele)
	seedRules(t, r, tele, flash) // someone else saved meanwhile
	ctx := context.Background()
	if _, err := r.Reorder(ctx, "a", base, []int{1, 0}, nil); !errors.Is(err, ErrStale) {
		t.Errorf("reorder: %v", err)
	}
	if _, err := r.SetEnabled(ctx, "a", base, 0, false, nil); !errors.Is(err, ErrStale) {
		t.Errorf("enable: %v", err)
	}
	if len(st.vs) != 2 {
		t.Fatalf("a stale operation stored a version: %d", len(st.vs))
	}
}

// Why: a "reorder" that drops or repeats a rule would delete or double a rule
// behind the admin's back.
func TestReorderNeedsAPermutation(t *testing.T) {
	r, _, _ := newRules("", nil)
	base := seedRules(t, r, flash, tele, pos)
	for name, order := range map[string][]int{"short": {0, 1}, "repeat": {0, 0, 1}, "range": {0, 1, 3}, "negative": {-1, 0, 1}} {
		var re *RuleError
		if _, err := r.Reorder(context.Background(), "a", base, order, nil); !errors.As(err, &re) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.Reorder(context.Background(), "a", base, []int{0, 1, 2}, nil); !errors.Is(err, ErrNoChange) {
		t.Errorf("same order must be no change: %v", err)
	}
}

// Why: disabling keeps the rule and its history (the prototype's "Paused while
// testing"), shows up in the version diff, and the live check stops using it.
func TestSetEnabledKeepsRule(t *testing.T) {
	var live []policy.Rule
	r, _, aud := newRules("", func(rs []policy.Rule) { live = rs })
	base := seedRules(t, r, flash, tele)
	id, err := r.SetEnabled(context.Background(), "ann", base, 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || !live[0].Disabled {
		t.Fatalf("live %+v", live)
	}
	d := aud.last().Detail
	if !strings.Contains(d, `"op":"disable"`) || !strings.Contains(d, `state \"enabled\" -\u003e \"disabled\"`) {
		t.Fatalf("audit detail %s", d)
	}
	if _, err := r.SetEnabled(context.Background(), "ann", id, 0, false, nil); !errors.Is(err, ErrNoChange) {
		t.Fatalf("disabling twice: %v", err)
	}
	if _, err := r.SetEnabled(context.Background(), "ann", id, 0, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetEnabled(context.Background(), "ann", id+1, 5, true, nil); !errors.Is(err, ErrNoSuchRule) {
		t.Fatalf("out of range: %v", err)
	}
}

// Why: "disabled" is part of the exported POLICY_RULES file; an export and
// re-import must not turn a parked rule back on.
func TestDisabledSurvivesExportImport(t *testing.T) {
	r, _, _ := newRules("", nil)
	off := tele
	off.Disabled = true
	seedRules(t, r, flash, off)
	data, err := r.Export(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	specs, err := r.ParseImport(data)
	if err != nil || len(specs) != 2 || specs[0].Disabled || !specs[1].Disabled {
		t.Fatalf("specs %+v err %v", specs, err)
	}
}
