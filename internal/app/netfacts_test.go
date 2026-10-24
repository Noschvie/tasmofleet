package app

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: devices created by an import or "Add device" carry no network facts (first contact does
// not wait for them). The background pass must read exactly the devices that are up and were
// never read, leave out minimal firmware (it answers neither command) and not hammer a device
// whose read failed.
func TestNetFactsReaderPassReadsOnlineUnreadDevices(t *testing.T) {
	read := model.NetFacts{ReadAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	devs := []model.Device{
		{ID: 1},            // online, never read: read
		{ID: 2, Net: read}, // already read
		{ID: 3},            // offline
		{ID: 4, Identity: model.Identity{Variant: "minimal"}},
		{ID: 5}, // online, the read fails
	}
	var got []model.DeviceID
	r := newNetFactsReader(
		func(context.Context) ([]model.Device, error) { return devs, nil },
		func(id model.DeviceID) bool { return id != 3 },
		func(_ context.Context, id model.DeviceID) error {
			got = append(got, id)
			if id == 5 {
				return errors.New("timeout")
			}
			return nil
		}, slog.New(slog.DiscardHandler))
	r.pass(context.Background())
	if !slices.Equal(got, []model.DeviceID{1, 5}) {
		t.Fatalf("first pass read %v, want [1 5]", got)
	}
	got = nil
	devs[0].Net = read // device 1 is stored now
	r.pass(context.Background())
	if len(got) != 0 {
		t.Fatalf("second pass read %v: device 5 failed a moment ago and must wait for its retry", got)
	}
}
