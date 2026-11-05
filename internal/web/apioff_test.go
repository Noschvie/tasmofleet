package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// An HTTP-only device (no MQTT) whose HTTP API is off: the router's facts carry the tracker's finding also after
// its own memory forgot the refusal (app.TransportFacts.httpState).
func httpOnlyFacts() transport.Facts {
	return transport.Facts{Now: time.Now(), FirmwareBaseURL: true,
		HTTP:     transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailAPIRefused, Since: time.Now()},
		Identity: model.Identity{Family: model.ChipESP8266, Version: "16.0.0", Variant: "tasmota", FreeKB: 700}}
}

func apiOffHealth() model.Health {
	return model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), HTTPRefusedAt: time.Now()}
}

// Why (decision 2026-11-05): an HTTP-only device whose HTTP API is off is Online but has no path. A manual Back
// up must refuse up front with the reason and the fix, not try and fail, and must not start the cooldown. A
// device that MQTT can serve is not refused: what works still works.
func TestManualBackupIsRefusedUpFrontWhenTheHTTPAPIIsOffAndNothingElseReaches(t *testing.T) {
	for _, from := range []string{"", "list", "tab"} {
		e := setupWith(t, func(d *Deps) {
			d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: httpOnlyFacts()}}
		})
		e.health[1] = apiOffHealth()
		resp, body := e.do(t, "POST", "/devices/1/backup", "admin", url.Values{"csrf": {"tok"}, "from": {from}})
		got := body + resp.Header.Get("HX-Trigger")
		if e.bk.taken != 0 {
			t.Fatalf("from %q: the backup was attempted", from)
		}
		if !strings.Contains(got, "HTTP API off (SetOption128 0)") || !strings.Contains(strings.ReplaceAll(got, `(`, "("), "SetOption128 1") {
			t.Fatalf("from %q: no reason and fix in %q", from, got)
		}
		// the refusal did not use up the cooldown: after the API is back the next click works
		e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(), SeenHTTP: time.Now()}
		e.do(t, "POST", "/devices/1/backup", "admin", url.Values{"csrf": {"tok"}, "from": {from}})
		if e.bk.taken != 1 {
			t.Fatalf("from %q: the refusal started the cooldown (taken %d)", from, e.bk.taken)
		}
	}
	e := setupWith(t, func(d *Deps) {
		f := mqttFacts(time.Now(), model.MQTTPasswordOwn)
		d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: f}}
	})
	e.health[1] = apiOffHealth()
	e.do(t, "POST", "/devices/1/backup", "admin", url.Values{"csrf": {"tok"}})
	if e.bk.taken != 1 {
		t.Fatal("a device that MQTT can back up was refused because its HTTP API is off")
	}
}

// Why: the page decides on what the tracker knows, not only on the router's ten minutes: the Back up button and
// the card's action rows say "Not available" with the one name of the problem.
func TestDevicePageNamesTheAPIOffAsTheReasonForNoPath(t *testing.T) {
	e := setupWith(t, func(d *Deps) {
		d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: httpOnlyFacts()}}
	})
	e.health[1] = apiOffHealth()
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{"HTTP API off (SetOption128 0)", "SetOption128 1", "Connection: HTTP API off"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(body, "web API (SetOption128)") {
		t.Error("the transport's own wording leaked next to the one name")
	}
}

// Why: a bulk action over a device that has no path would only fail; the scope page skips it with the reason, and
// nothing is sent to it.
func TestBulkActionSkipsADeviceWhoseHTTPAPIIsOff(t *testing.T) {
	fbk := &fakeBackupBatch{}
	e := setupWith(t, func(d *Deps) {
		d.DeviceCmd = &fakeCmd{}
		d.CommandBatch = &fakeBatch{}
		d.BackupBatch = fbk
		d.Transport = fakeFacts{f: map[model.DeviceID]transport.Facts{1: httpOnlyFacts(), 2: mqttFacts(time.Now(), model.MQTTPasswordOwn)}}
	})
	e.health[1] = apiOffHealth()
	h2 := apiOffHealth()
	h2.DeviceID = 2
	e.health[2] = h2 // API off too, but MQTT serves it
	_, body := e.do(t, "POST", "/devices/bulk/backup", "admin", bulkForm("id", "1", "id", "2", "opts", "1", "run", "1"))
	_ = body
	if len(fbk.ids) != 1 || len(fbk.ids[0]) != 1 || fbk.ids[0][0] != 2 {
		t.Fatalf("backed up %v, want only device 2 (device 1 has no path)", fbk.ids)
	}
}

// Why: the update picker, the preview and the submit all refuse such a device before anything starts.
func TestUpdateRefusesADeviceWhoseHTTPAPIIsOff(t *testing.T) {
	e := setup(t)
	o := &fakeOTA{items: map[model.DeviceID]ota.PreviewItem{}}
	h, err := Handler(Deps{Guard: fakeGuard{}, Inventory: e.inv, Health: e.health, Backups: e.bk, Prober: e.pr, Audit: e.aud, Recorder: e.aud,
		Hub: e.hub, OTA: o, Jobs: &fakeJobs{}, Firmware: &fakeFW{}, Batches: fakeBatches{}, BuildMemory: e.inv,
		Transport: fakeFacts{f: map[model.DeviceID]transport.Facts{1: httpOnlyFacts()}}})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Close()
	e.srv = httptest.NewServer(http.NewServeMux())
	mux := http.NewServeMux()
	mux.Handle("/tf/", httpx.Proxy{BasePath: "/tf"}.Middleware(h))
	e.srv.Config.Handler = mux
	t.Cleanup(e.srv.Close)
	e.health[1] = apiOffHealth()

	_, picker := e.do(t, "GET", "/updates/new", "operator", nil)
	if !strings.Contains(picker, "HTTP API off (SetOption128 0)") {
		t.Errorf("the picker does not grey the device out with the reason")
	}
	form := url.Values{"id": {"1"}, "release": {"16.1.0"}, "policy": {"only-newer"}}
	if resp, body := e.do(t, "POST", "/updates/preview", "operator", form); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "SetOption128 1") {
		t.Errorf("preview: %d %s", resp.StatusCode, body)
	}
	form.Set("skip_backup", "1")
	if resp, body := e.do(t, "POST", "/updates", "operator", form); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "HTTP API off") {
		t.Errorf("submit: %d %s", resp.StatusCode, body)
	}
	if len(o.submitReqs) != 0 {
		t.Fatal("the update was submitted")
	}
}
