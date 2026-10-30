package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// upMQTT is the command channel with FileUpload: it records what the router hands it.
type upMQTT struct {
	fakeMQTT
	mu2       sync.Mutex
	uploads   int
	typ       int
	password  string
	data      []byte
	uploadErr error
	mac       string // what Status 5 reports; rigMAC when empty
	extraMAC  string // a second device on the topic answers Status 5 with this MAC
}

func (m *upMQTT) Upload(_ context.Context, _ model.Device, typ int, _ string, size int64, r io.Reader, pw string, _ func(int64)) error {
	m.mu2.Lock()
	defer m.mu2.Unlock()
	m.uploads++
	m.typ, m.password = typ, pw
	m.data, _ = io.ReadAll(r)
	if int64(len(m.data)) != size {
		return errors.New("size mismatch")
	}
	return m.uploadErr
}

type marked struct {
	mu  sync.Mutex
	ids []model.DeviceID
}

func (k *marked) MarkMQTTPasswordBad(_ context.Context, id model.DeviceID, _ time.Time) error {
	k.mu.Lock()
	k.ids = append(k.ids, id)
	k.mu.Unlock()
	return nil
}

// pwFacts adds the password facts the shared test fake does not carry.
type pwFacts struct {
	*facts
	pw Password
}

func (f pwFacts) FactsFor(ctx context.Context, d model.Device) (Facts, error) {
	x, err := f.facts.FactsFor(ctx, d)
	x.Password = f.pw
	return x, err
}

// Status 5 as the device answers it: the MAC the restore checked.
const rigMAC = "aa:bb:cc:dd:ee:01"

// DoAll is Do with the replies of a second device: extraMAC makes one more device answer.
func (m *upMQTT) DoAll(ctx context.Context, d model.Device, c string, _ time.Duration) ([]json.RawMessage, error) {
	raw, err := m.Do(ctx, d, c)
	if err != nil {
		return nil, err
	}
	out := []json.RawMessage{raw}
	if m.extraMAC != "" {
		out = append(out, json.RawMessage(`{"StatusNET":{"Mac":"`+m.extraMAC+`"}}`))
	}
	return out, nil
}

func (m *upMQTT) Do(_ context.Context, _ model.Device, c string) (json.RawMessage, error) {
	if c == "Status 5" {
		mac := rigMAC
		if m.mac != "" {
			mac = m.mac
		}
		return json.RawMessage(`{"StatusNET":{"Mac":"` + strings.ToUpper(mac) + `"}}`), nil
	}
	return m.fakeMQTT.Do(context.TODO(), model.Device{}, c)
}

type upRig struct {
	*rig
	up *upMQTT
	mk *marked
}

func newUpRig(t *testing.T, pw Password) *upRig {
	t.Helper()
	g := newRig(t)
	g.devs.d.MQTTPasswordMode, g.devs.d.MQTTPassword = model.MQTTPasswordOwn, "own-secret-pw"
	g.devs.d.MAC, g.devs.d.Name = rigMAC, "plug"
	up := &upMQTT{fakeMQTT: fakeMQTT{reply: `{}`}}
	mk := &marked{}
	g.r = NewRouter(Options{HTTP: g.http, Devices: g.devs, Facts: pwFacts{g.f, pw}, Memory: g.mem, Marker: mk})
	g.r.SetMQTT(up)
	return &upRig{rig: g, up: up, mk: mk}
}

func (g *upRig) bind() UploadBinding { return BindingOf(g.devs.d) }

// Why: the restore over MQTT sends exactly the dump, as Type 2, with the device's own password,
// and never touches HTTP (an upload never fails over).
func TestUploadConfigSendsTheDumpAsType2WithTheDevicePassword(t *testing.T) {
	g := newUpRig(t, Password{Mode: model.MQTTPasswordOwn})
	dump := bytes.Repeat([]byte{7}, 4096)
	if err := g.r.UploadConfig(t.Context(), g.target(), dump, g.bind()); err != nil {
		t.Fatal(err)
	}
	if g.up.uploads != 1 || g.up.typ != mqttcmd.UploadSettings || g.up.password != "own-secret-pw" || !bytes.Equal(g.up.data, dump) {
		t.Fatalf("upload: %+v", g.up)
	}
	if g.http.n() != 0 {
		t.Errorf("HTTP calls: %v", g.http.calls)
	}
}

// Why (D6): broker mode sends TasmoFleet's live broker password, resolved at send time.
func TestUploadConfigBrokerModeUsesTheLiveBrokerPassword(t *testing.T) {
	g := newUpRig(t, Password{Mode: model.MQTTPasswordBroker})
	g.devs.d.MQTTPasswordMode = model.MQTTPasswordBroker
	bp := "broker-pw-1"
	g.r.o.BrokerPassword = func() string { return bp }
	bp = "broker-pw-2" // changed on the settings page after the router was built
	if err := g.r.UploadConfig(t.Context(), g.target(), make([]byte, 100), g.bind()); err != nil || g.up.password != "broker-pw-2" {
		t.Fatalf("%v, password %q", err, g.up.password)
	}
}

// Why (D6): Error 1 marks the password bad, so the next attempt is refused until it is saved
// again, and the error text carries no password.
func TestUploadConfigErrorOneMarksThePasswordBad(t *testing.T) {
	g := newUpRig(t, Password{Mode: model.MQTTPasswordOwn})
	g.up.uploadErr = mqttcmd.ErrPasswordRefused
	err := g.r.UploadConfig(t.Context(), g.target(), make([]byte, 100), g.bind())
	if !errors.Is(err, mqttcmd.ErrPasswordRefused) || strings.Contains(err.Error(), "own-secret-pw") {
		t.Fatalf("%v", err)
	}
	if len(g.mk.ids) != 1 || g.mk.ids[0] != 7 {
		t.Fatalf("marked %v", g.mk.ids)
	}
}

// Why: a restore replaces the whole configuration, so it needs the strict conditions and a
// usable password; otherwise nothing is published.
func TestUploadConfigRefusesWithoutAPathOrPassword(t *testing.T) {
	g := newUpRig(t, Password{Mode: model.MQTTPasswordNone})
	if err := g.r.UploadConfig(t.Context(), g.target(), make([]byte, 100), g.bind()); !errors.Is(err, tasmota.ErrUnreachable) || g.up.uploads != 0 {
		t.Fatalf("no password: %v, uploads %d", err, g.up.uploads)
	}
	g = newUpRig(t, Password{Mode: model.MQTTPasswordOwn, BadAt: time.Now()})
	if err := g.r.UploadConfig(t.Context(), g.target(), make([]byte, 100), g.bind()); !errors.Is(err, tasmota.ErrUnreachable) || g.up.uploads != 0 {
		t.Fatalf("bad password: %v, uploads %d", err, g.up.uploads)
	}
	g = newUpRig(t, Password{Mode: model.MQTTPasswordOwn})
	g.f.groupKnown = false // a message to an unread group topic could reach other devices
	if err := g.r.UploadConfig(t.Context(), g.target(), make([]byte, 100), g.bind()); err == nil || g.up.uploads != 0 {
		t.Fatalf("group topic unknown: %v, uploads %d", err, g.up.uploads)
	}
	g = newUpRig(t, Password{Mode: model.MQTTPasswordOwn})
	if err := g.r.UploadConfig(t.Context(), tasmota.TargetFor(model.Device{Address: "192.0.2.1"}), make([]byte, 100), g.bind()); err == nil || g.up.uploads != 0 {
		t.Fatalf("device without ID: %v", err)
	}
}

// Why: a channel that cannot upload (no MQTT, or a fake) is "not reachable", never a silent success.
func TestUploadConfigWithoutMQTTFails(t *testing.T) {
	g := newUpRig(t, Password{Mode: model.MQTTPasswordOwn})
	g.r.SetMQTT(nil)
	if err := g.r.UploadConfig(t.Context(), g.target(), make([]byte, 100), g.bind()); !errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatalf("%v", err)
	}
}

// Why (identity binding): the restore checked one device; the upload must go to that device or
// nowhere. A topic, broker or MAC change between the check and the start, and another device
// answering on the topic, each send nothing.
func TestUploadConfigIsBoundToTheCheckedDevice(t *testing.T) {
	changes := map[string]func(g *upRig){
		"topic changed":                    func(g *upRig) { g.devs.d.Topic = "other-plug" },
		"full topic changed":               func(g *upRig) { g.devs.d.FullTopic = "%topic%/%prefix%/" },
		"group topic changed":              func(g *upRig) { g.devs.d.GroupTopic = "elsewhere" },
		"broker capability":                func(g *upRig) { g.devs.d.MQTT = model.MQTTNone },
		"stored MAC changed":               func(g *upRig) { g.devs.d.MAC = "aa:bb:cc:dd:ee:99" },
		"another device answers":           func(g *upRig) { g.up.mac = "11:22:33:44:55:66" },
		"a second device answers as well":  func(g *upRig) { g.up.extraMAC = rigMAC },
		"a second device with another MAC": func(g *upRig) { g.up.extraMAC = "11:22:33:44:55:66" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			g := newUpRig(t, Password{Mode: model.MQTTPasswordOwn})
			bind := g.bind() // what the restore checked
			change(g)
			err := g.r.UploadConfig(t.Context(), g.target(), make([]byte, 100), bind)
			if err == nil || g.up.uploads != 0 {
				t.Fatalf("%v, uploads %d", err, g.up.uploads)
			}
			if !errors.Is(err, ErrIdentityChanged) {
				// the broker capability change is refused by the path check first; still nothing sent
				if name != "broker capability" {
					t.Fatalf("want ErrIdentityChanged, got %v", err)
				}
			}
		})
	}
	g := newUpRig(t, Password{Mode: model.MQTTPasswordOwn})
	g.up.mac = "AA-BB-CC-DD-EE-01" // other spelling of the same MAC
	if err := g.r.UploadConfig(t.Context(), g.target(), make([]byte, 100), g.bind()); err != nil {
		t.Fatalf("the same MAC in another spelling must pass: %v", err)
	}
	g.up.mac = "11:22:33:44:55:66"
	err := g.r.UploadConfig(t.Context(), g.target(), make([]byte, 100), g.bind())
	if err == nil || !strings.Contains(err.Error(), "plug is not") && !strings.Contains(err.Error(), "not plug any more") {
		t.Errorf("error should say the device is not plug any more: %v", err)
	}
}
