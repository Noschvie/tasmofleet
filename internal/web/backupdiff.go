package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// LiveSnapshotter reads the Status snapshot of a device without storing it
// (*backup.Manager implements it). Deps.Backups is asserted against it, so
// the backup.Service contract stays unchanged; without it the drift page
// answers 404.
type LiveSnapshotter interface {
	LiveSnapshot(ctx context.Context, id model.DeviceID) ([]byte, error)
}

// driftMinInterval spares the single-threaded device web server: the drift
// page asks 17 status commands, so repeated reloads must not hammer it. A
// reload inside the interval shows the snapshot read last ("read 5s ago").
const driftMinInterval = 15 * time.Second

type driftKey struct {
	h  *handler
	id model.DeviceID
}

// driftRead is the last live snapshot of a device and when it was read; doc is
// nil while that read runs or when it failed.
type driftRead struct {
	at  time.Time
	doc []byte
}

var (
	driftMu   sync.Mutex
	driftLast = map[driftKey]driftRead{}
)

// reserveDrift is the check and the reservation in one critical section, so
// parallel requests cannot all pass the throttle before the first one records.
// Inside the interval it returns the last read instead (fresh false).
func reserveDrift(k driftKey, now time.Time) (last driftRead, fresh bool) {
	driftMu.Lock()
	defer driftMu.Unlock()
	if last, ok := driftLast[k]; ok && now.Sub(last.at) < driftMinInterval {
		return last, false
	}
	driftLast[k] = driftRead{at: now}
	return driftRead{}, true
}

// keepDrift records a successful read for the reloads of the interval.
func keepDrift(k driftKey, read driftRead) {
	driftMu.Lock()
	defer driftMu.Unlock()
	driftLast[k] = read
}

// diffRow is one line of a diff table. Values were scrubbed by package
// backup; the template escapes them.
type diffRow struct {
	Path, Old, New       string
	Added, Removed       bool
	Secret, Hidden, Body bool
}

func toRows(cs []backup.Change) []diffRow {
	rows := make([]diffRow, 0, len(cs))
	for _, c := range cs {
		rows = append(rows, diffRow{
			Path: c.Path, Old: c.Old, New: c.New, Secret: c.Secret, Hidden: c.Hidden, Body: c.Body,
			Added: c.Old == "" && !c.Secret && !c.Hidden, Removed: c.New == "" && !c.Secret && !c.Hidden,
		})
	}
	return rows
}

func (h *handler) registerBackupDiff(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op func(http.Handler) http.Handler) {
	route("GET /devices/{id}/backups", op, h.backupsPage)
	route("GET /devices/{id}/backups/diff", op, h.backupDiff)
	route("GET /devices/{id}/drift", op, h.drift)
}

// driftView is the drift tab: the live settings against the newest backup's snapshot.
type driftView struct {
	Tabs          deviceTabs
	Base, CSRF    string
	Self          string // this page, for "Try again"
	Notice, Error string
	Retry         bool // offer "Try again" (the device was busy or did not answer)
	Compared      bool
	ReadAgo       string // when the live settings were read: "now", "5s ago"
	StoredAt      time.Time
	StoredBy      string // what started that backup ("Automatic, before update to 14.3.0"), "" when unknown
	Changed       int
	Total         int // settings compared
	All           bool
	AllHref       string // the same page with "Show all" toggled
	Rows          []driftRow
	Hiding        bool // free-text bodies are hidden for this role
	CanBackup     bool
	RestoreHref   string // the newest config dump's restore confirmation (admins)
}

type driftRow struct {
	Path, Old, New string
	Changed        bool
}

// storedBy is " · " and what started the compared backup, or "".
func (p driftView) storedBy() string {
	if p.StoredBy == "" {
		return ""
	}
	return " · " + p.StoredBy
}

// countText is the line above the table.
func (p driftView) countText() string {
	if p.Changed == 0 {
		return "No setting changed since the last backup"
	}
	if p.Changed == 1 {
		return "1 setting changed since the last backup"
	}
	return strconv.Itoa(p.Changed) + " settings changed since the last backup"
}

// driftValue is a compared value as text: a JSON string without its quotes, the redacted kinds
// in words.
func driftValue(c backup.Change, v string, other string) string {
	switch {
	case c.Secret && c.Same:
		return "secret (not shown)"
	case c.Secret:
		return "secret value changed (not shown)"
	case c.Hidden:
		return "text changed (visible to admins only)"
	case v == "" && other != "":
		return "(absent)"
	}
	var s string
	if strings.HasPrefix(v, `"`) && json.Unmarshal([]byte(v), &s) == nil {
		return s
	}
	return v
}

func driftRows(cs []backup.Change) (rows []driftRow, changed int) {
	for _, c := range cs {
		if !c.Same {
			changed++
		}
		rows = append(rows, driftRow{Path: c.Path, Old: driftValue(c, c.Old, c.New), New: driftValue(c, c.New, c.Old), Changed: !c.Same})
	}
	return rows, changed
}

func (h *handler) drift(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	live, ok := h.d.Backups.(LiveSnapshotter)
	if !ok {
		h.notFound(w, r)
		return
	}
	v := h.view(r, d.Name+" drift", "devices", nil)
	self := v.Base + devPath(d.ID, "/drift")
	all := r.URL.Query().Get("all") == "1"
	p := driftView{Base: v.Base, CSRF: v.CSRF, Self: self, Hiding: !v.IsAdmin, All: all, AllHref: self + "?all=1"}
	if all {
		p.AllHref = self
	}
	show := func(status int) {
		p.Tabs = h.tabsOf(r, v, d, "drift")
		p.CanBackup = p.Tabs.Reason == ""
		h.headPage(w, r, status, "drift", v, d, driftPage(p))
	}
	list, err := h.d.Backups.List(r.Context(), d.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var stored *model.Backup
	for i := range list {
		if list[i].Kind == model.BackupSnapshot && stored == nil { // newest first
			stored = &list[i]
		}
		if list[i].Kind == model.BackupConfigDump && p.RestoreHref == "" && v.IsAdmin && h.d.Restore != nil {
			p.RestoreHref = v.Base + "/backups/" + strconv.FormatInt(int64(list[i].ID), 10) + "/restore"
		}
	}
	if stored == nil {
		p.Notice = "There is no backup of this device yet."
		show(http.StatusOK)
		return
	}
	p.StoredAt, p.StoredBy = stored.TakenAt, backupTriggerText(stored.Trigger, stored.TriggerRef)
	now := h.d.Clock.Now()
	key := driftKey{h, d.ID}
	read, fresh := reserveDrift(key, now)
	if !fresh && read.doc == nil {
		p.Notice = "The device was queried a moment ago. Try again in a few seconds."
		p.Retry = true
		show(http.StatusTooManyRequests)
		return
	}
	old, _, err := h.d.Backups.Open(r.Context(), stored.ID)
	if errors.Is(err, backup.ErrFileMissing) {
		h.backupFileMissing(w, r, *stored)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if fresh {
		cur, err := live.LiveSnapshot(r.Context(), d.ID)
		if err != nil {
			p.Error = "The device could not be read: " + errText(err)
			p.Retry = true
			show(http.StatusBadGateway)
			return
		}
		read = driftRead{at: now, doc: cur}
		keepDrift(key, read)
	}
	changes, err := backup.SnapshotCompare(old, read.doc)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !v.IsAdmin {
		changes = backup.HideBodies(changes)
	}
	p.Compared, p.Total, p.ReadAgo = true, len(changes), agoWords(now, read.at)
	rows, changed := driftRows(changes)
	p.Changed = changed
	for _, row := range rows {
		if all || row.Changed {
			p.Rows = append(p.Rows, row)
		}
	}
	show(http.StatusOK)
}
