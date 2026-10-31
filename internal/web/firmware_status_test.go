package web

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// statusFW is a firmware.Cache that also reports the index status.
type statusFW struct {
	st       firmware.IndexStatus
	releases atomic.Int32
}

func (f *statusFW) Releases(context.Context) ([]firmware.Release, error) {
	f.releases.Add(1)
	return []firmware.Release{{Version: "16.1.0", Files: []string{"tasmota.bin.gz"}}}, nil
}
func (f *statusFW) Ensure(context.Context, string, string) (model.Artifact, error) {
	return model.Artifact{}, nil
}
func (f *statusFW) URLFor(model.Artifact) string { return "" }
func (f *statusFW) Prune(context.Context, int) (firmware.DeleteResult, error) {
	return firmware.DeleteResult{}, nil
}
func (f *statusFW) IndexStatus() firmware.IndexStatus { return f.st }

func firmwarePageBody(t *testing.T, fw *statusFW) string {
	t.Helper()
	e := setupWith(t, func(d *Deps) { d.Firmware = fw })
	resp, body := e.do(t, "GET", "/firmware", "operator", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	return body
}
