package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// dumpPathRig is a real transport.Router in front of a device that is live on TasmoFleet's broker, so
// that reads would go MQTT first (#456 A). It records which transport carried each call.
type dumpPathRig struct {
	mu         sync.Mutex
	dev        model.Device
	httpFails  bool  // HTTP is known to fail: the dump goes over MQTT (FileDownload)
	noPassword bool  // no MQTT password: MQTT cannot carry the dump, HTTP (/dl) does
	dlErr      error // the dump fails with this error on either transport
	statusLost bool  // MQTT does not answer Status 11 (the evidence read after a failed dump)
	http, mqtt []string
}

func (g *dumpPathRig) GetDevice(context.Context, model.DeviceID) (model.Device, error) {
	return g.dev, nil
}

func (g *dumpPathRig) note(list *[]string, c string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	*list = append(*list, c)
}

func dumpPathReply(c string) json.RawMessage {
	switch {
	case c == "Status 2":
		return json.RawMessage(`{"StatusFWR":{"Version":"16.0.0(tasmota)"}}`)
	case strings.HasPrefix(c, "Status "):
		return json.RawMessage(`{"StatusX":{}}`)
	case c == "Template":
		return json.RawMessage(`{"NAME":"Generic","GPIO":[0],"FLAG":0,"BASE":18}`)
	}
	return json.RawMessage(`{"` + c + `":"OFF"}`)
}

type dumpPathHTTP dumpPathRig

func (h *dumpPathHTTP) Command(_ context.Context, _ tasmota.Target, c string) (json.RawMessage, error) {
	(*dumpPathRig)(h).note(&h.http, c)
	return dumpPathReply(c), nil
}
func (h *dumpPathHTTP) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	(*dumpPathRig)(h).note(&h.http, "/dl")
	if h.dlErr != nil {
		return nil, h.dlErr
	}
	return []byte("DMP-FAKE-dump-path"), nil
}
func (h *dumpPathHTTP) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, nil
}

type dumpPathMQTT dumpPathRig

func (m *dumpPathMQTT) Do(_ context.Context, _ model.Device, c string) (json.RawMessage, error) {
	(*dumpPathRig)(m).note(&m.mqtt, c)
	if m.statusLost && c == "Status 11" {
		return nil, tasmota.ErrTimeout
	}
	return dumpPathReply(c), nil
}
func (m *dumpPathMQTT) DoLoose(ctx context.Context, d model.Device, c string) (json.RawMessage, error) {
	return m.Do(ctx, d, c)
}
func (m *dumpPathMQTT) Connected() bool { return true }
func (m *dumpPathMQTT) Download(context.Context, model.Device, string) ([]byte, error) {
	(*dumpPathRig)(m).note(&m.mqtt, "FileDownload")
	if m.dlErr != nil {
		return nil, m.dlErr
	}
	return []byte("DMP-FAKE-dump-path"), nil
}

type dumpPathFacts dumpPathRig

func (f *dumpPathFacts) HTTPState(model.DeviceID) transport.HTTPState {
	if f.httpFails {
		return transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailUnreachable, Since: time.Now()}
	}
	return transport.HTTPState{}
}
func (f *dumpPathFacts) FactsFor(_ context.Context, d model.Device) (transport.Facts, error) {
	now := time.Now()
	return transport.Facts{Now: now, HTTP: f.HTTPState(d.ID), MQTT: transport.MQTTFacts{
		BusConnected: true, Ours: true, Topic: d.Topic, TopicExclusive: true, LWTOnline: true,
		GroupTopic: "tasmotas", GroupTopicReadAt: now, LastProbeAt: now.Add(-time.Hour),
	}, Password: f.password()}, nil
}

func (f *dumpPathFacts) password() transport.Password {
	if f.noPassword {
		return transport.Password{Mode: model.MQTTPasswordNone}
	}
	return transport.Password{Mode: model.MQTTPasswordOwn}
}

// Why: the dump and the snapshot of a backup run never mix transports. Since reads go MQTT first (#456 A), the status snapshot
// would be read over MQTT right after a dump over HTTP (and the audit "via" would say http+mqtt);
// the snapshot follows the dump: HTTP after an HTTP dump (MQTT cannot carry it: no password), MQTT after a dump
// over MQTT, first (#607) or because HTTP fails.
func TestBackupSnapshotFollowsTheTransportOfTheDump(t *testing.T) {
	for _, tc := range []struct {
		name       string
		httpFails  bool
		noPassword bool
		wantHTTP   bool
	}{
		{"dump over HTTP: snapshot over HTTP", false, true, true},
		{"dump over MQTT first: snapshot over MQTT", false, false, false},
		{"dump over MQTT, HTTP fails: snapshot over MQTT", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &dumpPathRig{httpFails: tc.httpFails, noPassword: tc.noPassword, dev: model.Device{
				ID: 7, Name: "kitchen", Address: "172.31.13.28", Topic: "kitchen", MQTT: model.MQTTOurs,
				MQTTPasswordMode: model.MQTTPasswordOwn, MQTTPassword: "fake-mqtt-pass",
			}}
			r := transport.NewRouter(transport.Options{HTTP: (*dumpPathHTTP)(g), Devices: g, Facts: (*dumpPathFacts)(g)})
			r.SetMQTT((*dumpPathMQTT)(g))
			e := newEnv(t, func(o *Options) { o.Client, o.Devices = r, devs{g.dev} })
			if _, err := e.m.Backup(t.Context(), "alice", 7); err != nil {
				t.Fatal(err)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			want := len(snapshotCommands) + 1 // the dump and every snapshot command
			if tc.wantHTTP && (len(g.http) != want || len(g.mqtt) != 0) {
				t.Fatalf("http=%v mqtt=%v: the run mixed transports", g.http, g.mqtt)
			}
			// After a dump over MQTT the files step asks the device on its topic whether it has a file system (#855):
			// that question follows the dump too.
			if !tc.wantHTTP && (len(g.mqtt) != want+1 || g.mqtt[want] != "UfsType" || len(g.http) != 0) {
				t.Fatalf("http=%v mqtt=%v: the run mixed transports", g.http, g.mqtt)
			}
		})
	}
}

// Why (#887, transport rule): the evidence read after a failed dump goes over the transport that carried the
// dump: MQTT after a dump over MQTT (never HTTP, which is what can starve an ESP8266 during a transfer), HTTP
// only after a dump over HTTP (MQTT cannot carry the device). One Status 11, nothing else: a Status 11 MQTT leaves
// unanswered after an MQTT dump does not go on over HTTP, as an unpinned read would once (T2), and the
// evidence would describe a device the dump's transport did not see.
func TestFailedDumpEvidenceFollowsTheTransportOfTheDump(t *testing.T) {
	for _, tc := range []struct {
		name       string
		httpFails  bool
		noPassword bool
		statusLost bool
		err        error
		wantHTTP   []string
		wantMQTT   []string
	}{
		{"dump over MQTT first", false, false, false, errEmptyChunk, nil, []string{"FileDownload", "Status 11"}},
		{"dump over MQTT, HTTP fails", true, false, false, errEmptyChunk, nil, []string{"FileDownload", "Status 11"}},
		{"dump over MQTT, Status 11 unanswered: no HTTP", false, false, true, errEmptyChunk, nil, []string{"FileDownload", "Status 11"}},
		{"dump over HTTP", false, true, false, fmt.Errorf("%w: HTTP 500", tasmota.ErrBadResponse), []string{"/dl", "Status 11"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &dumpPathRig{httpFails: tc.httpFails, noPassword: tc.noPassword, statusLost: tc.statusLost, dlErr: tc.err, dev: model.Device{
				ID: 7, Name: "kitchen", Address: "172.31.13.28", Topic: "kitchen", MQTT: model.MQTTOurs,
				MQTTPasswordMode: model.MQTTPasswordOwn, MQTTPassword: "fake-mqtt-pass",
			}}
			r := transport.NewRouter(transport.Options{HTTP: (*dumpPathHTTP)(g), Devices: g, Facts: (*dumpPathFacts)(g)})
			r.SetMQTT((*dumpPathMQTT)(g))
			e := newEnv(t, func(o *Options) { o.Client, o.Devices = r, devs{g.dev} })
			if _, err := e.m.Backup(t.Context(), "alice", 7); err == nil {
				t.Fatal("the backup must fail")
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if !slices.Equal(g.http, tc.wantHTTP) || !slices.Equal(g.mqtt, tc.wantMQTT) {
				t.Fatalf("http=%v mqtt=%v, want http=%v mqtt=%v", g.http, g.mqtt, tc.wantHTTP, tc.wantMQTT)
			}
		})
	}
}
