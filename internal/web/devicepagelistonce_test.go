package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// onceFacts is a facts source with all three reads of the device page. Like app's webFacts, every call stands for one
// ListDevices of the inventory, which is what the test counts: DeviceFacts must be that one read for the whole render.
type onceFacts struct {
	lists     *atomic.Int32
	facts     transport.Facts
	conflicts []transport.TopicConflict
	err       error // DeviceFacts fails (the inventory cannot be read)
}

func (s onceFacts) Facts(context.Context, model.DeviceID) (transport.Facts, error) {
	s.lists.Add(1)
	return s.facts, nil
}

func (s onceFacts) TopicCheck(context.Context) (transport.TopicCheck, error) {
	s.lists.Add(1)
	return transport.TopicCheck{ByDevice: map[model.DeviceID][]transport.TopicConflict{1: s.conflicts}}, nil
}

func (s onceFacts) DeviceFacts(context.Context, model.Device) (transport.Facts, []transport.TopicConflict, error) {
	s.lists.Add(1)
	if s.err != nil {
		return transport.Facts{}, nil, s.err
	}
	return s.facts, s.conflicts, nil
}

const (
	pathsCardText = "How TasmoFleet reaches this device"
	topicHintMark = `id="topic-hint"`
)

func onceSetup(t *testing.T, src onceFacts, mutate func(*Deps)) *env {
	t.Helper()
	return setupWith(t, func(d *Deps) {
		topicDevices(d)
		d.DeviceNames = nil
		d.Transport = src
		if mutate != nil {
			mutate(d)
		}
	})
}

func collisionOf1() []transport.TopicConflict { return collisionCheck().Of(1) }

// Why (#1151): the device page listed the whole inventory twice per render, once for the router's facts and once for
// the topic hint; the live tick repeats the render every few seconds for every open page. The page reads both from one
// list, on every path that renders the Identity card: the page, the live tick, Refresh identity and the name answers.
func TestDevicePageListsTheInventoryOncePerRender(t *testing.T) {
	cases := []struct {
		name, method, path string
		form               url.Values
	}{
		{"page", "GET", "/devices/1", nil},
		{"live tick", "GET", "/devices/1/live", nil},
		{"refresh identity", "POST", "/devices/1/probe", url.Values{"csrf": {"tok"}}},
		{"keep the device name", "POST", "/devices/1/name/keep", url.Values{"csrf": {"tok"}, "name": {"x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lists := &atomic.Int32{}
			now := time.Now()
			src := onceFacts{lists: lists, facts: mqttFacts(now, model.MQTTPasswordOwn), conflicts: collisionOf1()}
			e := onceSetup(t, src, func(d *Deps) { d.DeviceNames = fakeNames{inv: d.Inventory.(*fakeInv)} })
			e.inv.devs[1] = func() model.Device { d := e.inv.devs[1]; d.DeviceName = "x"; return d }()
			lists.Store(0) // setup reads nothing; the count starts with the request
			resp, body := e.do(t, tc.method, tc.path, "admin", tc.form, "HX-Request", "true")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d:\n%s", resp.StatusCode, body)
			}
			if n := lists.Load(); n != 1 {
				t.Errorf("the render read the inventory %d times, want once", n)
			}
			if tc.name == "page" {
				for _, want := range []string{pathsCardText, topicHintMark} {
					if !strings.Contains(body, want) {
						t.Errorf("page lacks %q", want)
					}
				}
			}
		})
	}
}

// Why (#1151): with one read, a list that cannot be read costs both the Paths card and the hint together (before: each
// read failed alone). The page itself still answers: a failed read costs the card, never the page. The facts source here
// would answer Facts fine, so a page that went back to reading Facts apart would show the card and fail this test.
func TestDevicePageWithoutDeviceFactsShowsNeitherPathsNorHint(t *testing.T) {
	lists := &atomic.Int32{}
	src := onceFacts{lists: lists, facts: mqttFacts(time.Now(), model.MQTTPasswordOwn), conflicts: collisionOf1(), err: errors.New("disk gone")}
	e := onceSetup(t, src, nil)
	resp, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if strings.Contains(body, pathsCardText) || strings.Contains(body, topicHintMark) {
		t.Errorf("the page shows the Paths card or the hint although the inventory could not be read")
	}
	if n := lists.Load(); n != 1 {
		t.Errorf("a failed read was repeated: %d reads", n)
	}
}

// Why (#1047): a device without an MQTT topic never gets a topic hint; the card of the check does not list it either.
// DeviceFacts computes the check for every device, so the page keeps that rule itself.
func TestDevicePageGivesNoTopicHintToADeviceWithoutATopic(t *testing.T) {
	lists := &atomic.Int32{}
	src := onceFacts{lists: lists, facts: mqttFacts(time.Now(), model.MQTTPasswordOwn), conflicts: collisionOf1()}
	e := onceSetup(t, src, func(d *Deps) {
		d.Inventory = newInv(model.Device{ID: 1, Name: "kitchen", Address: "10.0.0.1", MQTT: model.MQTTOurs})
	})
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if strings.Contains(body, topicHintMark) {
		t.Error("a device without a Topic shows a topic hint")
	}
	if !strings.Contains(body, pathsCardText) {
		t.Error("the Paths card is missing")
	}
}
