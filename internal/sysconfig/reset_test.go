package sysconfig

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/store"
)

// resetRows seeds stored settings next to the internal rows that share the table.
func resetRows() *memStore {
	st := newMem()
	for k, v := range map[string]string{
		"BACKUP_KEEP":       "4",
		"LOG_LEVEL":         "debug",
		"BACKUP_MAX_AGE":    "720h", // the default value, stored by an earlier save
		setupMarker:         "1",
		LivePausedKey:       "1",
		"NO_LONGER_A_FIELD": "x",
	} {
		st.rows[k] = store.SystemSetting{Key: k, Value: v, UpdatedBy: "ann"}
	}
	return st
}

// Why (N9): "Reset settings…" changes the sign-in settings, the broker and the
// device networks at once, so it always needs the confirmation, and a refused
// reset must leave everything as it was and say so in the audit log.
func TestResetWithoutStepUpIsRefusedAndChangesNothing(t *testing.T) {
	st := resetRows()
	s, aud := newSvc(t, st)
	before := len(st.userRows())
	_, err := s.Reset(context.Background(), ResetRequest{Actor: "ann", StepUp: StepUp{Possible: true}})
	var need *StepUpRequiredError
	if !errors.As(err, &need) || !need.Possible {
		t.Fatalf("err %v", err)
	}
	if len(st.userRows()) != before || s.Snapshot().BackupKeep != 4 {
		t.Fatalf("a refused reset changed the settings: %v", st.rows)
	}
	if ev := aud.last(); ev.Action != "sysconfig.reset" || !strings.HasPrefix(ev.Result, "denied") {
		t.Fatalf("audit %+v", ev)
	}
}

// Why (N9): the table also holds internal rows. The setup marker must stay or
// the first-run setup would start again after a reset; the live check pause
// must stay because only Resume ends a pause; the import marker must stay or
// the environment would be imported again at the next start. A row that is no
// field any more is not the reset's to remove either. Every field row goes,
// also one that holds the default value (afterwards nothing is "stored").
func TestResetRemovesFieldRowsAndKeepsInternalRows(t *testing.T) {
	st := resetRows()
	s, aud := newSvc(t, st)
	var live []string
	s.Subscribe(func(e Event) { live = append(live, e.Keys...) })
	res, err := s.Reset(context.Background(), ResetRequest{Actor: "ann", StepUp: stepped})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"BACKUP_KEEP", "LOG_LEVEL", "BACKUP_MAX_AGE"} {
		if _, ok := st.rows[k]; ok {
			t.Errorf("%s still stored", k)
		}
	}
	for _, k := range []string{setupMarker, LivePausedKey, importMarker, "NO_LONGER_A_FIELD"} {
		if _, ok := st.rows[k]; !ok {
			t.Errorf("internal row %s was deleted", k)
		}
	}
	if !s.SetupDone() {
		t.Error("setup would start again")
	}
	if s.Snapshot().BackupKeep != 10 || s.Snapshot().LogLevel != "info" {
		t.Errorf("not back at the defaults: keep=%d level=%s", s.Snapshot().BackupKeep, s.Snapshot().LogLevel)
	}
	for _, f := range s.State() {
		if f.Source == SourceStored {
			t.Errorf("%s still reads as stored", f.Key)
		}
	}
	if !slices.Contains(live, "LOG_LEVEL") || !slices.Contains(res.Live, "LOG_LEVEL") {
		t.Errorf("the default log level is not applied live: event %v result %v", live, res.Live)
	}
	ev := aud.last()
	if ev.Action != "sysconfig.reset" || ev.Result != "ok" || ev.Actor != "ann" ||
		!strings.Contains(ev.Detail, `"BACKUP_KEEP"`) || !strings.Contains(ev.Detail, `"LOG_LEVEL"`) {
		t.Fatalf("audit %+v", ev)
	}
}

// Why (N9): a field the environment sets is not the page's; a reset must not
// touch it (the page shows it read-only and cannot change it either).
func TestResetLeavesEnvironmentLockedFields(t *testing.T) {
	st := resetRows()
	s, _ := newSvc(t, st, "TASMOFLEET_LOG_LEVEL=warn")
	if _, err := s.Reset(context.Background(), ResetRequest{Actor: "ann", StepUp: stepped}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.rows["LOG_LEVEL"]; !ok {
		t.Error("the row of a locked field was removed")
	}
	if s.Snapshot().LogLevel != "warn" {
		t.Errorf("locked level changed: %s", s.Snapshot().LogLevel)
	}
	if _, ok := st.rows["BACKUP_KEEP"]; ok {
		t.Error("an unlocked field was kept")
	}
}

// Why: a reset with nothing stored still leaves a trace of who pressed it.
func TestResetWithNothingStoredIsAudited(t *testing.T) {
	s, aud := newSvc(t, newMem())
	if _, err := s.Reset(context.Background(), ResetRequest{Actor: "ann", StepUp: stepped}); err != nil {
		t.Fatal(err)
	}
	if ev := aud.last(); ev.Action != "sysconfig.reset" || ev.Result != "ok" {
		t.Fatalf("audit %+v", ev)
	}
}
