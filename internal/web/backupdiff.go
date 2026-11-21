package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
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
	Total         int    // settings compared
	Unread        string // the sections one side could not read ("Status 9"), "" when every section was read
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

// headline is the sentence above the table: what differs from the last backup, and when that backup is from.
func (p driftView) headline() string {
	switch {
	case p.Changed == 0 && p.Unread != "":
		return "No difference in the settings that could be compared"
	case p.Changed == 0:
		return "Everything matches the last backup"
	}
	if p.Changed == 1 {
		return "1 setting differs from the last backup"
	}
	return strconv.Itoa(p.Changed) + " settings differ from the last backup"
}

// allText is the link that toggles the unchanged settings.
func (p driftView) allText() string {
	if p.All {
		return "Show changed only"
	}
	return "Show all " + plural(p.Total, "setting")
}

// unreadText is the banner for sections that one side could not read.
func (p driftView) unreadText() string {
	return "Part of the device could not be read (" + p.Unread + "). Those settings are not compared."
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

// readErrorKey is the key under which a snapshot records a section it could not read ({"_error": "..."}, written
// by package backup).
const readErrorKey = "._error"

// comparedSettings is what a snapshot comparison may show: the changes of the sections both sides read.
type comparedSettings struct {
	Changes  []backup.Change
	Unread   []string // sections one side could not read and the other could, as the page names them ("Status 9")
	NoneRead bool     // the live read answered no section at all
}

// readableSections drops every section that one side could not read from a snapshot comparison. Such a section
// is no setting that changed: its failure marker (_error) and the settings of the other side would show as
// "changed" or "absent". It decides by structure, never by the error text, which differs by transport. A
// section both sides could not read is dropped without a word: the device never answers it (Status 9 on a
// device without energy monitoring), so nothing is missing. A section one side could not read is dropped and
// named, so the page does not claim a comparison it did not make.
func readableSections(cs []backup.Change) comparedSettings {
	type sides struct{ stored, live bool }
	unread := map[string]sides{}
	for _, c := range cs {
		sec, ok := strings.CutSuffix(c.Path, readErrorKey)
		if !ok || strings.ContainsAny(sec, ".[") {
			continue
		}
		s := unread[sec]
		switch {
		case c.Same:
			s = sides{true, true}
		case c.Secret: // a marker the redaction rule took for a credential says no side: report it as one-sided
			s.stored = true
		default:
			s.stored = s.stored || c.Old != ""
			s.live = s.live || c.New != ""
		}
		unread[sec] = s
	}
	var out comparedSettings
	for _, c := range cs { // a section is dropped whole: its marker, and the settings the other side holds
		dropped := false
		for sec := range unread {
			if c.Path == sec || strings.HasPrefix(c.Path, sec+".") || strings.HasPrefix(c.Path, sec+"[") {
				dropped = true
				break
			}
		}
		if !dropped {
			out.Changes = append(out.Changes, c)
		}
	}
	liveUnread := false
	for sec, s := range unread {
		liveUnread = liveUnread || s.live
		if s.stored != s.live {
			out.Unread = append(out.Unread, sec)
		}
	}
	sort.Slice(out.Unread, func(i, j int) bool { // Status5 before Status10
		a, b := out.Unread[i], out.Unread[j]
		return len(a) < len(b) || (len(a) == len(b) && a < b)
	})
	out.NoneRead = len(out.Changes) == 0 && liveUnread
	return out
}

// sectionName is how the page names a snapshot section: "Status9" is "Status 9".
func sectionName(sec string) string {
	if n, ok := strings.CutPrefix(sec, "Status"); ok && n != "" && strings.Trim(n, "0123456789") == "" {
		return "Status " + n
	}
	return sec
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
	v := h.view(r, d.Name+" drift", "device", nil)
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
	// A device whose HTTP API is off and which nothing else reads is not asked: the read would only fail (#645).
	if why := h.apiOffBlock(r.Context(), d.ID, transport.ActCommands); why != "" {
		p.Error = why
		show(http.StatusConflict)
		return
	}
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
		var limited *backup.LimitedFirmwareError
		if errors.As(err, &limited) { // safeboot or minimal firmware: not a failed read, and a retry would change nothing (#558)
			p.Error = "The device runs " + limited.Variant + " firmware, which does not know the status commands the comparison reads. TasmoFleet checks it again about once an hour."
			show(http.StatusOK)
			return
		}
		if err != nil {
			h.logReadFailure(r, d.ID, err)
			p.Error = readErrorText(err) // the class sentence: the Go error chain never reaches the page
			p.Retry = true
			show(http.StatusBadGateway)
			return
		}
		read = driftRead{at: now, doc: cur}
	}
	changes, err := backup.SnapshotCompare(old, read.doc)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	cmp := readableSections(changes)
	if fresh && !cmp.NoneRead { // a read that answered nothing is a failed read: the reloads of the interval do not show it
		keepDrift(key, read)
	}
	if cmp.NoneRead {
		p.Error = "The device could not be read."
		p.Retry = true
		show(http.StatusBadGateway)
		return
	}
	changes = cmp.Changes
	if !v.IsAdmin {
		changes = backup.HideBodies(changes)
	}
	names := make([]string, len(cmp.Unread))
	for i, sec := range cmp.Unread {
		names[i] = sectionName(sec)
	}
	p.Unread = strings.Join(names, ", ")
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
