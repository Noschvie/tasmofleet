package transport

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// macAsked records each Status 5 with its target and answers in order.
type macAsked struct {
	tasmota.Client
	answers []func() (json.RawMessage, error)
	targets []tasmota.Target
	paths   []Path
}

func (m *macAsked) Command(ctx context.Context, t tasmota.Target, _ string) (json.RawMessage, error) {
	m.targets = append(m.targets, t)
	p, _ := PathFrom(ctx)
	m.paths = append(m.paths, p)
	a := m.answers[0]
	m.answers = m.answers[1:]
	return a()
}

func answer(raw string) func() (json.RawMessage, error) {
	return func() (json.RawMessage, error) { return json.RawMessage(raw), nil }
}

func failing(err error) func() (json.RawMessage, error) {
	return func() (json.RawMessage, error) { return nil, err }
}

// Why (#607, the review's second finding): the identity question of a backup and a restore is the one place that decides
// whether the host at the device's address is the device. Over HTTP it goes without the stored web credentials first,
// so a host that is not the device never sees them unless it demands a password (a 401); only then the credentials go,
// over HTTP, to the same address. Over MQTT it asks by the device's topic (the target keeps the device ID).
func TestTheIdentityQuestionSendsTheWebPasswordOnlyToAHostThatAsksForIt(t *testing.T) {
	dev := model.Device{ID: 7, Address: "192.0.2.7", Port: 80, HTTPPassword: "stored-web-password"}
	mac := `{"StatusNET":{"Mac":"AA:BB:CC:DD:EE:FF"}}`

	c := &macAsked{answers: []func() (json.RawMessage, error){answer(mac)}}
	got, err := AskMAC(t.Context(), c, dev)
	if err != nil || got != "aabbccddeeff" || len(c.targets) != 1 || c.targets[0].HasCredentials() || c.targets[0].DeviceID() != 0 || c.paths[0] != HTTP {
		t.Fatalf("%q, %v, targets %+v, paths %v: one question over HTTP without credentials", got, err, c.targets, c.paths)
	}

	c = &macAsked{answers: []func() (json.RawMessage, error){failing(tasmota.ErrAuth), answer(mac)}}
	if got, err := AskMAC(t.Context(), c, dev); err != nil || got != "aabbccddeeff" || len(c.targets) != 2 || !c.targets[1].HasCredentials() {
		t.Fatalf("%q, %v, targets %+v: after a 401 the credentials go", got, err, c.targets)
	}

	c = &macAsked{answers: []func() (json.RawMessage, error){failing(tasmota.ErrAuth), failing(tasmota.ErrAuth)}}
	if _, err := AskMAC(t.Context(), c, dev); !errors.Is(err, tasmota.ErrAuth) {
		t.Fatalf("%v: a refused password is the answer", err)
	}

	c = &macAsked{answers: []func() (json.RawMessage, error){failing(tasmota.ErrUnreachable)}}
	if _, err := AskMAC(t.Context(), c, dev); !errors.Is(err, tasmota.ErrUnreachable) || len(c.targets) != 1 {
		t.Fatalf("%v, %d questions: no answer ends it, without the credentials", err, len(c.targets))
	}

	c = &macAsked{answers: []func() (json.RawMessage, error){answer(`{"StatusNET":{}}`)}}
	if _, err := AskMAC(t.Context(), c, dev); !errors.Is(err, tasmota.ErrBadResponse) {
		t.Fatalf("%v: an answer without a MAC proves nothing", err)
	}

	c = &macAsked{answers: []func() (json.RawMessage, error){answer(mac)}}
	if got, err := AskMAC(WithPath(t.Context(), MQTT), c, dev); err != nil || got != "aabbccddeeff" || c.targets[0].DeviceID() != 7 || c.paths[0] != MQTT {
		t.Fatalf("%q, %v, targets %+v: over MQTT by the device's topic", got, err, c.targets)
	}
	c = &macAsked{answers: []func() (json.RawMessage, error){failing(tasmota.ErrTimeout)}}
	if _, err := AskMAC(WithPath(t.Context(), MQTT), c, dev); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("%v", err)
	}
}

// Why (#607): the files of a transfer over MQTT ask FilesUsable before their identity question over HTTP; it must say
// no without sending anything when no file call could go: HTTP known to fail, or an HTTP client without file access.
func TestFilesUsableSaysWhenNoFileCallCouldGo(t *testing.T) {
	r, _, tg, mem := newFSRig(t)
	if err := r.FilesUsable(tg); err != nil {
		t.Fatalf("%v: files go while HTTP is not failing", err)
	}
	mem.Fail(7, HTTPFailUnreachable)
	if err := r.FilesUsable(tg); !errors.Is(err, ErrFilesNeedHTTP) {
		t.Fatalf("%v", err)
	}
	g := newRig(t) // its HTTP client has no file access
	if err := g.r.FilesUsable(g.target()); !errors.Is(err, tasmota.ErrNotImplemented) {
		t.Fatalf("%v", err)
	}
}
