package ota

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// BackupOptions are the choices of a bulk backup: "Keep these backups" and a
// "Label" stored with each dump. They travel in the job's Commands after the
// marker (one line each), so a job resumed after a restart or retried keeps
// them.
type BackupOptions struct {
	Keep  bool
	Label string
}

const (
	backupKeepLine  = "keep"
	backupLabelLine = "label "
)

func (o BackupOptions) commands() []string {
	out := []string{model.BackupMarker}
	if o.Keep {
		out = append(out, backupKeepLine)
	}
	if l := backup.CleanLabel(o.Label); l != "" {
		out = append(out, backupLabelLine+l)
	}
	return out
}

// backupRun reads the options back from a job's Commands.
func backupRun(commands string) backup.Run {
	r := backup.Run{Trigger: model.TriggerBulk}
	for _, line := range strings.Split(commands, "\n") {
		switch {
		case line == backupKeepLine:
			r.Keep = true
		case strings.HasPrefix(line, backupLabelLine):
			r.Label = strings.TrimPrefix(line, backupLabelLine)
		}
	}
	return r
}

// "Back up" for several devices runs as a transaction too: one job per device,
// shown and retried like a command. The work is the backup service's (the same
// call as the single-device button and the backup before an update). Like a
// command, it is sent as an active admin decided when the backup is taken.

// SubmitBackup creates a transaction that backs up each device. Devices that
// already have an active job are returned with the reason and the others still
// start; ErrNothingToFix when none is left.
func (e *Engine) SubmitBackup(ctx context.Context, actor string, ids []model.DeviceID) (model.BatchID, []Refusal, error) {
	return e.SubmitBackupWith(ctx, actor, ids, BackupOptions{})
}

// SubmitBackupWith is SubmitBackup with the bulk options.
func (e *Engine) SubmitBackupWith(ctx context.Context, actor string, ids []model.DeviceID, opt BackupOptions) (model.BatchID, []Refusal, error) {
	reqs := make([]PolicyRequest, len(ids))
	for i, id := range ids {
		reqs[i] = PolicyRequest{DeviceID: id, Commands: opt.commands()}
	}
	id, skipped, err := e.submitPolicy(ctx, actor, model.KindBackup, reqs, 0)
	if e.audit != nil && err == nil {
		skip := map[model.DeviceID]bool{}
		for _, s := range skipped {
			skip[s.DeviceID] = true
		}
		var devs []model.DeviceID
		for _, d := range ids {
			if !skip[d] {
				devs = append(devs, d)
			}
		}
		detail, _ := json.Marshal(map[string]any{"devices": devs, "skipped": len(skipped), "keep": opt.Keep, "label": backup.CleanLabel(opt.Label)}) // a plain map always encodes
		e.audit.Record(ctx, model.AuditEvent{Actor: actor, Action: "backup.bulk.start", Target: "transaction:" + strconv.FormatInt(int64(id), 10), Detail: string(detail), Result: "ok"})
	}
	return id, skipped, err
}

// workBackup drives one backup job to its end. A backup is safe to repeat, so a
// job found mid-way after a restart simply runs again.
func (e *Engine) workBackup(ctx context.Context, j model.Job) {
	if e.backups == nil {
		e.finishCommand(ctx, j, "", "this server cannot take backups")
		return
	}
	if j.State != model.JobApplying {
		var ok bool
		if j, ok = e.policyMove(ctx, j, model.JobApplying, "backup", ""); !ok {
			return
		}
	}
	if _, err := e.currentAdmin(ctx, j.CreatedKind, j.CreatedBy); err != nil {
		e.auditCommandRefusal(ctx, j, err)
		e.finishCommand(ctx, j, "", err.Error())
		return
	}
	bs, err := e.backups.Backup(backup.WithRun(ctx, backupRun(j.Commands)), j.CreatedBy, j.DeviceID)
	if ctx.Err() != nil {
		return // shutting down: the job runs again
	}
	if err != nil {
		e.finishCommand(ctx, j, "", err.Error())
		return
	}
	note := "backup taken"
	if len(bs) > 1 {
		note = strconv.Itoa(len(bs)) + " backup files taken"
	}
	e.finishCommand(ctx, j, note, "")
}
