package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The restore pages (design v2 screen "restore", WP P10): the confirmation with what will
// change, then the progress of the job in four steps, ending done or failed.

// restoreConfirmView is the confirmation of a restore.
type restoreConfirmView struct {
	Sub        string // "Living Room Ceiling Light · backup from 22 Oct 2026, 09:41 · Before update · Tasmota 14.2.0 · 4.1 KiB"
	Error      string
	Missing    bool // the acknowledgement was not ticked
	Refused    bool // the live checks refuse this restore: no form
	HasPreview bool
	Changes    []ui.DiffRow // Old = now on the device, New = after the restore
	Risks      []string     // risks the admin may accept (the live checks' detail): warn banners
	Warnings   []string     // what the restore will do that can hurt: danger banners
	Action     string
	CSRF       string
	Hidden     []ui.KVRow // the risk flags the form carries
	Ack        string     // the acknowledgement's text
	CancelHref string
}

// backupSub is the line under the restore title: device, backup date, trigger, firmware, size.
func backupSub(name string, b model.Backup) string {
	parts := []string{name, "backup from " + shortDate(b.TakenAt)}
	if t := triggerShort(b.Trigger); t != "" {
		parts = append(parts, t)
	}
	if b.Version != "" {
		parts = append(parts, "Tasmota "+b.Version)
	}
	return strings.Join(append(parts, humanBytes(b.Size)), " · ")
}

// restoreConfirmOf turns the checked restore (confirmPage) into the page's view.
func restoreConfirmOf(v view, p confirmPage) restoreConfirmView {
	c := restoreConfirmView{Sub: backupSub(p.Device.Name, p.Backup), Refused: p.Refused, HasPreview: p.HasPreview,
		Action: fmt.Sprintf("%s/backups/%d/restore", v.Base, p.Backup.ID), CSRF: v.CSRF,
		CancelHref: fmt.Sprintf("%s/devices/%d/backups", v.Base, p.Device.ID)}
	if p.Error == needConfirm {
		c.Missing = true
	} else {
		c.Error = p.Error
	}
	for _, r := range p.Rows {
		row := ui.DiffRow{Key: r.Path, Old: r.Old, New: r.New, Changed: true}
		switch {
		case r.Secret:
			row.Old, row.New = "secret (not shown)", "secret value changed (not shown)"
		case r.Hidden:
			row.Old, row.New = "text (visible to admins only)", "text changed (visible to admins only)"
		case r.Added:
			row.Old = "(absent)"
		case r.Removed:
			row.New = "(removed)"
		}
		c.Changes = append(c.Changes, row)
	}
	if p.Detail != "" {
		c.Risks = append(c.Risks, sentence(p.Detail))
	}
	if p.MACUnknown {
		chip := "chip family unknown"
		if p.LiveChip != "" {
			chip = "chip " + p.LiveChip
		}
		c.Warnings = append(c.Warnings, fmt.Sprintf("The inventory has no MAC for %s. Device at %s reports MAC %s, %s — this will be recorded as its identity. Check that this is the right device.", p.Device.Name, p.LiveAddr, p.LiveMAC, chip))
		c.Hidden = append(c.Hidden, ui.KVRow{Key: "expect_mac", Value: p.LiveMAC})
	}
	if p.Secrets {
		c.Warnings = append(c.Warnings, "Secret values differ between the backup and the current configuration; they will be replaced.")
	}
	if p.WifiChange {
		c.Warnings = append(c.Warnings, "The Wi-Fi or network settings differ. If the backup holds settings the device cannot use, it may not rejoin the network and will have to be recovered on site.")
	}
	if p.NeedMajor {
		c.Hidden = append(c.Hidden, ui.KVRow{Key: "major_ok", Value: "1"})
	}
	if p.NeedFamily {
		c.Hidden = append(c.Hidden, ui.KVRow{Key: "family_ok", Value: "1"})
	}
	ack := "I understand that " + p.Device.Name + " restarts, its configuration is replaced by this backup and the old credentials (Wi-Fi, MQTT, web password) come back"
	if p.MACUnknown {
		ack += "; that MAC " + p.LiveMAC + " is recorded as its identity"
	}
	if p.NeedMajor {
		ack += "; that the backup is from another firmware major version"
	}
	if p.NeedFamily {
		ack += "; and I confirm that the backup belongs to the same chip family as " + p.Device.Name
	}
	c.Ack = ack + "."
	return c
}

func (h *handler) renderConfirm(w http.ResponseWriter, r *http.Request, status int, p confirmPage) {
	v := h.view(r, "Restore backup", "devices", nil)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: p.Device.Name, Path: fmt.Sprintf("/devices/%d", p.Device.ID)}, {Label: "Restore"}}
	h.show(w, r, status, "restore_confirm", v, restoreConfirm(restoreConfirmOf(v, p)))
}

// restoreStepView is one step of the progress list.
type restoreStepView struct {
	N             int
	Label, Detail string
	State         string // done, active, failed or todo
}

// Mark is what the step's circle shows.
func (s restoreStepView) Mark() string {
	switch s.State {
	case "done":
		return "✓"
	case "failed":
		return "✕"
	case "active":
		return "•"
	}
	return fmt.Sprint(s.N)
}

// restoreJobView is the progress page of a restore or a replace, and its polled fragment.
type restoreJobView struct {
	Title, Sub   string
	StatusURL    string // polled while the job runs
	Running      bool
	Done, Failed bool
	Banner       string // after "Restore finished." / "Restore failed."
	Lead         string
	Steps        []restoreStepView
	RollbackHref string // restore the backup taken before the restore
	RollbackText string
	RetryHref    string
	BackHref     string
	DeviceHref   string // a finished replace: open the device
}

// restoreJobOf builds the progress view of a job.
func restoreJobOf(base string, p jobPage, target, pre model.Backup) restoreJobView {
	j := p.Job
	replace := j.Kind == store.KindReplace
	v := restoreJobView{Title: "Restore backup", StatusURL: fmt.Sprintf("%s/restores/%d/status", base, j.ID),
		Running: !p.Terminal, Done: j.State == store.RestoreDone, Failed: j.State == store.RestoreFailed,
		BackHref: fmt.Sprintf("%s/devices/%d/backups", base, j.DeviceID)}
	if target.ID != 0 {
		v.Sub = backupSub(p.Device.Name, target)
	} else {
		v.Sub = fmt.Sprintf("%s · backup #%d", p.Device.Name, j.BackupID)
	}
	labels := [4]string{"Back up the current state", "Upload the backup to the device", "Restart and wait for the device", "Verify settings"}
	if replace {
		v.Title = "Replace device"
		v.Sub = fmt.Sprintf("%s · onto the new device %s", p.Device.Name, j.NewMAC)
		labels = [4]string{"Back up the new device", "Upload the backup to the new device", "Restart and wait for the new device", "Verify settings and take over the entry"}
	}
	step := restore.Step(j)
	for i, l := range labels {
		n := i + 1
		s := restoreStepView{N: n, Label: l, State: "todo"}
		switch {
		case n < step:
			s.State = "done"
		case n == step && v.Failed:
			s.State, s.Detail = "failed", sentence(j.Detail)
		case n == step:
			s.State, s.Detail = "active", sentence(j.Detail)
		}
		if s.State == "done" {
			switch n {
			case restore.StepBackup:
				if pre.ID != 0 {
					s.Detail = "Saved " + shortDate(pre.TakenAt) + " · " + humanBytes(pre.Size)
					if t := backupTriggerText(pre.Trigger, pre.TriggerRef); t != "" {
						s.Detail += " · labelled “" + t + "”"
					}
				}
			case restore.StepUpload:
				if target.ID != 0 {
					s.Detail = humanBytes(target.Size) + " uploaded over HTTP"
				}
			case restore.StepRestart:
				s.Detail = "Back online"
			case restore.StepVerify:
				s.Detail = sentence(j.Detail)
			}
		}
		v.Steps = append(v.Steps, s)
	}
	what := "Restore"
	if replace {
		what = "Replace"
	}
	switch {
	case v.Done && replace:
		v.Lead = what + " finished."
		v.Banner = p.Device.Name + " now stands for the new device (" + j.NewMAC + ")."
		v.DeviceHref = fmt.Sprintf("%s/devices/%d", base, j.DeviceID)
	case v.Done:
		v.Lead = what + " finished."
		v.Banner = "The device is back online with the settings from " + shortDate(target.TakenAt) + "."
		if j.Verified {
			v.Banner += " Verified: the device holds the restored configuration."
		}
	case v.Failed && pre.ID != 0 && step > restore.StepUpload:
		// Only a failure after the upload may have changed the device; before it, the
		// failure text says nothing was changed and there is nothing to roll back.
		v.Lead = what + " failed."
		v.Banner = "The state before was backed up at " + pre.TakenAt.Format("15:04") + " before anything changed, so you can roll back."
		if !replace {
			v.RollbackHref = fmt.Sprintf("%s/backups/%d/restore", base, pre.ID)
			v.RollbackText = "Restore the " + pre.TakenAt.Format("15:04") + " backup"
		}
	case v.Failed:
		v.Lead = what + " failed."
		v.Banner = "Nothing was changed on the device."
	}
	if v.Failed && !replace {
		v.RetryHref = fmt.Sprintf("%s/backups/%d/restore", base, j.BackupID)
	}
	return v
}

// jobBackups looks up the restored backup and the one taken before the restore (zero when
// gone or not taken yet). A failed read only costs the details.
func (h *handler) jobBackups(r *http.Request, j restore.Job) (target, pre model.Backup) {
	list, err := h.d.Backups.List(r.Context(), j.DeviceID)
	if err != nil {
		h.log.WarnContext(r.Context(), "restore page: backups", "err", err)
		return target, pre
	}
	for _, b := range list {
		switch b.ID {
		case j.BackupID:
			target = b
		case j.PreBackupID:
			pre = b
		}
	}
	return target, pre
}

// writeComponent answers with one component (an htmx fragment).
func writeComponent(w http.ResponseWriter, r *http.Request, status int, c templ.Component) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	return c.Render(r.Context(), w)
}
