package backup

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// dumpPathRig is a real transport.Router in front of a device that is live on TasmoFleet's broker, so
// that reads would go MQTT first (#456 A). It records which transport carried each call.
type dumpPathRig struct {
	mu         sync.Mutex
	dev        model.Device
	httpFails  bool // HTTP is known to fail: the dump goes over MQTT (FileDownload)
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
	return []byte("DMP-FAKE-dump-path"), nil
}
func (h *dumpPathHTTP) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, nil
}

type dumpPathMQTT dumpPathRig

func (m *dumpPathMQTT) Do(_ context.Context, _ model.Device, c string) (json.RawMessage, error) {
	(*dumpPathRig)(m).note(&m.mqtt, c)
	return dumpPathReply(c), nil
}
func (m *dumpPathMQTT) DoLoose(ctx context.Context, d model.Device, c string) (json.RawMessage, error) {
	return m.Do(ctx, d, c)
}
func (m *dumpPathMQTT) Connected() bool { return true }
func (m *dumpPathMQTT) Download(context.Context, model.Device, string) ([]byte, error) {
	(*dumpPathRig)(m).note(&m.mqtt, "FileDownload")
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
	}, Password: transport.Password{Mode: model.MQTTPasswordOwn}}, nil
}

// Why: a backup run never mixes transports. Since reads go MQTT first (#456 A), the status snapshot
// would be read over MQTT right after a dump over HTTP (and the audit "via" would say http+mqtt);
// backups keep their order until stage 2 (457-C1). The snapshot follows the dump: HTTP after an HTTP
// dump, MQTT after a dump that HTTP could not carry.
func TestBackupSnapshotFollowsTheTransportOfTheDump(t *testing.T) {
	for _, tc := range []struct {
		name      string
		httpFails bool
		wantHTTP  bool
	}{
		{"dump over HTTP: snapshot over HTTP", false, true},
		{"dump over MQTT: snapshot over MQTT", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &dumpPathRig{httpFails: tc.httpFails, dev: model.Device{
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
			if !tc.wantHTTP && (len(g.mqtt) != want || len(g.http) != 0) {
				t.Fatalf("http=%v mqtt=%v: the run mixed transports", g.http, g.mqtt)
			}
		})
	}
}
