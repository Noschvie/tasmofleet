package web

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/backup"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// A device HTTP cannot reach, on our broker, with a password: the router takes the dump over MQTT.
type noteHTTP struct{}

func (noteHTTP) Command(context.Context, tasmota.Target, string) (json.RawMessage, error) {
	return nil, tasmota.ErrUnreachable
}
func (noteHTTP) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	return nil, tasmota.ErrUnreachable
}
func (noteHTTP) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, tasmota.ErrUnreachable
}

type noteMQTT struct{}

func (noteMQTT) Do(context.Context, model.Device, string) (json.RawMessage, error) { return nil, nil }
func (noteMQTT) DoLoose(context.Context, model.Device, string) (json.RawMessage, error) {
	return nil, nil
}
func (noteMQTT) Connected() bool { return true }
func (noteMQTT) Download(context.Context, model.Device, string) ([]byte, error) {
	return []byte("DUMP"), nil
}

type noteDevices struct{}

func (noteDevices) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	return model.Device{ID: id, Topic: "plug", MQTT: model.MQTTOurs, MQTTPasswordMode: model.MQTTPasswordOwn, MQTTPassword: "pw"}, nil
}

type noteFacts struct{}

func (noteFacts) HTTPState(model.DeviceID) transport.HTTPState {
	return transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailUnreachable, Since: time.Now()}
}
func (noteFacts) FactsFor(_ context.Context, d model.Device) (transport.Facts, error) {
	now := time.Now()
	return transport.Facts{Now: now, HTTP: noteFacts{}.HTTPState(d.ID),
		MQTT: transport.MQTTFacts{BusConnected: true, Ours: true, Topic: d.Topic, TopicExclusive: true, LWTOnline: true,
			GroupTopic: "tasmotas", GroupTopicReadAt: now, LastProbeAt: now.Add(-time.Hour)},
		Password: transport.Password{Mode: d.MQTTPasswordMode}}, nil
}

// routedBackups is a backup service whose dump goes through a real router (when there is one), so the
// trace of the request records which transport carried the run, as the engine's bulk backup does.
type routedBackups struct {
	*fakeBackups
	r     *transport.Router
	kinds []model.BackupKind
}

func (b routedBackups) Backup(ctx context.Context, _ string, id model.DeviceID) ([]model.Backup, error) {
	if b.r != nil {
		if _, err := b.r.DownloadConfig(ctx, tasmota.TargetFor(model.Device{ID: id, Address: "192.0.2.1"})); err != nil {
			return nil, err
		}
	}
	out := make([]model.Backup, 0, len(b.kinds))
	for i, k := range b.kinds {
		out = append(out, model.Backup{ID: model.BackupID(i + 1), DeviceID: id, Kind: k})
	}
	return out, nil
}

func mqttRouter() *transport.Router {
	r := transport.NewRouter(transport.Options{HTTP: noteHTTP{}, Devices: noteDevices{}, Facts: noteFacts{}})
	r.SetMQTT(noteMQTT{})
	return r
}

var allParts = []model.BackupKind{model.BackupConfigDump, model.BackupSnapshot, model.BackupDecoded, model.BackupFiles}

// Why (#882, asked for in #838): one Back up of one device reads the same on the device page as on the
// bulk result and the transaction page: one backup, its parts named, the way it travelled. The device
// page answered only "Backup taken." however the run went, so the person who pressed the button could
// not see over MQTT that nothing went over HTTP, nor what the backup holds. Both buttons are covered:
// the header's (every tab) and the empty Backups tab's.
func TestBackUpOnTheDevicePageSaysWhatTheBackupHolds(t *testing.T) {
	for _, tc := range []struct {
		name, form, flash string
		redirectBase      string
	}{
		{"header", "", "ok=backup-taken", "/tf/devices/1"},
		{"backups tab", "tab", "ok=backed-up", "/tf/devices/1/backups"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupWith(t, func(d *Deps) {
				d.Backups = routedBackups{fakeBackups: &fakeBackups{}, r: mqttRouter(), kinds: allParts}
			})
			form := url.Values{"csrf": {"tok"}}
			if tc.form != "" {
				form.Set("from", tc.form)
			}
			resp, _ := e.do(t, "POST", "/devices/1/backup", "admin", form, "HX-Request", "true")
			loc := resp.Header.Get("HX-Redirect")
			u, err := url.Parse(loc)
			if err != nil || u.Path != tc.redirectBase || !strings.Contains(u.RawQuery, tc.flash) {
				t.Fatalf("redirect %q", loc)
			}
			_, page := e.do(t, "GET", strings.TrimPrefix(loc, "/tf"), "admin", nil)
			if want := "Backup taken over MQTT (config, status, decoded, files)."; !strings.Contains(page, want) {
				t.Errorf("the page lacks %q:\n%s", want, page)
			}
		})
	}
}

// Why: a run that no router recorded does not claim MQTT. Saying "over MQTT" without the proof would
// make the sentence a guess; the plain HTTP path says what it was.
func TestBackUpWithoutARoutedPathDoesNotClaimMQTT(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Backups = routedBackups{fakeBackups: &fakeBackups{}, kinds: allParts} })
	resp, _ := e.do(t, "POST", "/devices/1/backup", "admin", url.Values{"csrf": {"tok"}}, "HX-Request", "true")
	_, page := e.do(t, "GET", strings.TrimPrefix(resp.Header.Get("HX-Redirect"), "/tf"), "admin", nil)
	if strings.Contains(page, "over MQTT") || !strings.Contains(page, "Backup taken over HTTP (config, status, decoded, files).") {
		t.Errorf("a run without a recorded MQTT path must not claim it:\n%s", page)
	}
}

// Why: the note rides in the address of the redirect, so a crafted link must never show a sentence of
// its own. Only the fixed words of backup.Note can appear; whatever else the values say is dropped.
func TestBackUpFlashNeverEchoesTheQuery(t *testing.T) {
	e := setup(t)
	for _, path := range []string{
		"/devices/1?ok=backup-taken&parts=bogus&via=x",
		"/devices/1/backups?ok=backed-up&parts=%3Cb%3Eowned%3C/b%3E&via=%3Cb%3Eowned%3C/b%3E",
		"/devices/1?ok=backup-taken&parts=dmp,%3Cb%3Eowned%3C/b%3E,snapshot&via=ftp",
	} {
		_, page := e.do(t, "GET", path, "admin", nil)
		if strings.Contains(page, "owned") || strings.Contains(page, "bogus") {
			t.Errorf("%s echoes the query:\n%s", path, page)
		}
		if !strings.Contains(page, "Backup taken") {
			t.Errorf("%s lost the confirmation", path)
		}
	}
	_, page := e.do(t, "GET", "/devices/1?ok=backup-taken&parts=bogus&via=x", "admin", nil)
	if !strings.Contains(page, "Backup taken.") {
		t.Errorf("without a known part the sentence stays plain:\n%s", page)
	}
	// a known part next to an unknown one and an unknown way: only the known words come through
	_, page = e.do(t, "GET", "/devices/1?ok=backup-taken&parts=dmp,%3Cb%3Eowned%3C/b%3E,snapshot&via=ftp", "admin", nil)
	if !strings.Contains(page, "Backup taken (config, status).") {
		t.Errorf("the known parts must be worded and the rest dropped:\n%s", page)
	}
}

// Why (#882, live test finding 2): the Activity box of the device page showed "Transaction #1 Succeeded
// Back up" and nothing else, while the bulk result and the transaction page of the same job say how the
// backup went. The note is the detail of the job's last event; the box reads it for succeeded backup
// jobs, and leaves every other row as it was.
func TestActivityShowsTheBackupNote(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	note := backup.Note(allParts, "mqtt")
	jobs := []model.Job{
		{ID: 2, BatchID: 2, DeviceID: 1, Kind: model.KindBackup, State: model.JobFailed, LastError: "tasmota: unreachable", UpdatedAt: at.Add(time.Hour)},
		{ID: 1, BatchID: 1, DeviceID: 1, Kind: model.KindBackup, State: model.JobSucceeded, UpdatedAt: at},
	}
	fj := &fakeJobs{jobs: jobs, events: map[model.JobID][]model.JobEvent{
		1: {{Event: "backup_started"}, {Event: "succeeded", Detail: note}},
		2: {{Event: "failed", Detail: "failed detail must stay out"}},
	}}
	e := setupWith(t, func(d *Deps) { d.Jobs, d.OTA = fj, &fakeOTA{} })
	_, body := e.do(t, "GET", "/devices/1", "admin", nil)
	if !strings.Contains(body, "Backup taken over MQTT (config, status, decoded, files)") {
		t.Errorf("the Activity box lacks the note:\n%s", body)
	}
	if strings.Contains(body, "failed detail must stay out") {
		t.Error("a failed job shows its own reason column, not the event detail")
	}
	if !strings.Contains(body, "Transaction #2") {
		t.Error("the failed row disappeared")
	}
}
