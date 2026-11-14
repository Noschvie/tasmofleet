package ota

import (
	"context"
	"sync"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// pinRecBackups records the path the backup of an update job was pinned to ("unpinned" when it was not).
type pinRecBackups struct {
	*fakeBackups
	mu   sync.Mutex
	pins []string
}

func (b *pinRecBackups) Backup(ctx context.Context, actor string, id model.DeviceID) ([]model.Backup, error) {
	p, ok := transport.PathFrom(ctx)
	b.mu.Lock()
	if ok {
		b.pins = append(b.pins, string(p))
	} else {
		b.pins = append(b.pins, "unpinned")
	}
	b.mu.Unlock()
	return b.fakeBackups.Backup(ctx, actor, id)
}

// Why (#607, the product owner's transport rule of 2026-11-14): the backup before an update is a backup like any other
// and goes over MQTT first when the device is usable there, whatever carries the update. An update over HTTP must not
// pin its backup to HTTP; the backup's own rule decides (the router, which never moves a transfer that published to
// the other transport). An update over MQTT keeps its backup on MQTT: HTTP is not usable for it (D5).
func TestTheBackupBeforeAnUpdateTakesTheBackupsOwnTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		path transport.Path
		pin  transport.Path // the update's own calls
		want string         // the backup's pin
	}{
		{"update over HTTP: the backup decides for itself (MQTT first)", transport.DownloadHTTP, transport.HTTP, "unpinned"},
		{"update over MQTT: the backup stays on MQTT", transport.DownloadMQTT, transport.MQTT, string(transport.MQTT)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, 2, plugDevice())
			spy := &pathSpy{fakeAPI: h.api}
			bs := &pinRecBackups{fakeBackups: h.backups}
			paths := pathsOf(tc.path, "because")
			paths.Backup = transport.MQTT
			h.e = NewEngine(Options{Machine: testCfg(), MaxParallel: 2, Paths: &fakePaths{paths: paths}}, h.st, h.st, spy, h.fw, bs, h.clk, nil)
			h.e.sel = h.sel
			id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0"})
			spy.seen = nil // the preview probe is unpinned on purpose: it is what chooses
			h.run(t, h.e)
			if j := h.job(t, id); j.State != model.JobSucceeded {
				t.Fatalf("job %s %q", j.State, j.LastError)
			}
			bs.mu.Lock()
			defer bs.mu.Unlock()
			if len(bs.pins) != 1 || bs.pins[0] != tc.want {
				t.Fatalf("backup pinned %v, want [%s]", bs.pins, tc.want)
			}
			if !spy.only(tc.pin) {
				t.Errorf("the update's own calls %v must stay pinned to its path", spy.seen)
			}
		})
	}
}
