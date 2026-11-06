package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
	"github.com/hellwand/tasmofleet/internal/sysconfig"
)

// Why (#623, #602): the Reset control is "reset to default on save". The default of the days of a
// schedule is no day, which unticking the chips already does; offering a Reset next to chips that
// still show days contradicts them. Neither the backup nor the snapshot days offer one at all,
// whatever is stored.
func TestScheduleDaysOfferNoReset(t *testing.T) {
	for _, key := range []string{"BACKUP_DAYS", "SNAPSHOT_DAYS"} {
		row := sysRow{FieldState: sysconfig.FieldState{Field: sysconfig.Field{Key: key}, Value: "mon,wed", Source: "stored"}}
		if sysCanReset(row) {
			t.Errorf("%s: the days of a schedule must not offer a reset", key)
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
		"m.BACKUP_DAYS": {"1"}, "v.BACKUP_DAYS": {"mon", "wed"}, "v.BACKUP_TIME": {"04:30"},
		"v.BACKUP_KEEP": {"4"}, "v.BACKUP_MAX_AGE": {"48h"}})
	_, page := e.do(t, "GET", "/devices/backups", "admin", nil)
	for _, key := range []string{"BACKUP_KEEP", "BACKUP_MAX_AGE"} {
		f := fieldHTML(t, page, key)
		want := `name="clear.` + key + `" value="1" data-default="` + config.DefaultValue(key) + `"`
		if !strings.Contains(f, want) {
			t.Errorf("%s: reset control lacks its default, want %s in %s", key, want, f)
		}
	}
	if strings.Contains(page, `name="clear.BACKUP_DAYS"`) {
		t.Error("the backup days must not offer a reset: unticking them is the reset")
	}
	if f := fieldHTML(t, page, "BACKUP_TIME"); !strings.Contains(f, `name="clear.BACKUP_TIME" value="1" data-default="03:00"`) {
		t.Errorf("a stored time that differs from 03:00 offers its reset with the default: %s", f)
	}
}

// Why (#623): the page must also work without JavaScript: the form posts clear.KEY, and the server
// stores the default, so what a script showed after the click is what the page shows after Save.
func TestResetOnSaveStoresTheDefault(t *testing.T) {
	e, svc, _, _ := sysEnvFull(t, true, func(d *Deps) { d.BackupFleet = &fakeFleet{} })
	e.do(t, "POST", "/devices/backups", "admin", url.Values{"csrf": {"tok"},
		"m.BACKUP_DAYS": {"1"}, "v.BACKUP_DAYS": {"mon", "wed"}, "v.BACKUP_TIME": {"04:30"},
		"v.BACKUP_KEEP": {"4"}, "v.BACKUP_MAX_AGE": {"48h"}})
	r, _ := e.do(t, "POST", "/devices/backups", "admin", url.Values{"csrf": {"tok"},
		"m.BACKUP_DAYS": {"1"}, "v.BACKUP_DAYS": {"mon", "wed"}, "v.BACKUP_TIME": {"04:30"},
		"v.BACKUP_KEEP": {"4"}, "v.BACKUP_MAX_AGE": {"48h"}, "clear.BACKUP_KEEP": {"1"}, "clear.BACKUP_MAX_AGE": {"1"}})
	if r.StatusCode != 303 {
		t.Fatalf("save: %d", r.StatusCode)
	}
	s := svc.Snapshot()
	if s.BackupKeep != 10 || s.BackupMaxAge != 720*time.Hour {
		t.Errorf("keep=%d maxage=%v, want the defaults 10 and 720h", s.BackupKeep, s.BackupMaxAge)
	}
	if sc := s.BackupSchedule(); sc.Days.String() != "mon,wed" || sc.Clock() != "04:30" {
		t.Errorf("resetting other fields must leave the schedule as it was: %+v", sc)
	}
}

// Why (#623): the field shows after the click what it shows after Save. TELEPERIOD's default is written
// 300s in the code but the page writes the value as 5m; showing 300s and then 5m (and offering a Reset
// for a stored 5m that Save cannot change) is the mismatch the issue is about.
func TestResetDefaultIsWrittenAsThePageWritesIt(t *testing.T) {
	row := sysRow{FieldState: sysconfig.FieldState{Field: sysconfig.Field{Key: "TELEPERIOD"}, Value: "5m", Source: "stored"}}
	if got := sysDefault(row); got != "5m" {
		t.Errorf("TELEPERIOD default shown as %q, want 5m (the page's own spelling)", got)
	}
	if sysCanReset(row) {
		t.Error("a stored value equal to the default has nothing to reset")
	}
	e, _, _, _ := sysEnv(t, true)
	e.do(t, "POST", "/settings/system?tab=network", "admin", url.Values{"csrf": {"tok"}, "v.TELEPERIOD": {"60s"}})
	_, page := e.do(t, "GET", "/settings/system?tab=network", "admin", nil)
	if f := fieldHTML(t, page, "TELEPERIOD"); !strings.Contains(f, `data-default="5m"`) {
		t.Errorf("reset control of TELEPERIOD: want data-default=\"5m\" in %s", f)
	}
}
