package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The backup comparison (design v2 screen "compare", WP P10): two backups of one device, the
// settings that differ, or with "Show all" every setting; side by side or unified. Secrets are
// never shown, a changed secret only appears as a row.

// compareView is the comparison page.
type compareView struct {
	Base, Action string // the form's GET address
	Sub          string // "Living Room Ceiling Light · 4 of 17 settings differ"
	Options      []compareOption
	A, B         model.BackupID // the decoded copies compared (older, newer)
	All, Unified bool
	NAll         int
	Diff         ui.DiffData
	Identical    bool   // no difference and not showing all
	Hiding       bool   // free-text bodies are hidden for this role
	RestoreHref  string // restore the older backup; empty when the viewer may not
	BackHref     string
}

// compareOption is one backup in the Older and Newer selects.
type compareOption struct {
	ID    model.BackupID
	Label string // "15 Oct 2026, 03:00 · Scheduled"
}

// triggerShort is the trigger in the comparison's selects: "Scheduled", "Before update".
func triggerShort(t model.BackupTrigger) string {
	switch t {
	case model.TriggerManual:
		return "Manual"
	case model.TriggerScheduled:
		return "Scheduled"
	case model.TriggerUpdate:
		return "Before update"
	case model.TriggerRestore:
		return "Before restore"
	case model.TriggerReplace:
		return "During replace"
	case model.TriggerBulk:
		return "Bulk backup"
	}
	return ""
}

// pairState says whether comparePair found two backups to compare.
type pairState int

const (
	pairOK        pairState = iota
	pairNotFound            // an id that is not one of this device's backups (or malformed)
	pairUndecoded           // the backups exist but one has no decoded copy (no decoder), or fewer than two have
)

// comparePair picks the two decoded copies to compare: a and b (decoded ids), or two selected
// rows (id, any file of a run), or the two newest.
func comparePair(list []model.Backup, dev model.DeviceID, q url.Values) (older, newer model.Backup, st pairState) {
	decoded := map[time.Time]model.Backup{}
	var newest []model.Backup // decoded, newest first (List order)
	byID := map[model.BackupID]model.Backup{}
	for _, b := range list {
		if b.DeviceID != dev {
			continue // never compare another device's backup through this page
		}
		byID[b.ID] = b
		if b.Kind == model.BackupDecoded {
			decoded[b.TakenAt] = b
			newest = append(newest, b)
		}
	}
	pick := func(s string, wantDecoded bool) (model.Backup, pairState) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			return model.Backup{}, pairNotFound
		}
		b, found := byID[model.BackupID(n)]
		if !found || (wantDecoded && b.Kind != model.BackupDecoded) {
			return model.Backup{}, pairNotFound
		}
		d, found := decoded[b.TakenAt]
		if !found {
			return model.Backup{}, pairUndecoded
		}
		return d, pairOK
	}
	worst := func(a, b pairState) pairState {
		if a == pairNotFound || b == pairNotFound {
			return pairNotFound
		}
		if a == pairUndecoded || b == pairUndecoded {
			return pairUndecoded
		}
		return pairOK
	}
	var x, y model.Backup
	switch ids := q["id"]; {
	case q.Get("a") != "" || q.Get("b") != "":
		var sa, sb pairState
		x, sa = pick(q.Get("a"), true)
		y, sb = pick(q.Get("b"), true)
		if st := worst(sa, sb); st != pairOK {
			return x, y, st
		}
	case len(ids) > 0:
		if len(ids) != 2 {
			return x, y, pairNotFound
		}
		var sa, sb pairState
		x, sa = pick(ids[0], false)
		y, sb = pick(ids[1], false)
		if st := worst(sa, sb); st != pairOK {
			return x, y, st
		}
	default:
		if len(newest) < 2 {
			return x, y, pairUndecoded
		}
		x, y = newest[1], newest[0]
	}
	if y.TakenAt.Before(x.TakenAt) {
		x, y = y, x
	}
	return x, y, pairOK
}

// diffRows turns the comparison into the component's rows; values that may not be shown are
// replaced by a word.
func diffRows(cs []backup.Change) []ui.DiffRow {
	rows := make([]ui.DiffRow, 0, len(cs))
	for _, c := range cs {
		r := ui.DiffRow{Key: c.Path, Old: c.Old, New: c.New, Changed: !c.Same}
		switch {
		case c.Secret && c.Same:
			r.Old, r.New = "secret (not shown)", "secret (not shown)"
		case c.Secret:
			r.Old, r.New = "secret (not shown)", "secret value changed (not shown)"
		case c.Hidden && c.Same:
			r.Old, r.New = "text (visible to admins only)", "text (visible to admins only)"
		case c.Hidden:
			r.Old, r.New = "text (visible to admins only)", "text changed (visible to admins only)"
		default:
			if r.Old == "" {
				r.Old = "(absent)"
			}
			if r.New == "" {
				r.New = "(removed)"
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func (h *handler) backupDiff(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	list, err := h.d.Backups.List(r.Context(), d.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	a, b, st := comparePair(list, d.ID, q)
	switch st {
	case pairNotFound:
		h.notFound(w, r)
		return
	case pairUndecoded:
		v := h.view(r, "Compare backups", "devices", nil)
		v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: d.Name, Path: fmt.Sprintf("/devices/%d", d.ID)}, {Label: "Compare backups"}}
		h.show(w, r, http.StatusOK, "compare", v, compareUndecoded(d.Name, fmt.Sprintf("%s/devices/%d/backups", v.Base, d.ID), v.Base))
		return
	case pairOK:
	}
	da, _, err := h.d.Backups.Open(r.Context(), a.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	db, _, err := h.d.Backups.Open(r.Context(), b.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	all, err := backup.Compare(da, db)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := h.view(r, "Compare backups", "devices", nil)
	if !v.IsAdmin {
		all = backup.HideBodies(all)
	}
	p := compareView{Base: v.Base, Action: fmt.Sprintf("%s/devices/%d/backups/diff", v.Base, d.ID), A: a.ID, B: b.ID,
		All: q.Get("all") == "1", Unified: q.Get("view") == "unified", NAll: len(all), Hiding: !v.IsAdmin,
		BackHref: fmt.Sprintf("%s/devices/%d/backups", v.Base, d.ID)}
	var changed []backup.Change
	for _, c := range all {
		if !c.Same {
			changed = append(changed, c)
		}
	}
	p.Sub = fmt.Sprintf("%s · %d of %s differ", d.Name, len(changed), plural(len(all), "setting"))
	shown := changed
	if p.All {
		shown = all
	}
	p.Identical = len(shown) == 0
	p.Diff = ui.DiffData{OldLabel: shortDate(a.TakenAt), NewLabel: shortDate(b.TakenAt), Unified: p.Unified, Rows: diffRows(shown)}
	for _, x := range list {
		if x.Kind != model.BackupDecoded {
			continue
		}
		label := shortDate(x.TakenAt)
		if t := triggerShort(x.Trigger); t != "" {
			label += " · " + t
		}
		p.Options = append(p.Options, compareOption{ID: x.ID, Label: label})
		if v.IsAdmin && h.d.Restore != nil {
			for _, f := range list {
				if f.Kind == model.BackupConfigDump && f.TakenAt.Equal(a.TakenAt) && x.ID == a.ID {
					p.RestoreHref = fmt.Sprintf("%s/backups/%d/restore", v.Base, f.ID)
				}
			}
		}
	}
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: d.Name, Path: fmt.Sprintf("/devices/%d", d.ID)}, {Label: "Compare backups"}}
	h.show(w, r, http.StatusOK, "compare", v, comparePage(p))
}
