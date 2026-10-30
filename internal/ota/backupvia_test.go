package ota

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// A device HTTP cannot reach, on our broker, with a password: the router backs it up over MQTT.
type viaHTTP struct{}

func (viaHTTP) Command(context.Context, tasmota.Target, string) (json.RawMessage, error) {
	return nil, tasmota.ErrUnreachable
}
func (viaHTTP) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	return nil, tasmota.ErrUnreachable
}
func (viaHTTP) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, tasmota.ErrUnreachable
}

type viaMQTT struct{}

func (viaMQTT) Do(context.Context, model.Device, string) (json.RawMessage, error) { return nil, nil }
func (viaMQTT) DoLoose(context.Context, model.Device, string) (json.RawMessage, error) {
	return nil, nil
}
func (viaMQTT) Connected() bool { return true }
func (viaMQTT) Download(context.Context, model.Device, string) ([]byte, error) {
	return []byte("DUMP"), nil
}

type viaDevices struct{}

func (viaDevices) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	return model.Device{ID: id, Topic: "plug", MQTT: model.MQTTOurs, MQTTPasswordMode: model.MQTTPasswordOwn, MQTTPassword: "pw"}, nil
}

type viaFacts struct{}

func (viaFacts) HTTPState(model.DeviceID) transport.HTTPState {
	return transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailUnreachable, Since: time.Now()}
}
func (viaFacts) FactsFor(_ context.Context, d model.Device) (transport.Facts, error) {
	now := time.Now()
	return transport.Facts{Now: now, HTTP: viaFacts{}.HTTPState(d.ID),
		MQTT: transport.MQTTFacts{BusConnected: true, Ours: true, Topic: d.Topic, TopicExclusive: true, LWTOnline: true,
			GroupTopic: "tasmotas", GroupTopicReadAt: now, LastProbeAt: now.Add(-time.Hour)},
		Password: transport.Password{Mode: d.MQTTPasswordMode}}, nil
}

// routedBackups is a backup service whose dump comes through a real router.
type routedBackups struct{ r *transport.Router }

func (b routedBackups) Backup(ctx context.Context, _ string, id model.DeviceID) ([]model.Backup, error) {
	_, err := b.r.DownloadConfig(ctx, tasmota.TargetFor(model.Device{ID: id, Address: "192.0.2.1"}))
	return []model.Backup{{ID: 1}}, err
}

// Why (#257): the page of a bulk backup job says which way the dump came, so a person who sees a
// device backed up learns that it was over MQTT.
func TestBulkBackupJobNoteSaysItWasTakenOverMQTT(t *testing.T) {
	h, _, _, _ := backupHarness(t, 1)
	r := transport.NewRouter(transport.Options{HTTP: viaHTTP{}, Devices: viaDevices{}, Facts: viaFacts{}})
	r.SetMQTT(viaMQTT{})
	h.e.backups = routedBackups{r}
	if _, _, err := h.e.SubmitBackup(as("local"), "alice", []model.DeviceID{1}); err != nil {
		t.Fatal(err)
	}
	h.run(t, h.e)
	var note string
	h.st.mu.Lock()
	for _, ev := range h.st.events {
		if ev.To == model.JobSucceeded {
			note = ev.Detail
		}
	}
	h.st.mu.Unlock()
	if !strings.Contains(note, "over MQTT") {
		t.Fatalf("note %q", note)
	}
}
