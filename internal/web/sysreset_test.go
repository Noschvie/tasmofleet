package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// Why (#623): the Reset control is "reset to default on save". For the schedule intervals the
// default is 0s = off, which is what the schedule switch already does; a Reset on the interval
// would switch the schedule off while the switch still showed On. Both intervals therefore offer
// no Reset at all (the switch is the one control), whatever is stored.
func TestScheduleIntervalsOfferNoReset(t *testing.T) {
	for _, key := range []string{"BACKUP_INTERVAL", "SNAPSHOT_INTERVAL"} {
		row := sysRow{FieldState: sysconfig.FieldState{Field: sysconfig.Field{Key: key}, Value: "6h", Source: "stored"}}
		if sysCanReset(row) {
			t.Errorf("%s: a schedule interval must not offer a reset", key)
		}
	}
	// the rule is about the schedules only: another stored duration keeps its reset
	other := sysRow{FieldState: sysconfig.FieldState{Field: sysconfig.Field{Key: "BACKUP_MAX_AGE"}, Value: "48h", Source: "stored"}}
	if !sysCanReset(other) {
		t.Error("BACKUP_MAX_AGE is stored and differs: it keeps its reset")
	}
}

// Why (#623): after the click the page shows the default that Save will store. The script can only
// do that when the server says what the default is, so the reset control carries it, taken from the
// one place that defines it (config), never typed twice.
func TestResetControlCarriesTheDefault(t *testing.T) {
	e, _, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = &fakeFleet{} })
	e.do(t, "POST", "/devices/backups", "admin", url.Values{"csrf": {"tok"},
		"swm.BACKUP_INTERVAL": {"1"}, "sw.BACKUP_INTERVAL": {"1"}, "v.BACKUP_INTERVAL": {"6h"},
		"v.BACKUP_KEEP": {"4"}, "v.BACKUP_MAX_AGE": {"48h"}})
	_, page := e.do(t, "GET", "/devices/backups", "admin", nil)
	for _, key := range []string{"BACKUP_KEEP", "BACKUP_MAX_AGE"} {
		f := fieldHTML(t, page, key)
		want := `name="clear.` + key + `" value="1" data-default="` + config.DefaultValue(key) + `"`
		if !strings.Contains(f, want) {
			t.Errorf("%s: reset control lacks its default, want %s in %s", key, want, f)
		}
	}
	if strings.Contains(page, `name="clear.BACKUP_INTERVAL"`) {
		t.Error("the interval of a schedule that is on must not offer a reset")
	}
}

// Why (#623): the page must also work without JavaScript: the form posts clear.KEY, and the server
// stores the default, so what a script showed after the click is what the page shows after Save.
func TestResetOnSaveStoresTheDefault(t *testing.T) {
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = &fakeFleet{} })
	e.do(t, "POST", "/devices/backups", "admin", url.Values{"csrf": {"tok"},
		"swm.BACKUP_INTERVAL": {"1"}, "sw.BACKUP_INTERVAL": {"1"}, "v.BACKUP_INTERVAL": {"6h"},
		"v.BACKUP_KEEP": {"4"}, "v.BACKUP_MAX_AGE": {"48h"}})
	r, _ := e.do(t, "POST", "/devices/backups", "admin", url.Values{"csrf": {"tok"},
		"swm.BACKUP_INTERVAL": {"1"}, "sw.BACKUP_INTERVAL": {"1"}, "v.BACKUP_INTERVAL": {"6h"},
		"v.BACKUP_KEEP": {"4"}, "v.BACKUP_MAX_AGE": {"48h"}, "clear.BACKUP_KEEP": {"1"}, "clear.BACKUP_MAX_AGE": {"1"}})
	if r.StatusCode != 303 {
		t.Fatalf("save: %d", r.StatusCode)
	}
	s := svc.Snapshot()
	if s.BackupKeep != 10 || s.BackupMaxAge != 720*time.Hour {
		t.Errorf("keep=%d maxage=%v, want the defaults 10 and 720h", s.BackupKeep, s.BackupMaxAge)
	}
	if s.BackupSchedule != 6*time.Hour {
		t.Errorf("resetting other fields must leave the schedule on: %v", s.BackupSchedule)
	}
}
