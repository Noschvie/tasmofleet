package web

import (
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/inventory"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
)

// Why (#988): the edit form always saves the device and then the MQTT password. A save that changed only the
// MQTT password mode used to leave an empty device.update {"changed":null} row next to the password-mode
// entry: the log then claimed a device edit nobody made. The mode entry stays, and it never carries the
// credential (safety rule 4). A real edit in the same save is still audited as device.update with its field.
// It runs on the real inventory and store, because the empty row came from how they compare a device without
// tags; a fake inventory cannot show that.
func TestEditFormAuditsOnlyWhatChanged(t *testing.T) {
	const typed = "typed-own-pw-2468"
	for _, tc := range []struct {
		name        string
		form        url.Values
		wantUpdate  string // the "changed" list of the device.update row, empty when no row is wanted
		wantMode    model.MQTTPasswordMode
		wantStoredN int // device.mqtt_password rows
	}{
		{"only the mode", url.Values{"mqtt_password_mode_shown": {""}, "mqtt_password_mode": {"broker"}}, "", model.MQTTPasswordBroker, 1},
		{"a field and the mode", url.Values{"notes": {"by the door"}, "mqtt_password_mode_shown": {""}, "mqtt_password_mode": {"own"}, "mqtt_password": {typed}}, `{"changed":["notes"]}`, model.MQTTPasswordOwn, 1},
		{"only a field", url.Values{"notes": {"by the door"}}, `{"changed":["notes"]}`, model.MQTTPasswordNone, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			box, err := secretbox.LoadOrCreate(dir)
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(filepath.Join(dir, "tasmofleet.db"), box)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			aud := &fakeAudit{}
			inv, err := inventory.New(inventory.Options{Store: st, Audit: aud, Networks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}})
			if err != nil {
				t.Fatal(err)
			}
			id, err := inv.Create(t.Context(), "seed", model.Device{Name: "porch", Address: "10.0.0.7"}) // no tags, like the live case
			if err != nil {
				t.Fatal(err)
			}
			e := setupWith(t, func(d *Deps) { d.Inventory, d.MQTTPasswords, d.Recorder, d.Audit = inv, st, aud, aud })
			aud.mu.Lock()
			aud.events = nil // only what the save writes
			aud.mu.Unlock()

			post := editPost(tc.form)
			res, _ := e.do(t, "POST", "/devices/"+strconv.FormatInt(int64(id), 10), "admin", post)
			if res.StatusCode != 303 {
				t.Fatalf("status %d", res.StatusCode)
			}

			var updates, modes []string
			aud.mu.Lock()
			events := append([]model.AuditEvent(nil), aud.events...)
			aud.mu.Unlock()
			for _, ev := range events {
				switch ev.Action {
				case "device.update":
					updates = append(updates, ev.Detail)
				case "device.mqtt_password":
					modes = append(modes, ev.Detail)
				}
				if strings.Contains(ev.Detail, typed) {
					t.Fatalf("the typed MQTT password is in the audit row %s %s", ev.Action, ev.Detail)
				}
			}
			if tc.wantUpdate == "" && len(updates) != 0 {
				t.Fatalf("device.update rows %v for a save that changed no device field", updates)
			}
			if tc.wantUpdate != "" && (len(updates) != 1 || updates[0] != tc.wantUpdate) {
				t.Fatalf("device.update rows %v, want one %s", updates, tc.wantUpdate)
			}
			if len(modes) != tc.wantStoredN {
				t.Fatalf("device.mqtt_password rows %v, want %d", modes, tc.wantStoredN)
			}
			if tc.wantStoredN == 1 && !strings.Contains(modes[0], `"mode":"`+string(tc.wantMode)+`"`) {
				t.Fatalf("mode row %s, want mode %s", modes[0], tc.wantMode)
			}
			if got, _ := inv.Get(t.Context(), id); got.MQTTPasswordMode != tc.wantMode {
				t.Fatalf("stored mode %q, want %q", got.MQTTPasswordMode, tc.wantMode)
			}
		})
	}
}
