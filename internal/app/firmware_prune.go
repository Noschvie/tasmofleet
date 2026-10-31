package app

import (
	"context"
	"log/slog"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// pruneFirmwareCache deletes the cached firmware of releases older than the newest keep ones
// (FIRMWARE_KEEP_RELEASES; 0 = off). The cache itself spares files a queued or running update
// needs and uploads. A run that deletes something or fails is audited as "system" with the
// count and the bytes freed; a run that finds nothing to do stays silent (it runs after every
// index refresh and daily).
func pruneFirmwareCache(ctx context.Context, c firmware.Cache, keep int, rec audit.Recorder, log *slog.Logger) {
	if c == nil || keep <= 0 {
		return
	}
	res, err := c.Prune(ctx, keep)
	ev := model.AuditEvent{Actor: "system", Action: "firmware.prune", Target: "cache", Result: "ok"}
	if err != nil {
		ev.Result = "failed: " + err.Error()
		log.Warn("firmware cache pruning failed", "err", err, "deleted", len(res.Deleted), "freed", res.Freed)
	} else if len(res.Deleted) == 0 {
		return
	}
	ev.Detail = auditJSON(map[string]any{"keep": keep, "files": len(res.Deleted), "bytes": res.Freed, "kept_in_use": res.Kept})
	log.Info("firmware cache pruned", "keep", keep, "files", len(res.Deleted), "bytes", res.Freed)
	rec.Record(ctx, ev)
}

// pruneFirmware is pruneFirmwareCache with the live setting.
func (a *runtime) pruneFirmware(ctx context.Context) {
	pruneFirmwareCache(ctx, a.cache, a.svc.Sys.Snapshot().FWKeepReleases, a.svc.Audit, a.log)
}
