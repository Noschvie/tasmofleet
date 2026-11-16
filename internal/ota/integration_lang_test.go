//go:build integration

package ota

import (
	"context"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/simulator"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// pagesRead counts the main page reads of the device client it wraps.
type pagesRead struct {
	tasmota.Client
	n atomic.Int32
}

func (p *pagesRead) RootPage(ctx context.Context, t tasmota.Target) (tasmota.RootInfo, error) {
	p.n.Add(1)
	return p.Client.RootPage(ctx, t)
}

// Why (#686): a German 1 MB ESP8266 is updated through minimal over HTTP without a single main page read. The
// preflight reads the language from the Status 4 locale and the chip from Status 2; the minimal hop is named by
// the job's own family and the bare Upgrade reply; and the final image is the German build the preflight chose,
// although the minimal firmware in between reports no language at all.
func TestITStagedGermanESP8266ReadsNoMainPage(t *testing.T) {
	e := newItEnv(t)
	pages := &pagesRead{Client: tasmota.NewHTTPClient(tasmota.Options{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		Timeout:         1500 * time.Millisecond,
	})}
	api := NewDeviceAPI(pages)
	api.(*deviceAPI).gap = 5 * time.Millisecond
	e.api = &guardedAPI{DeviceAPI: api, env: e}
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M, Locale: "0407"})
	orig := e.ctrl[dev].OtaURL()
	eng := e.start(2)
	e.submit(eng, Request{DeviceID: dev})
	j := e.expectSuccess(dev, orig, 2, model.PlanStaged)
	if !j.IdentityBefore.LanguageKnown || j.IdentityBefore.Language != "DE" {
		t.Fatalf("preflight identity %+v, want the German build read from Status 4", j.IdentityBefore)
	}
	flashed := false
	for _, c := range e.ctrl[dev].Commands() {
		flashed = flashed || strings.HasSuffix(c, "/fw/"+itRelease+"/tasmota-DE.bin.gz")
	}
	if !flashed {
		t.Fatalf("the German file was never set as OtaUrl: %v", e.ctrl[dev].Commands())
	}
	if n := pages.n.Load(); n != 0 {
		t.Fatalf("the update read the main page %d times", n)
	}
}
