package transport

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// fileMQTT is the command channel with FileDownload Type 8: it records the files asked for and the passwords.
type fileMQTT struct {
	dlMQTT
	files []string
	ferr  error
}

func (m *fileMQTT) DownloadFile(_ context.Context, _ model.Device, pw, path string, _ int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files, m.pws = append(m.files, path), append(m.pws, pw)
	if m.ferr != nil {
		return nil, m.ferr
	}
	return []byte("FILE " + path), nil
}

// newFileRig is a device on our broker with its own MQTT password, HTTP unknown, whose channel can send files.
func newFileRig(t *testing.T) (*dlRig, *fileMQTT) {
	t.Helper()
	g := newRig(t)
	fm := &fileMQTT{dlMQTT: dlMQTT{fakeMQTT: fakeMQTT{reply: `{"UfsList":[["autoexec.be","2026-11-01T10:00:00",18],["lib","",0]]}`}}}
	d := &dlRig{rig: g, marker: &fakeMarker{}, broker: "broker-pw-1"}
	g.r.o.Marker = d.marker
	g.r.o.BrokerPassword = func() string { return d.broker }
	g.r.SetMQTT(fm)
	g.devs.d.MQTTPasswordMode, g.devs.d.MQTTPassword = model.MQTTPasswordOwn, "own-pw-1"
	return d, fm
}

// Why (#924, transport rule): the files of a backup go over MQTT under the same rule as the dump's FileDownload: a
// transfer needs the strict MQTT conditions (no other device on the topic), a password the device has not
// refused, and a device whose MQTT settings did not change since the backup read it; otherwise nothing is
// published, the error says the files cannot go over MQTT, and the file calls never move to HTTP on their own.
// The listing follows the same rule: it is the start of the transfer, and a listing that cannot be followed by
// the contents only costs a publish.
func TestFilesOverMQTTFollowTheTransferRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mod     func(g *dlRig, d *model.Device)
		publish bool
	}{
		{"a usable device", func(*dlRig, *model.Device) {}, true},
		{"another device answers on the topic", func(g *dlRig, _ *model.Device) { g.r.TopicTaken().Mark(7, "plug") }, false},
		{"the topic changed since the backup read the device", func(_ *dlRig, d *model.Device) { d.Topic = "old-plug" }, false},
		{"no password", func(g *dlRig, _ *model.Device) { g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone }, false},
		{"the password was refused before", func(g *dlRig, _ *model.Device) { g.devs.d.MQTTPasswordBadAt = time.Now() }, false},
		{"not on our broker", func(g *dlRig, d *model.Device) { g.devs.d.MQTT, d.MQTT = model.MQTTOtherBroker, model.MQTTOtherBroker }, false},
		{"broker mode without a broker password", func(g *dlRig, _ *model.Device) { g.devs.d.MQTTPasswordMode, g.broker = model.MQTTPasswordBroker, "" }, false},
		{"MQTT not connected", func(g *dlRig, _ *model.Device) { g.r.SetMQTT(nil) }, false},
		{"a job pinned to HTTP", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, fm := newFileRig(t)
			snap := g.devs.d
			ctx := t.Context()
			if tc.mod == nil {
				ctx = WithPath(ctx, HTTP)
			} else {
				tc.mod(g, &snap)
			}
			lctx, ltr := WithTrace(ctx)
			entries, lerr := g.r.MQTTListFiles(lctx, snap, "/")
			fctx, ftr := WithTrace(ctx)
			data, ferr := g.r.MQTTDownloadFile(fctx, snap, "/autoexec.be", 100)
			if !tc.publish {
				if !errors.Is(lerr, ErrFilesNotOverMQTT) || !errors.Is(ferr, ErrFilesNotOverMQTT) {
					t.Fatalf("list %v, file %v: want ErrFilesNotOverMQTT", lerr, ferr)
				}
				if fm.n() != 0 || len(fm.files) != 0 || len(fm.pws) != 0 || g.http.n() != 0 {
					t.Fatalf("published %v, files %v, HTTP %d", fm.sent, fm.files, g.http.n())
				}
				if ltr.Via() != "none" || ftr.Via() != "none" {
					t.Fatalf("via %s/%s: a refused call sent nothing", ltr.Via(), ftr.Via())
				}
				return
			}
			if lerr != nil || len(entries) != 2 || entries[0].Name != "autoexec.be" || !entries[1].Dir {
				t.Fatalf("list %v, %v", entries, lerr)
			}
			if ferr != nil || string(data) != "FILE /autoexec.be" {
				t.Fatalf("file %q, %v", data, ferr)
			}
			if fm.sent[0] != "UfsList /" || fm.pws[0] != "own-pw-1" || g.http.n() != 0 {
				t.Fatalf("sent %v, passwords %v, HTTP %d", fm.sent, fm.pws, g.http.n())
			}
			if ltr.Via() != "mqtt" || ftr.Via() != "mqtt" {
				t.Fatalf("via %s/%s", ltr.Via(), ftr.Via())
			}
		})
	}
}

// Why (D6): Error 1 to a file download is the same signal as to the dump: the password is marked bad, so no
// further transfer sends it until the person enters the right one, and the caller can tell it with errors.Is.
func TestARefusedPasswordOnAFileMarksItBad(t *testing.T) {
	g, fm := newFileRig(t)
	fm.ferr = fmt.Errorf("%w: %w", mqttcmd.ErrPublished, mqttcmd.ErrPasswordRefused)
	_, err := g.r.MQTTDownloadFile(t.Context(), g.devs.d, "/autoexec.be", 100)
	if !errors.Is(err, mqttcmd.ErrPasswordRefused) || len(g.marker.ids) != 1 || g.marker.ids[0] != 7 {
		t.Fatalf("%v, marked %v", err, g.marker.ids)
	}
}

// Why: a listing that does not parse, or whose names break the file path rules, is read by the same rules as over
// HTTP (tasmota.ParseUfsList), so a name the HTTP walk would skip is skipped over MQTT too.
func TestTheListingOverMQTTUsesTheNameRulesOfHTTP(t *testing.T) {
	g, fm := newFileRig(t)
	fm.reply = `{"UfsList":[["ok.be","2026-11-01T10:00:00",1],[".hidden","2026-11-01T10:00:00",1]]}`
	entries, err := g.r.MQTTListFiles(t.Context(), g.devs.d, "/")
	var sk *tasmota.SkippedNamesError
	if !errors.As(err, &sk) || len(entries) != 1 || entries[0].Name != "ok.be" {
		t.Fatalf("%v, %v", entries, err)
	}
	if _, err := g.r.MQTTListFiles(t.Context(), g.devs.d, "lib"); !errors.Is(err, tasmota.ErrBadPath) {
		t.Fatalf("a folder without its slash: %v", err)
	}
}

// Why: a command channel without file transfer (a build or a fake that only does commands) cannot send the files, so
// nothing may be published for them, not even the listing a backup could not follow up.
func TestAChannelWithoutFileTransferListsNothing(t *testing.T) {
	g, _ := newFileRig(t)
	plain := &fakeMQTT{reply: `{"UfsList":"Done"}`}
	g.r.SetMQTT(plain)
	if _, err := g.r.MQTTListFiles(t.Context(), g.devs.d, "/"); !errors.Is(err, ErrFilesNotOverMQTT) || plain.n() != 0 {
		t.Fatalf("%v, sent %v", err, plain.sent)
	}
}

type failingDevices struct{}

func (failingDevices) GetDevice(context.Context, model.DeviceID) (model.Device, error) {
	return model.Device{}, errors.New("store unavailable")
}

type factsUnavailable struct{ DeviceFacts }

func (factsUnavailable) FactsFor(context.Context, model.Device) (Facts, error) {
	return Facts{}, errors.New("facts unavailable")
}

// Why: without a fresh read of the device or of its facts the transfer rule cannot be checked, so the call fails with
// that error and publishes nothing (it never guesses that the device is still usable).
func TestAFileCallWhoseDeviceCannotBeReadPublishesNothing(t *testing.T) {
	for name, mod := range map[string]func(*dlRig){
		"device": func(g *dlRig) { g.r.o.Devices = failingDevices{} },
		"facts":  func(g *dlRig) { g.r.o.Facts = factsUnavailable{g.f} },
	} {
		t.Run(name, func(t *testing.T) {
			g, fm := newFileRig(t)
			mod(g)
			_, lerr := g.r.MQTTListFiles(t.Context(), g.devs.d, "/")
			_, ferr := g.r.MQTTDownloadFile(t.Context(), g.devs.d, "/a.be", 10)
			if lerr == nil || ferr == nil || fm.n() != 0 || len(fm.files) != 0 {
				t.Fatalf("list %v, file %v, sent %v %v", lerr, ferr, fm.sent, fm.files)
			}
		})
	}
}

// Why (T2): a listing the device left unanswered is an error of the walk, not a reason to ask HTTP: the backup decides
// what happens to the files, the router never moves a file call to the other transport.
func TestAnUnansweredListingStaysAnMQTTError(t *testing.T) {
	g, fm := newFileRig(t)
	fm.err = fmt.Errorf("mqtt: no answer: %w", tasmota.ErrTimeout)
	ctx, tr := WithTrace(t.Context())
	if _, err := g.r.MQTTListFiles(ctx, g.devs.d, "/"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("%v", err)
	}
	if g.http.n() != 0 || tr.Via() != "mqtt" {
		t.Fatalf("HTTP %d, via %s: the unanswered listing was published over MQTT and nothing else", g.http.n(), tr.Via())
	}
}

// Why (the audit's via): a file download the channel refused before publishing sent nothing, so the trace says "none";
// one that published anything says "mqtt" even when it failed, because the password crossed the broker.
func TestTheTraceOfAFileDownloadSaysWhetherAnythingWasPublished(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		via  string
	}{
		{"refused before publishing", fmt.Errorf("%w: the path cannot be sent", mqttcmd.ErrRefused), "none"},
		{"failed after publishing", fmt.Errorf("%w: %w", mqttcmd.ErrPublished, mqttcmd.ErrTransfer), "mqtt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, fm := newFileRig(t)
			fm.ferr = tc.err
			ctx, tr := WithTrace(t.Context())
			if _, err := g.r.MQTTDownloadFile(ctx, g.devs.d, "/a.be", 10); err == nil || tr.Via() != tc.via {
				t.Fatalf("%v, via %s, want %s", err, tr.Via(), tc.via)
			}
		})
	}
}

type failingMarker struct{ calls int }

func (m *failingMarker) MarkMQTTPasswordBad(context.Context, model.DeviceID, time.Time) error {
	m.calls++
	return errors.New("store unavailable")
}

// Why (D6): the refused password must reach the caller as such even when the mark cannot be written (it is logged) or
// no marker is attached (a router without a store): the walk still stops on it.
func TestARefusedPasswordIsReportedWhenTheMarkCannotBeWritten(t *testing.T) {
	refused := fmt.Errorf("%w: %w", mqttcmd.ErrPublished, mqttcmd.ErrPasswordRefused)
	g, fm := newFileRig(t)
	fm.ferr = refused
	m := &failingMarker{}
	g.r.o.Marker = m
	if _, err := g.r.MQTTDownloadFile(t.Context(), g.devs.d, "/a.be", 10); !errors.Is(err, mqttcmd.ErrPasswordRefused) || m.calls != 1 {
		t.Fatalf("%v, mark attempts %d", err, m.calls)
	}
	g.r.o.Marker = nil
	if _, err := g.r.MQTTDownloadFile(t.Context(), g.devs.d, "/a.be", 10); !errors.Is(err, mqttcmd.ErrPasswordRefused) {
		t.Fatalf("without a marker: %v", err)
	}
}
