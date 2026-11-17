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
	// PrunePlan says, per device, what Prune now would remove (#622): the runs the retention lists
	// minus the newest run and the runs a restore uses, and whether the device is left out.
	PrunePlan(ctx context.Context) ([]PruneDevice, error)
	// LastMissed is when a scheduled run was last skipped because TasmoFleet was down at its time
	// (#601); zero when that never happened.
	LastMissed(ctx context.Context) (time.Time, error)
}

// PruneDevice is what Prune now does with one device. The rule for one device (never its newest
// run, never one a restore uses) is backup.PruneNowSet; the one fleet rule, LeftOut, is the
// implementer's.
type PruneDevice struct {
	Device model.DeviceID
	Name   string
	// Files are the files of the runs Prune now removes from the device (never empty).
	Files []model.Backup
	// Held counts the runs the retention lists that Prune now keeps (newest, used by a restore).
	Held int
	// LeftOut is why Prune now leaves the device alone ("its last run did not back it up"); empty
	// when it takes part.
	LeftOut string
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
	LeftOut    string // why Prune now leaves it alone; empty: it takes part
}

// fleetPruneRun is one backup run Prune now removes: the id is the run's dump row, what the form posts.
type fleetPruneRun struct {
	ID    model.BackupID
	Name  string
	Taken time.Time
	Bytes int64
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
	ACLText     string           // not empty: a broker is configured, so a backup travels over MQTT: the broker ACL notice (#853)
	SkippedIDs  []model.DeviceID // the ones it would leave out
	SkippedNow  int
	OldDays     int
	Runs, Kept  int
	Size        string
	Without     int
	WithoutHref string
	Keep        string // BACKUP_KEEP as shown, for the prune note
	PruneRuns   int    // runs the retention lists for the next backups, left-out devices included
	Prune       []fleetPrune
	PruneSize   string
	// What Prune now takes (#622): the listed runs of the devices that take part.
	PruneNow        []fleetPruneRun
	PruneNowDevices int
	PruneNowSize    string
	PruneLeftOut    []string // the names of the devices left out
	PruneHeld       int      // runs kept back by the never-the-newest and restore rules
	CanBulk         bool     // admin and the backup service can run the bulk actions
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
	// view takes the stream's sequence number before the fleet below is read (#160), so a backup stored
	// while the page is built is told to the browser as a resync. The cards follow a run from the stream (#950).
	v := h.view(r, "Backups", "backups", nil)
	v.Live = true
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
		st.CanBulk = fv.CanBulk
		if !v.IsAdmin { // an operator may look and keep, never delete
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
	h.registerFleetBulk(route, adm)
}

// fleetOf reads the last run and the per-device overview and arranges them for the page.
func (h *handler) fleetOf(r *http.Request, admin bool) (*fleetView, error) {
	ctx := r.Context()
	base := httpBase(r)
	fv := &fleetView{Base: base, Admin: admin, ACLText: h.backupACLText(), Now: h.d.Clock.Now(), Filter: r.URL.Query().Get("filter"), WithoutHref: base + fleetPath + "?filter=" + fleetNone}
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
	_, bulk := h.d.Backups.(BackupBulk)
	fv.CanBulk = admin && bulk
	if err := h.fleetPrune(ctx, base, fv); err != nil {
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

// fleetFailedShown is how many failed devices of the last run the Last run card names; the table below lists
// every one, and the card links the filter for the rest (#684).
const fleetFailedShown = 3

// pruneRunsOf groups a device's files into the runs Prune now removes, oldest first. The id is the
// run's dump row (or its first file when the run has no dump): the confirmation posts it, and the
// POST works the same list out again to see what is still allowed.
func pruneRunsOf(d PruneDevice) []fleetPruneRun {
	var out []fleetPruneRun
	at := map[time.Time]int{}
	for _, b := range d.Files {
		i, ok := at[b.TakenAt]
		if !ok {
			i = len(out)
			at[b.TakenAt] = i
			out = append(out, fleetPruneRun{ID: b.ID, Name: d.Name, Taken: b.TakenAt})
		} else if b.Kind == model.BackupConfigDump {
			out[i].ID = b.ID
		}
		out[i].Bytes += b.Size
	}
	slices.SortFunc(out, func(a, b fleetPruneRun) int { return a.Taken.Compare(b.Taken) })
	return out
}

// fleetPrune fills the Storage card's prune list and Prune now's confirmation from the plan: per
// device the runs, the oldest and the size; the runs Prune now takes; the devices it leaves out.
func (h *handler) fleetPrune(ctx context.Context, base string, fv *fleetView) error {
	plan, err := h.d.BackupFleet.PrunePlan(ctx)
	if err != nil {
		return err
	}
	var all, now int64
	for _, d := range plan {
		runs := pruneRunsOf(d)
		fp := fleetPrune{Name: d.Name, Href: fmt.Sprintf("%s/devices/%d/backups", base, d.Device), Runs: len(runs), Oldest: runs[0].Taken, LeftOut: d.LeftOut}
		var size int64
		for _, r := range runs {
			size += r.Bytes
		}
		fp.Size = humanBytes(size)
		fv.Prune = append(fv.Prune, fp)
		fv.PruneRuns += len(runs)
		fv.PruneHeld += d.Held
		all += size
		if d.LeftOut != "" {
			fv.PruneLeftOut = append(fv.PruneLeftOut, d.Name)
			continue
		}
		fv.PruneNow = append(fv.PruneNow, runs...)
		fv.PruneNowDevices++
		now += size
	}
	slices.SortStableFunc(fv.Prune, func(a, b fleetPrune) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	slices.SortStableFunc(fv.PruneNow, func(a, b fleetPruneRun) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return a.Taken.Compare(b.Taken)
	})
	slices.Sort(fv.PruneLeftOut)
	fv.PruneSize, fv.PruneNowSize = humanBytes(all), humanBytes(now)
	return nil
}

// ChipHref is the page with the device table filtered by a chip, scrolled to the table.
func (v fleetView) ChipHref(filter string) string {
	return v.Base + fleetPath + "?filter=" + filter + "#card-devices"
}

// ResultTitle is the full text of the Last result cell of a failed or skipped device, for the cell that cuts
// it to one line; empty when the cell holds nothing that can be cut.
func (r fleetRow) ResultTitle() string {
	if r.Outcome == model.BackupOutcomeFailed || r.Outcome == model.BackupOutcomeSkipped {
		return r.Why.Plain()
	}
	return ""
}

// fleetScheduleLine is the retention under the schedule's days and time: what is kept per device, and how
// old a backup must be before pruning may take it.
func fleetScheduleLine(c sysCard) string {
	var keep, age string
	for _, row := range c.Rows {
		switch row.Key {
		case "BACKUP_KEEP":
			keep = row.Value
		case "BACKUP_MAX_AGE":
			age = fleetScheduleValue(row)
		}
	}
	return keep + " kept per device · never pruned younger than " + age + "."
}

// orphanSummary counts the backups of deleted devices, how many of them are kept and what they take.
func orphanSummary(st sysStorage) string {
	kept := 0
	for _, o := range st.Orphans {
		if o.Kept {
			kept++
		}
	}
	s := plural(len(st.Orphans), "backup") + " · " + st.OrphanSize
	if kept > 0 {
		s += fmt.Sprintf(" · %d kept", kept)
	}
	return s
}

// orphanGroup is the backups of one deleted device (name and MAC), newest first.
type orphanGroup struct {
	MAC  string
	Runs []sysOrphan
}

// OrphanGroups groups the runs by deleted device, in the order of each device's newest run.
func (st sysStorage) OrphanGroups() []orphanGroup {
	var out []orphanGroup
	at := map[[2]string]int{}
	for _, o := range st.Orphans {
		k := [2]string{o.Name, o.MAC}
		i, ok := at[k]
		if !ok {
			i = len(out)
			at[k] = i
			out = append(out, orphanGroup{MAC: o.MAC})
		}
		out[i].Runs = append(out[i].Runs, o)
	}
	return out
}

// Selectable counts the runs a bulk delete may take: every run that is not kept.
func (st sysStorage) Selectable() int {
	n := 0
	for _, o := range st.Orphans {
		if !o.Kept {
			n++
		}
	}
	return n
}

// SelectAllText labels the box above the list; it says what a kept run does to the count.
func (st sysStorage) SelectAllText() string {
	n, kept := st.Selectable(), len(st.Orphans)-st.Selectable()
	s := fmt.Sprintf("Select all that can go (%d", n)
	if kept > 0 {
		s += fmt.Sprintf("; %d kept stay", kept)
	}
	return s + ")"
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

// LiveHref is the address the Devices card reloads itself from (#950): this page with the filter and the sort
// the reader chose. The notice of a save (ok=) is not kept, or every refresh would show it again.
func (v fleetView) LiveHref() string {
	q := url.Values{"sort": {v.Sort}, "dir": {v.Dir}}
	if v.Filter != fleetAll {
		q.Set("filter", v.Filter)
	}
	return v.Base + fleetPath + "?" + q.Encode()
}

// LiveOOB names what the same reload replaces besides the Devices card: the cards a backup run changes (Last
// run, Storage), Prune now's dialog, which is built from the same plan as the Storage card, and for an admin
// the ids Back up all now posts. The schedule, the deleted devices' backups and the unreferenced files hold a
// draft, an open fold or a form of their own and are never swapped. Each id is on the page once, empty
// or not, so the swap always finds it.
func (v fleetView) LiveOOB() string {
	ids := "card-lastrun,card-storage,bk-prune"
	if v.Admin {
		ids += ",bk-all"
	}
	return ids
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
	return fmt.Sprintf("%d of %d devices. Files: counted from the newest backup · Settings only (no file system on the device) · Needs HTTP (MQTT could not carry the files and HTTP did not; the device's Backups tab says why).", len(v.Rows), v.Total)
}

// AllTitle explains what "Back up all now" leaves out.
func (v fleetView) AllTitle() string {
	if v.SkippedNow == 0 {
		return "Backs up every device."
	}
	return "Backs up every device except the " + strconv.Itoa(v.SkippedNow) + " that would be skipped (offline or never answered)."
}
