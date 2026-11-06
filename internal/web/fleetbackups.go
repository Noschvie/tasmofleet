package web

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/restore"
	"github.com/hellwand/tasmofleet/internal/schedule"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// BackupFleet is what the Devices, Backups page (#409) reads: the last scheduled run, one row per
// device, and what the next prune removes. Optional: without it the page and the dashboard hint
// say nothing about runs.
type BackupFleet interface {
	// LastRun is the last finished scheduled run; ok is false when there is none yet.
	LastRun(ctx context.Context) (run model.BackupRun, ok bool, err error)
	// Overview has one row per inventory device and does no device I/O: it reads the database and,
	// for the files columns, the manifest of each device's newest archive on disk. Files is the number
	// of stored files in that archive; FilesNote says why the newest run has none ("settings only: no
	// file system", "files need HTTP") and is empty otherwise.
	Overview(ctx context.Context) ([]model.BackupDeviceRow, error)
	// PruneNext lists the backup runs the next backup would delete.
	PruneNext(ctx context.Context) ([]model.Backup, error)
	// LastMissed is when a scheduled run was last skipped because TasmoFleet was down at its time
	// (#601); zero when that never happened.
	LastMissed(ctx context.Context) (time.Time, error)
}

// whyText is why a device was skipped, failed or left out: the reason as a sentence, and when the
// device went offline as an instant the page words like every other time.
type whyText struct {
	Reason    string
	Since     time.Time
	SinceText string // the fallback text of Since
	SinceFull string // the fallback title of Since; time.js shows the full date in the reader's zone
}

// Plain is the reason alone, for the title of a cell that cuts it off; the time inside has its own
// title that time.js writes in the reader's zone.
func (w whyText) Plain() string { return w.Reason }

func newWhy(now time.Time, reason string, since time.Time) whyText {
	w := whyText{Reason: fleetSentence(reason), Since: since}
	if !since.IsZero() {
		w.SinceText, w.SinceFull = ui.StampDayText(now, since), ui.StampText(since, ui.StampTitle)
	}
	return w
}

// fleetPath is Devices -> Backups (#409). The schedule form on it posts to the same path.
const fleetPath = "/devices/backups"

// onBackups says whether the request is for the Backups page or something below it, so the shared
// settings handlers answer with that page.
func onBackups(r *http.Request) bool {
	return r.URL.Path == fleetPath || strings.HasPrefix(r.URL.Path, fleetPath+"/")
}

// The filter chips of the device table. The keys are what ?filter= takes.
const (
	fleetAll     = ""
	fleetNone    = "none"
	fleetOld     = "old"
	fleetFailed  = "failed"
	fleetSkipped = "skipped"
)

// fleetMinOld is the least age that makes a backup "older than": max(this, 2 x the longest gap between runs).
const fleetMinOld = 7 * 24 * time.Hour

type fleetChip struct {
	Key, Label, Href string
	N                int
	Current          bool
}

// fleetRow is one device of the table.
type fleetRow struct {
	ID                   model.DeviceID
	Name, BackupsHref    string
	DeviceHref           string
	ReplaceHref          string // empty for a role that may not replace, or without the restore service
	Newest               time.Time
	Ago                  string
	Runs                 int
	RunsTitle            string
	Files                string // "12 files", or ""
	FilesBadge, FilesWhy string // "Settings only" with its full sentence; "" without
	Outcome              model.BackupOutcome
	Reason               string
	Why                  whyText
	NotInRun             bool // the last run does not name this device (added after it)
	Skip                 bool // a run started now would leave it out
	sortResult           int
}

// fleetPrune is one device in "Pruned at the next run".
type fleetPrune struct {
	Name, Href string
	Runs       int
	Oldest     time.Time
	Size       string
}

// fleetView is everything the Backups page shows beyond the settings form.
type fleetView struct {
	Base        string
	Admin       bool
	Now         time.Time
	Run         *model.BackupRun
	Scheduled   bool      // a backup schedule is set (BACKUP_DAYS has a weekday)
	Zone        string    // the server's time zone as the clock shows it ("CET"), for the note that times are on its clock
	Plan        string    // the schedule in words, "Mon, Wed, Fri at 03:00"; empty when off
	Next        time.Time // the next scheduled run; zero when off
	Missed      time.Time // when the newest skipped run was noticed, when that is newer than the last run; else zero
	Failed      []model.BackupRunItem
	Skipped     []model.BackupRunItem
	Chips       []fleetChip
	Rows        []fleetRow
	Total       int // devices, whatever the filter
	Filter      string
	Sort, Dir   string
	AllIDs      []model.DeviceID // the devices a run started now would try
	SkippedIDs  []model.DeviceID // the ones it would leave out
	SkippedNow  int
	OldDays     int
	Runs, Kept  int
	Size        string
	Without     int
	WithoutHref string
	Keep        string // BACKUP_KEEP as shown, for the prune note
	PruneRuns   int
	Prune       []fleetPrune
	PruneMore   int
}

// fleetSchedule is the effective backup schedule (BACKUP_DAYS, BACKUP_TIME); no weekday is "manual only".
func (h *handler) fleetSchedule() schedule.Schedule {
	return h.scheduleOf("BACKUP_DAYS", "BACKUP_TIME")
}

// scheduleOf is the effective schedule of a pair of settings (weekdays, time of day); no weekday is off.
func (h *handler) scheduleOf(daysKey, timeKey string) schedule.Schedule {
	if h.d.SysConfig == nil {
		return schedule.Schedule{}
	}
	var days, clock string
	for _, st := range h.d.SysConfig.State() {
		switch st.Key {
		case daysKey:
			days = st.Value
		case timeKey:
			clock = st.Value
		}
	}
	s, err := schedule.New(days, clock)
	if err != nil {
		return schedule.Schedule{} // validation keeps this from being stored
	}
	return s
}

var dayFull = map[string]string{"mon": "Monday", "tue": "Tuesday", "wed": "Wednesday", "thu": "Thursday", "fri": "Friday", "sat": "Saturday", "sun": "Sunday"}

// dayLabels are the short weekday names; the chips show them.
var dayLabels = map[string]string{"mon": "Mon", "tue": "Tue", "wed": "Wed", "thu": "Thu", "fri": "Fri", "sat": "Sat", "sun": "Sun"}

// schedulePlan words a schedule: "every day at 03:00", "Mon, Wed, Fri at 03:00"; empty when off.
func schedulePlan(s schedule.Schedule) string {
	if s.Off() {
		return ""
	}
	days := strings.Split(s.Days.String(), ",")
	if len(days) == 7 {
		return "every day at " + s.Clock()
	}
	for i, d := range days {
		days[i] = dayLabels[d]
	}
	return strings.Join(days, ", ") + " at " + s.Clock()
}

// backupsRender draws Devices -> Backups: only the Backups part of the settings layout, with the
// fleet read now. An operator sees the same page without anything that starts or changes a run.
func (h *handler) backupsRender(w http.ResponseWriter, r *http.Request, status int, p sysPage) {
	p.Tabs = slices.DeleteFunc(p.Tabs, func(t sysTab) bool { return t.Slug != sysBackupsSlug })
	v := h.view(r, "Backups", "backups", nil)
	v.Flash = backupsNotice(r)
	if r.URL.Query().Get("ok") == "saved" {
		v.Flash = "Saved."
	}
	if p.Storage == nil {
		st, err := h.sysStorageOf(r, p.Base)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		p.Storage = st
	}
	fv, err := h.fleetOf(r, v.IsAdmin)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if st := p.Storage; st != nil {
		fv.Runs, fv.Kept, fv.Size = st.Runs, st.Kept, st.Size // the totals include the backups of deleted devices
		if !v.IsAdmin {                                       // an operator may look and keep, never delete
			st.CanDelete = false
			for i := range st.Orphans {
				st.Orphans[i].DeleteText, st.Orphans[i].Download = "", "" // both routes are admin-only
			}
		}
	}
	p.Fleet = fv
	h.show(w, r, status, "backups", v, fleetBackupsPage(p))
}

func (h *handler) fleetBackupsPage(w http.ResponseWriter, r *http.Request) {
	h.sysRender(w, r, http.StatusOK, h.sysData(r, nil, nil))
}

func (h *handler) registerFleetBackups(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op, adm func(http.Handler) http.Handler) {
	if h.d.BackupFleet == nil || h.d.SysConfig == nil {
		return
	}
	route("GET "+fleetPath, op, h.fleetBackupsPage)
	route("POST "+fleetPath, adm, h.sysSave)
	route("POST "+fleetPath+"/stepup", adm, h.sysStepUpPost)
}

// fleetOf reads the last run and the per-device overview and arranges them for the page.
func (h *handler) fleetOf(r *http.Request, admin bool) (*fleetView, error) {
	ctx := r.Context()
	base := httpBase(r)
	fv := &fleetView{Base: base, Admin: admin, Now: h.d.Clock.Now(), Filter: r.URL.Query().Get("filter"), WithoutHref: base + fleetPath + "?filter=" + fleetNone}
	if !slices.Contains([]string{fleetNone, fleetOld, fleetFailed, fleetSkipped}, fv.Filter) {
		fv.Filter = fleetAll
	}
	sched := h.fleetSchedule()
	fv.Scheduled = !sched.Off()
	fv.Plan, fv.Next = schedulePlan(sched), sched.Next(fv.Now)
	fv.Zone, _ = fv.Now.Zone()
	oldAfter := max(fleetMinOld, 2*sched.LongestGap())
	fv.OldDays = int(oldAfter.Hours() / 24)
	for _, st := range h.d.SysConfig.State() {
		if st.Key == "BACKUP_KEEP" {
			fv.Keep = st.Value
		}
	}
	run, ok, err := h.d.BackupFleet.LastRun(ctx)
	if err != nil {
		return nil, err
	}
	if ok {
		fv.Run = &run
		for _, it := range run.Items {
			switch it.Outcome {
			case model.BackupOutcomeFailed:
				fv.Failed = append(fv.Failed, it)
			case model.BackupOutcomeSkipped:
				fv.Skipped = append(fv.Skipped, it)
			case model.BackupOutcomeOK:
			}
		}
	}
	missed, err := h.d.BackupFleet.LastMissed(ctx)
	if err != nil {
		return nil, err
	}
	if fv.Scheduled && !missed.IsZero() && (fv.Run == nil || missed.After(fv.Run.StartedAt)) {
		fv.Missed = missed
	}
	over, err := h.d.BackupFleet.Overview(ctx)
	if err != nil {
		return nil, err
	}
	_, canReplace := h.d.Restore.(restore.Replacer)
	var all []fleetRow
	var bytes int64
	for _, o := range over {
		row := fleetRow{ID: o.DeviceID, Name: o.Name, BackupsHref: fmt.Sprintf("%s/devices/%d/backups", base, o.DeviceID),
			DeviceHref: fmt.Sprintf("%s/devices/%d", base, o.DeviceID), Newest: o.NewestAt, Runs: o.Runs,
			Outcome: o.LastOutcome, Reason: o.LastReason, Why: newWhy(fv.Now, o.LastReason, o.LastSince), Skip: o.SkipReason != "", NotInRun: ok && o.LastOutcome == ""}
		if admin && canReplace {
			row.ReplaceHref = fmt.Sprintf("%s/devices/%d/replace", base, o.DeviceID)
		}
		if !o.NewestAt.IsZero() {
			row.Ago = agoWords(fv.Now, o.NewestAt)
		}
		row.RunsTitle = plural(o.Kept, "kept backup") + ", never pruned"
		if o.Kept == 0 {
			row.RunsTitle = "0 kept, none protected from pruning"
		}
		switch {
		case o.Files > 0:
			row.Files = plural(o.Files, "file")
		case o.FilesNote != "":
			row.FilesBadge, row.FilesWhy = fleetFilesBadge(o.FilesNote), o.FilesNote
		}
		switch o.LastOutcome {
		case model.BackupOutcomeFailed:
			row.sortResult = 0
		case model.BackupOutcomeSkipped:
			row.sortResult = 1
		case model.BackupOutcomeOK:
			row.sortResult = 3
		default:
			row.sortResult = 2
		}
		fv.Runs += o.Runs
		fv.Kept += o.Kept
		bytes += o.Bytes
		if o.Runs == 0 {
			fv.Without++
		}
		if row.Skip {
			fv.SkippedNow++
			fv.SkippedIDs = append(fv.SkippedIDs, o.DeviceID)
		} else {
			fv.AllIDs = append(fv.AllIDs, o.DeviceID)
		}
		all = append(all, row)
	}
	fv.Size = humanBytes(bytes)
	fv.Total = len(all)
	// chips and the filtered table
	count := map[string]int{}
	match := func(key string, o model.BackupDeviceRow) bool {
		switch key {
		case fleetNone:
			return o.Runs == 0
		case fleetOld:
			return !o.NewestAt.IsZero() && fv.Now.Sub(o.NewestAt) > oldAfter
		case fleetFailed:
			return o.LastOutcome == model.BackupOutcomeFailed
		case fleetSkipped:
			return o.LastOutcome == model.BackupOutcomeSkipped
		}
		return true
	}
	for i, o := range over {
		for _, key := range []string{fleetNone, fleetOld, fleetFailed, fleetSkipped} {
			if match(key, o) {
				count[key]++
			}
		}
		if match(fv.Filter, o) {
			fv.Rows = append(fv.Rows, all[i])
		}
	}
	fv.Sort, fv.Dir = r.URL.Query().Get("sort"), r.URL.Query().Get("dir")
	if !slices.Contains([]string{"device", "newest", "result"}, fv.Sort) {
		fv.Sort = "device"
	}
	if fv.Dir != "desc" {
		fv.Dir = "asc"
	}
	slices.SortStableFunc(fv.Rows, func(a, b fleetRow) int {
		var c int
		switch fv.Sort {
		case "newest": // never first when ascending: the oldest news first
			c = a.Newest.Compare(b.Newest)
		case "result":
			c = cmp.Compare(a.sortResult, b.sortResult)
		}
		if c == 0 {
			c = strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		}
		if fv.Dir == "desc" {
			return -c
		}
		return c
	})
	chip := func(key, label string, n int) fleetChip {
		href := base + fleetPath
		if key != fleetAll {
			href += "?filter=" + key
		}
		return fleetChip{Key: key, Label: label, N: n, Href: href, Current: key == fv.Filter}
	}
	fv.Chips = []fleetChip{chip(fleetAll, "All", len(all)), chip(fleetNone, "No backup", count[fleetNone]),
		chip(fleetOld, "Older than "+plural(fv.OldDays, "day"), count[fleetOld]),
		chip(fleetFailed, "Failed last run", count[fleetFailed]), chip(fleetSkipped, "Skipped", count[fleetSkipped])}
	fv.Prune, fv.PruneRuns, fv.PruneMore, err = h.fleetPrune(ctx, base)
	if err != nil {
		return nil, err
	}
	return fv, nil
}

// fleetFilesBadge is the short word of a table cell for a run without files.
func fleetFilesBadge(note string) string {
	switch {
	case strings.HasPrefix(note, "settings only"):
		return "Settings only"
	case strings.HasPrefix(note, "files need HTTP"):
		return "Needs HTTP"
	}
	return note
}

// fleetPruneShown is how many devices "Pruned at the next run" names; the rest is a count.
const fleetPruneShown = 3

// fleetPrune groups what the next backups delete by device: runs, the oldest and the size.
func (h *handler) fleetPrune(ctx context.Context, base string) (rows []fleetPrune, runs, more int, err error) {
	list, err := h.d.BackupFleet.PruneNext(ctx)
	if err != nil {
		return nil, 0, 0, err
	}
	type key struct {
		dev model.DeviceID
		at  time.Time
	}
	seen := map[key]bool{}
	byDev := map[model.DeviceID]*fleetPrune{}
	var order []model.DeviceID
	sizes := map[model.DeviceID]int64{}
	for _, b := range list {
		k := key{b.DeviceID, b.TakenAt}
		fp := byDev[b.DeviceID]
		if fp == nil {
			fp = &fleetPrune{Name: b.DeviceName, Href: fmt.Sprintf("%s/devices/%d/backups", base, b.DeviceID), Oldest: b.TakenAt}
			byDev[b.DeviceID] = fp
			order = append(order, b.DeviceID)
		}
		if !seen[k] {
			seen[k] = true
			fp.Runs++
			runs++
		}
		if b.TakenAt.Before(fp.Oldest) {
			fp.Oldest = b.TakenAt
		}
		sizes[b.DeviceID] += b.Size
	}
	for _, id := range order {
		byDev[id].Size = humanBytes(sizes[id])
		rows = append(rows, *byDev[id])
	}
	slices.SortStableFunc(rows, func(a, b fleetPrune) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	if len(rows) > fleetPruneShown {
		more = len(rows) - fleetPruneShown
		rows = rows[:fleetPruneShown]
	}
	return rows, runs, more, nil
}

// LastLine is the one line of the no-schedule state: the last stored run in words.
func (v fleetView) LastLine() string {
	r := v.Run
	return fmt.Sprintf("%d backed up", r.OK) + failedSkipped(r.Failed, r.Skipped)
}

func failedSkipped(failed, skipped int) string {
	var s string
	if failed > 0 {
		s += fmt.Sprintf(" · %d failed", failed)
	}
	if skipped > 0 {
		s += fmt.Sprintf(" · %d skipped", skipped)
	}
	return s
}

// SortHref is the table's own address with the filter kept and the sort set; a second click on the
// current column reverses it.
func (v fleetView) SortHref(col string) string {
	dir := "asc"
	if v.Sort == col && v.Dir == "asc" {
		dir = "desc"
	}
	q := url.Values{"sort": {col}, "dir": {dir}}
	if v.Filter != fleetAll {
		q.Set("filter", v.Filter)
	}
	return v.Base + fleetPath + "?" + q.Encode()
}

// SortClass and AriaSort mark the sorted column.
func (v fleetView) SortClass(col string) string {
	switch {
	case v.Sort != col:
		return "sortlink"
	case v.Dir == "asc":
		return "sortlink is-asc"
	}
	return "sortlink is-desc"
}

func (v fleetView) AriaSort(col string) string {
	switch {
	case v.Sort != col:
		return "none"
	case v.Dir == "asc":
		return "ascending"
	}
	return "descending"
}

// FootNote is the line under the table.
func (v fleetView) FootNote() string {
	return fmt.Sprintf("%d of %d devices. Files: counted from the newest backup · Settings only (no file system on the device) · Needs HTTP (the run went over MQTT).", len(v.Rows), v.Total)
}

// AllTitle explains what "Back up all now" leaves out.
func (v fleetView) AllTitle() string {
	if v.SkippedNow == 0 {
		return "Backs up every device."
	}
	return "Backs up every device except the " + strconv.Itoa(v.SkippedNow) + " that would be skipped (offline or never answered)."
}
