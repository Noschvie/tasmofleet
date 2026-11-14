package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// #810: a check vouches for the transport its read used. The reads of the Settings tab (a page open) and of the
// policy poller go through the router without a pinned transport, so a read can fall back to HTTP (MQTT published
// nothing, or published and got no answer) and be answered by whoever holds the address now, while the identity
// check after it goes over MQTT and is answered by the stored device on its topic; or the mirror: the reads go to
// the topic and the check falls back to the address. Either way the check would vouch for a transport no read used,
// and another device's values would be shown on the Settings tab, judged by the policy report and become what
// Make compliant writes. These tests drive both callers unpinned through the real router, as the pages and the
// poller call them (refreshOverScript wires VerifyRead as production does).

// readFallbacks are the two ways a read over MQTT goes to HTTP: MQTT published nothing, or nobody answered.
var readFallbacks = map[string]error{"not published": tasmota.ErrUnreachable, "not answered": tasmota.ErrTimeout}

// Why (#810): a Settings read whose Status 3 fell back to HTTP was answered there by another device (another MAC at
// the stored address); the check after the read, routed MQTT first, was answered by the stored device on its topic
// and let the read through. The check must also be asked where the read went: the read is refused, and the Settings
// tab remembers nothing of it.
func TestAPageSettingsReadAnsweredOverHTTPByAnotherDeviceIsRefused(t *testing.T) {
	for name, fallback := range readFallbacks {
		t.Run(name, func(t *testing.T) {
			mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(storedMAC), nil }, other: answers("Status 3", fallback)}
			g := refreshOverScript(t, mqtt, otherMAC)
			if _, err := g.set.Read(t.Context(), 1); !errors.Is(err, errAnotherDevice) {
				t.Errorf("Settings read = %v, want errAnotherDevice", err)
			}
			if snap, ok := g.set.Last(1); ok {
				t.Errorf("the Settings tab remembers %d values read from another device", len(snap.Values))
			}
			if g.http.n.Load() == 0 {
				t.Fatal("no read went over HTTP: the test does not show the fallback it is about")
			}
		})
	}
}

// Why (#810): the same gap in the policy poller (and the on-demand live check, which reads the same way): the rule's
// value came over HTTP from another device, the check over MQTT from the stored one. Cached, it would be judged in
// the policy report as this device's value and drive what Make compliant writes. The read fails (the poller looks
// at the device again soon) and nothing is cached.
func TestAPolicyReadAnsweredOverHTTPByAnotherDeviceIsRefused(t *testing.T) {
	for name, fallback := range readFallbacks {
		t.Run(name, func(t *testing.T) {
			mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(storedMAC), nil }, other: answers("Status", fallback)}
			g := refreshOverScript(t, mqtt, otherMAC)
			if g.rep.readDevice(t.Context(), g.d) {
				t.Error("a policy read answered over HTTP by another device is reported as successful")
			}
			if v := g.rep.vals.get(1); len(v) != 0 {
				t.Errorf("the policy values hold %v read from another device", v)
			}
			if g.http.n.Load() == 0 {
				t.Fatal("no read went over HTTP: the test does not show the fallback it is about")
			}
		})
	}
}

// Why (#810, the mirror): every read of the Settings tab went to the topic, where any device listening on it
// answers, and the check after them fell back to HTTP, where the stored device holds the address and showed its
// MAC. That check says nothing about who answered on the topic: the read is refused and nothing is remembered.
func TestAPageSettingsReadOverMQTTIsNotVouchedForByAnHTTPCheck(t *testing.T) {
	for name, fallback := range readFallbacks {
		t.Run(name, func(t *testing.T) {
			mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return nil, fallback }, other: answers("", nil)}
			g := refreshOverScript(t, mqtt, storedMAC)
			if _, err := g.set.Read(t.Context(), 1); err == nil {
				t.Error("Settings read = nil: a read over MQTT was vouched for by a check over HTTP")
			}
			if snap, ok := g.set.Last(1); ok {
				t.Errorf("the Settings tab remembers %d values no check over MQTT vouched for", len(snap.Values))
			}
			if mqtt.nOther.Load() == 0 {
				t.Fatal("no read went over MQTT: the test does not show the case it is about")
			}
		})
	}
}

// Why (#810, the mirror): the same for the policy poller.
func TestAPolicyReadOverMQTTIsNotVouchedForByAnHTTPCheck(t *testing.T) {
	for name, fallback := range readFallbacks {
		t.Run(name, func(t *testing.T) {
			mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return nil, fallback }, other: answers("", nil)}
			g := refreshOverScript(t, mqtt, storedMAC)
			if g.rep.readDevice(t.Context(), g.d) {
				t.Error("a policy read over MQTT vouched for by a check over HTTP is reported as successful")
			}
			if v := g.rep.vals.get(1); len(v) != 0 {
				t.Errorf("the policy values hold %v no check over MQTT vouched for", v)
			}
			if mqtt.nOther.Load() == 0 {
				t.Fatal("no read went over MQTT: the test does not show the case it is about")
			}
		})
	}
}

// Why (#810): the rule must not cost the reads it guards. A Settings read that used both transports, each answered
// by the stored device, is remembered: the check is asked once over each, and both vouch.
func TestAReadOverBothTransportsIsCheckedOverBoth(t *testing.T) {
	mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(storedMAC), nil }, other: answers("Status 3", tasmota.ErrUnreachable)}
	g := refreshOverScript(t, mqtt, storedMAC)
	if _, err := g.set.Read(t.Context(), 1); err != nil {
		t.Fatalf("Settings read = %v", err)
	}
	if _, ok := g.set.Last(1); !ok {
		t.Error("the stored device's values were not remembered")
	}
	if n := mqtt.n5.Load(); n != 1 {
		t.Errorf("%d x Status 5 over MQTT, want 1", n)
	}
	if n := g.http.status.Load(); n != 1 {
		t.Errorf("%d x Status 5 over HTTP, want 1: a read went there, so the check must too", n)
	}
}

// Why (#810): the rule cannot be dodged by the caller. A check without the trace of its read does not know which
// transport to vouch for, and a prober without the router can only ask over HTTP: neither may pass a read that went
// over MQTT on the strength of an HTTP answer. Both refuse, although the device at the address is the stored one.
func TestACheckThatCannotFollowItsReadRefuses(t *testing.T) {
	mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(storedMAC), nil }, other: answers("", nil)}
	g := refreshOverScript(t, mqtt, storedMAC)
	rctx, read := transport.WithTrace(t.Context())
	if _, err := g.set.ReadKeys(rctx, 1, []string{"Topic"}); err != nil {
		t.Fatal(err)
	}
	if via := read.Via(); via != "mqtt" {
		t.Fatalf("the read went over %s, want mqtt", via)
	}
	httpOnly := &Prober{Client: g.http, Gap: time.Millisecond}
	if err := httpOnly.VerifyRead(t.Context(), g.d, read); err == nil {
		t.Error("a prober without the router vouched over HTTP for a read over MQTT")
	}
	if err := httpOnly.VerifyRead(t.Context(), g.d, nil); err == nil {
		t.Error("a check without the trace of its read passed")
	}
	if n := g.http.status.Load(); n != 0 {
		t.Errorf("%d x Status 5 over HTTP: a check that cannot vouch must not be asked", n)
	}
}

// Why (#810, review): a read MQTT published and nobody answered brought no value over MQTT. A device whose MQTT
// replies do not arrive (the case the router's unanswered mark is for, #456) has its read time out over MQTT once
// and answered over HTTP by the stored device. The check must be asked where the values came from, over HTTP: asked
// over MQTT too it waits out a second timeout and refuses the whole read, and the Settings tab and the poller fail
// once every unanswered window for a device that answers fine at its address.
func TestAReadMQTTLeftUnansweredIsCheckedWhereItWasAnswered(t *testing.T) {
	quiet := func() *scriptedMQTT {
		return &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return nil, tasmota.ErrTimeout },
			other: func(string) (json.RawMessage, error) { return nil, tasmota.ErrTimeout }}
	}
	t.Run("settings", func(t *testing.T) {
		g := refreshOverScript(t, quiet(), storedMAC)
		if _, err := g.set.Read(t.Context(), 1); err != nil {
			t.Fatalf("Settings read = %v: the check was asked over MQTT, which answered no read", err)
		}
		if snap, ok := g.set.Last(1); !ok || len(snap.Values) == 0 {
			t.Error("the stored device's values, answered over HTTP, were not remembered")
		}
	})
	t.Run("policy", func(t *testing.T) {
		g := refreshOverScript(t, quiet(), storedMAC)
		if !g.rep.readDevice(t.Context(), g.d) {
			t.Fatal("a policy read the stored device answered over HTTP failed")
		}
		if v := g.rep.vals.get(1); v["PowerOnState"].value != "3" {
			t.Errorf("policy values = %v, want PowerOnState 3", v)
		}
	})
}

// Why (#810, security check): a check handed a trace that recorded no read (the caller opened the trace but read
// with another context) does not know where the values came from. Read as HTTP, it would be answered by the stored
// device at the address while the values came from whoever listens on the topic. Through the router it refuses,
// and asks nothing.
func TestACheckOfATraceThatRecordedNoReadRefuses(t *testing.T) {
	mqtt := &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(otherMAC), nil }, other: answers("", nil)}
	g := refreshOverScript(t, mqtt, storedMAC)
	_, read := transport.WithTrace(t.Context())
	if _, err := g.set.ReadKeys(t.Context(), 1, []string{"Topic"}); err != nil { // not the traced context
		t.Fatal(err)
	}
	if mqtt.nOther.Load() == 0 {
		t.Fatal("the read did not go over MQTT: the test does not show the case it is about")
	}
	if err := g.a.svc.Prober.VerifyRead(t.Context(), g.d, read); err == nil {
		t.Error("a check with an empty trace vouched for a read")
	}
	if n := g.http.status.Load(); n != 0 {
		t.Errorf("%d x Status 5 over HTTP for a trace that holds no read", n)
	}
}

// Why (#810, security check): ReadNet's Status 5 is its check, and its Status 11 must go where that check was
// answered. Over MQTT the stored device answered Status 5; Status 11, left to the router, fell back to HTTP, where
// another device holds the address now, and that device's Wi-Fi (SSID, BSSID, channel) was stored on this one.
func TestReadNetAsksStatus11WhereStatus5WasAnswered(t *testing.T) {
	for name, fallback := range readFallbacks {
		t.Run(name, func(t *testing.T) {
			p, st, id, h := readNetOverRouter(t, &scriptedMQTT{status5: func(int) (json.RawMessage, error) { return mac5(storedMAC), nil },
				other: func(string) (json.RawMessage, error) { return nil, fallback }})
			if err := p.ReadNet(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			d, err := st.GetDevice(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if d.Net.SSID == "other-net" {
				t.Errorf("the Wi-Fi of the device at the address was stored: %+v", d.Net)
			}
			if n := h.n.Load(); n != 0 {
				t.Errorf("%d commands over HTTP, where no check was answered", n)
			}
		})
	}
}

// otherWifi is the device that holds the address over HTTP: another Tasmota on another network.
type otherWifi struct {
	tasmota.Client
	n atomic.Int32
}

func (h *otherWifi) Command(context.Context, tasmota.Target, string) (json.RawMessage, error) {
	h.n.Add(1)
	return json.RawMessage(`{"StatusSTS":{"Wifi":{"SSId":"other-net","BSSId":"AA:BB:CC:DD:EE:FF","Channel":11}},"StatusNET":{"Mac":"` + otherMAC + `"}}`), nil
}

// readNetOverRouter stores a device on our broker (stored MAC storedMAC) and returns a prober whose background
// reads go through the real router, with mqtt as its command channel and otherWifi at the address.
func readNetOverRouter(t *testing.T, mqtt *scriptedMQTT) (*Prober, *store.SQLite, model.DeviceID, *otherWifi) {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	id, err := st.CreateDevice(t.Context(), model.Device{Name: "plug", Topic: "plug", Address: "192.0.2.1", MQTT: model.MQTTOurs, MAC: storedMAC, MACs: []string{storedMAC}})
	if err != nil {
		t.Fatal(err)
	}
	h := &otherWifi{}
	router := transport.NewRouter(transport.Options{HTTP: h, Devices: st})
	router.SetFacts(&TransportFacts{Devices: st, Health: fakeFactsHealth{id: {LWT: "Online"}}, Memory: router.Memory(), Unanswered: router.UnansweredReads(),
		BusConnected: func() bool { return true }, FullTopic: func() string { return "%prefix%/%topic%/" }, FirmwareBaseURL: func() string { return "" },
		BrokerPassword: func() string { return "" }, Now: time.Now})
	router.SetMQTT(mqtt)
	return &Prober{Client: router.HTTPOnly(), Reads: router, Devices: st, Gap: time.Millisecond}, st, id, h
}
