package app

import (
	"context"
	"fmt"

	"github.com/hellwand/tasmofleet/internal/devlock"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/restore"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// setupRestore builds the restore service. A restore over HTTP is the device's own restore upload
// and one over MQTT is FileUpload; neither needs the firmware listener (#430). Which one a device
// gets is decided per restore from the transport facts. Restores that were running when the
// process stopped are failed: nobody watches their device any more.
func (a *runtime) setupRestore() error {
	if a.backups == nil {
		return nil
	}
	n, err := a.svc.Store.FailActiveRestores(context.Background(), "interrupted by a restart of tasmofleet", restore.InterruptedDetails())
	if err != nil {
		return fmt.Errorf("restore recovery: %w", err)
	}
	if n > 0 {
		a.log.Warn("restores interrupted by the restart were marked failed", "count", n)
	}
	m, err := restore.New(a.restoreOptions())
	if err != nil {
		return err
	}
	a.restore = m
	a.goBG("restore", func() error { <-a.bgContext.Done(); m.Wait(); return nil })
	return nil
}

// restoreOptions is what the restore service is built from. Its device writes (the restore upload, the MQTT
// FileUpload, the takeover of an entry by new hardware) go through the wrappers that tell the Settings
// service what they wrote (#676).
func (a *runtime) restoreOptions() restore.Options {
	return restore.Options{
		Devices: a.svc.Store, Backups: a.backups, Files: a.backups, Store: a.svc.Store, Client: a.restoreClient(a.svc.Client),
		Uploader: a.restoreUploader(a.svc.Router), PathFor: a.restorePath, MarkMQTTConnChanged: a.markMQTTConnChanged,
		BrokerPassword: func() string { return a.svc.Sys.Snapshot().MQTTPassword }, Audit: a.svc.Audit, Log: a.log, Context: a.bgContext,
		Locks: a.deviceLocks(), Jobs: a.svc.Store,
		Acquire:  deviceGuard{locks: a.deviceLocks()}.acquire, // a person's operation: waits for the background read (#670)
		Hardware: a.restoreHardware(a.svc.Store), Describe: a.svc.Prober.Describe,
	}
}

// restorePath is transport.Choose's verdict for a restore of d: HTTP (restore upload), MQTT (FileUpload)
// or None, with the sentence why. Without facts (nothing to judge) the established path, HTTP.
func (a *runtime) restorePath(ctx context.Context, d model.Device) (transport.Path, string) {
	if a.facts == nil {
		return transport.HTTP, ""
	}
	f, err := a.facts.FactsFor(ctx, d)
	if err != nil {
		return transport.None, "the transport facts of the device could not be read: " + err.Error()
	}
	p := transport.Choose(f)
	return p.Restore, p.Reason(transport.ActRestore)
}

// markMQTTConnChanged marks a device whose MQTT connection a restore over MQTT just changed while HTTP answered: the
// router then sends its next commands over HTTP while HTTP works (#607, as after such a write, #605 T10).
func (a *runtime) markMQTTConnChanged(id model.DeviceID) {
	if a.svc.Router == nil {
		return
	}
	a.svc.Router.MQTTConnChanged().Fail(id, transport.HTTPFailUnreachable)
}

// deviceLocks is the per-device operation lock set. Restore uses it now; OTA,
// settings and console must take the same locks (see docs/security/restore.md).
func (a *runtime) deviceLocks() *devlock.Locks {
	if a.devLocks == nil {
		a.devLocks = devlock.New()
	}
	return a.devLocks
}
