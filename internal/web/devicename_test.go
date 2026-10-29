package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// fakeNames keeps "Keep" in the fake inventory's devices, with the store's rule: only for the name
// the device reports now.
type fakeNames struct{ inv *fakeInv }

func (f fakeNames) DismissDeviceName(_ context.Context, id model.DeviceID, name string) error {
	f.inv.mu.Lock()
	defer f.inv.mu.Unlock()
	d, ok := f.inv.devs[id]
	switch {
	case !ok:
		return store.ErrNotFound
	case d.DeviceName != name || name == "":
		return fmt.Errorf("%w: another name now", store.ErrConflict)
	}
	d.KeptDeviceName = name
	f.inv.devs[id] = d
	return nil
}

func nameSetup(t *testing.T) *env {
	t.Helper()
	e := setupWith(t, func(d *Deps) { d.DeviceNames = fakeNames{inv: d.Inventory.(*fakeInv)} })
	e.inv.devs[1] = model.Device{ID: 1, Name: "Fan", Address: "10.0.0.1", MAC: "aa:bb:cc:dd:ee:ff", DeviceName: "Modbus Lüftung Test",
		Identity: model.Identity{Family: model.ChipESP32, Version: "16.1.0", Variant: "tasmota32"}}
	return e
}

func nameForm(name string) url.Values { return url.Values{"csrf": {"tok"}, "name": {name}} }

// Why (#125): a name set by hand is never overwritten, but the admin has to learn that the device
// says something else. The Identity card shows the DeviceName with the two answers; an operator
// sees the notice but cannot answer (renaming is an admin edit).
func TestDevicePageOffersTheDeviceNameWhenItDiffersFromAHandSetName(t *testing.T) {
	e := nameSetup(t)
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	for _, want := range []string{`Name on the device:`, `Modbus Lüftung Test`, `>Use device name</button>`, `>Keep</button>`, `/devices/1/name/adopt`, `/devices/1/name/keep`} {
		if !strings.Contains(body, want) {
			t.Errorf("admin page lacks %q", want)
		}
	}
	_, body = e.do(t, "GET", "/devices/1", "operator", nil)
	if !strings.Contains(body, `Name on the device:`) || strings.Contains(body, `>Use device name</button>`) || strings.Contains(body, `>Keep</button>`) {
		t.Errorf("operator sees the notice without the buttons, got:\n%s", body)
	}
}

// Why (#125): no notice when there is nothing to decide: the names agree, the device reports none
// yet, Tasmota's default name (it would nag on every device nobody named), or the admin already
// said Keep to exactly this name.
func TestNoNameOfferWhenThereIsNothingToDecide(t *testing.T) {
	for name, mutate := range map[string]func(*model.Device){
		"same name":      func(d *model.Device) { d.Name = d.DeviceName },
		"not read yet":   func(d *model.Device) { d.DeviceName = "" },
		"default name":   func(d *model.Device) { d.DeviceName = "Tasmota" },
		"kept this name": func(d *model.Device) { d.KeptDeviceName = d.DeviceName },
	} {
		t.Run(name, func(t *testing.T) {
			e := nameSetup(t)
			d := e.inv.devs[1]
			mutate(&d)
			e.inv.devs[1] = d
			_, body := e.do(t, "GET", "/devices/1", "admin", nil)
			if strings.Contains(body, `Name on the device:`) {
				t.Fatalf("offer shown:\n%s", body)
			}
		})
	}
}

// Why (#125): "Refresh identity" is where the admin learns the name changed, so the swapped card
// must carry the notice, not only a reload of the page.
func TestRefreshIdentityCardCarriesTheNameOffer(t *testing.T) {
	e := nameSetup(t)
	_, body := e.do(t, "POST", "/devices/1/probe", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
	if !strings.Contains(body, `Identity refreshed.`) || !strings.Contains(body, `Name on the device:`) || !strings.Contains(body, `Modbus Lüftung Test`) {
		t.Fatalf("refreshed card lacks the offer:\n%s", body)
	}
}

// Why (#125): "Use device name" adopts it through the inventory (validated, audited like any
// rename), and the header, breadcrumb and title follow in the same answer.
func TestAdoptTheDeviceName(t *testing.T) {
	e := nameSetup(t)
	resp, body := e.do(t, "POST", "/devices/1/name/adopt", "admin", nameForm("Modbus Lüftung Test"), "HX-Request", "true")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d:\n%s", resp.StatusCode, body)
	}
	if d := e.inv.devs[1]; d.Name != "Modbus Lüftung Test" {
		t.Fatalf("name %q", d.Name)
	}
	if strings.Contains(body, `Name on the device:`) || !strings.Contains(body, `<h1>Modbus Lüftung Test</h1>`) || !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Fatalf("answer does not show the new name:\n%s", body)
	}
}

// Why (#125): "Keep" must stay quiet only for the name the admin declined. When the device reports
// another one later, the notice comes back.
func TestKeepSilencesOnlyThatDeviceName(t *testing.T) {
	e := nameSetup(t)
	resp, body := e.do(t, "POST", "/devices/1/name/keep", "admin", nameForm("Modbus Lüftung Test"), "HX-Request", "true")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, `Name on the device:`) {
		t.Fatalf("status %d, offer still shown:\n%s", resp.StatusCode, body)
	}
	if d := e.inv.devs[1]; d.Name != "Fan" || d.KeptDeviceName != "Modbus Lüftung Test" {
		t.Fatalf("name %q kept %q", d.Name, d.KeptDeviceName)
	}
	// the decision is in the audit log: who declined which name
	var kept *model.AuditEvent
	for _, ev := range e.aud.events {
		if ev.Action == "device.name_kept" {
			kept = &ev
		}
	}
	if kept == nil || kept.Target != "device:1" || !strings.Contains(kept.Detail, "Modbus Lüftung Test") || kept.Result != "ok" {
		t.Fatalf("no device.name_kept audit event: %+v", e.aud.events)
	}
	_, body = e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(body, `Name on the device:`) {
		t.Fatal("the page offers the declined name again")
	}
	d := e.inv.devs[1]
	d.DeviceName = "Modbus Lüftung Büro" // renamed on the device again
	e.inv.devs[1] = d
	_, body = e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, `Name on the device:`) || !strings.Contains(body, `Modbus Lüftung Büro`) {
		t.Fatalf("a new device name must be offered:\n%s", body)
	}
}

// Why (#125): the answer belongs to the name the admin saw. If the device reports another one by
// the time the button is pressed, nothing changes and the card shows the new name to decide on.
func TestNameAnswersAreRefusedForAStaleName(t *testing.T) {
	for _, action := range []string{"adopt", "keep"} {
		t.Run(action, func(t *testing.T) {
			e := nameSetup(t)
			_, body := e.do(t, "POST", "/devices/1/name/"+action, "admin", nameForm("Older name"), "HX-Request", "true")
			d := e.inv.devs[1]
			if d.Name != "Fan" || d.KeptDeviceName != "" {
				t.Fatalf("a stale answer changed the device: %+v", d)
			}
			if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, `Modbus Lüftung Test`) {
				t.Fatalf("no explanation and fresh offer:\n%s", body)
			}
		})
	}
}

// Why (#125): renaming is an admin edit. An operator's answer is refused before anything is read.
func TestOperatorsCannotAnswerTheNameOffer(t *testing.T) {
	e := nameSetup(t)
	for _, action := range []string{"adopt", "keep"} {
		resp, _ := e.do(t, "POST", "/devices/1/name/"+action, "operator", nameForm("Modbus Lüftung Test"), "HX-Request", "true")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", action, resp.StatusCode)
		}
	}
	if d := e.inv.devs[1]; d.Name != "Fan" || d.KeptDeviceName != "" {
		t.Fatalf("%+v", d)
	}
}

// Why (#125 review): "Use device name" must not undo an edit made between reading the page and the
// click. It writes only if the stored device is still the one it read (UpdateIf); otherwise nothing
// changes and the card shows what is stored now.
func TestAdoptDoesNotOverwriteAConcurrentEdit(t *testing.T) {
	e := nameSetup(t)
	e.inv.beforeUpdateIf = func() {
		d := e.inv.devs[1]
		d.Notes = "edited meanwhile"
		e.inv.devs[1] = d
	}
	_, body := e.do(t, "POST", "/devices/1/name/adopt", "admin", nameForm("Modbus Lüftung Test"), "HX-Request", "true")
	d := e.inv.devs[1]
	if d.Name != "Fan" || d.Notes != "edited meanwhile" {
		t.Fatalf("the concurrent edit was lost or the name written: %+v", d)
	}
	if !strings.Contains(body, `role="alert"`) || !strings.Contains(body, "changed meanwhile") {
		t.Fatalf("no conflict message:\n%s", body)
	}
}
