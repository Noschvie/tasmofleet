package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/store"
)

// testMQTT is the command channel with FileDownload and the password test: it records the passwords it was given.
type testMQTT struct {
	dlMQTT
	tested []string
	terr   error
}

func (m *testMQTT) TestPassword(_ context.Context, _ model.Device, pw string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tested = append(m.tested, pw)
	if m.during != nil {
		m.during()
	}
	return m.terr
}

// okMarker records both test results; err makes both writes fail.
type okMarker struct {
	fakeMarker
	mu     sync.Mutex
	ok     []model.DeviceID
	tested []time.Time
	err    error
}

func (o *okMarker) MarkMQTTPasswordOK(_ context.Context, id model.DeviceID, tested, _ time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ok = append(o.ok, id)
	o.tested = append(o.tested, tested)
	return o.err
}

func (o *okMarker) MarkMQTTPasswordBad(ctx context.Context, id model.DeviceID, at time.Time) error {
	if err := o.fakeMarker.MarkMQTTPasswordBad(ctx, id, at); err != nil {
		return err
	}
	return o.err
}

type testRig struct {
	*dlRig
	tm     *testMQTT
	marker *okMarker
}

func newTestRig(t *testing.T) *testRig {
	t.Helper()
	g := newDLRig(t)
	tm := &testMQTT{dlMQTT: dlMQTT{fakeMQTT: fakeMQTT{reply: "{}"}, data: []byte("DUMP")}}
	mk := &okMarker{}
	g.r.o.Marker = mk
	g.r.SetMQTT(tm)
	return &testRig{dlRig: g, tm: tm, marker: mk}
}

// Why (#609): a device whose MqttPassword is empty must be backed up over MQTT like any other, sending
// Password "", its files too (Type 8, #924). Before, the empty mode did not exist: such a device was "not
// recognised" and every transfer went over HTTP or nowhere.
func TestANoPasswordDeviceBacksUpOverMQTT(t *testing.T) {
	g := newTestRig(t)
	g.devs.d.MQTTPasswordMode, g.devs.d.MQTTPassword = model.MQTTPasswordEmpty, ""
	ctx, tr := WithTrace(t.Context())
	got, err := g.r.DownloadConfig(ctx, g.target())
	if err != nil || string(got) != "DUMP" || tr.Via() != "mqtt" {
		t.Fatalf("%q, %v, via %s", got, err, tr.Via())
	}
	if len(g.tm.pws) != 1 || g.tm.pws[0] != "" || g.http.dl != 0 {
		t.Fatalf("passwords %q, http downloads %d", g.tm.pws, g.http.dl)
	}
	f, fm := newFileRig(t)
	f.devs.d.MQTTPasswordMode, f.devs.d.MQTTPassword = model.MQTTPasswordEmpty, ""
	fctx, ftr := WithTrace(t.Context())
	data, err := f.r.MQTTDownloadFile(fctx, f.devs.d, "/autoexec.be", 100)
	if err != nil || string(data) != "FILE /autoexec.be" || ftr.Via() != "mqtt" {
		t.Fatalf("file %q, %v, via %s", data, err, ftr.Via())
	}
	if len(fm.pws) != 1 || fm.pws[0] != "" || f.http.n() != 0 {
		t.Fatalf("passwords %q, HTTP %d", fm.pws, f.http.n())
	}
}

// Why (#609): the test starts a FileDownload, so it must obey the strict rule of transfers: a topic that may
// be shared (GroupTopic unread), a device that is offline, or no password to send gets nothing at all, and
// the person is told why. It never falls back to HTTP, which cannot check the password.
func TestThePasswordTestSendsNothingTheStrictRuleRefuses(t *testing.T) {
	for name, mod := range map[string]func(*testRig){
		"group topic unread":          func(g *testRig) { g.f.groupKnown = false },
		"offline":                     func(g *testRig) { g.f.lwtOffline = true },
		"not set":                     func(g *testRig) { g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone },
		"broker without a password":   func(g *testRig) { g.devs.d.MQTTPasswordMode = model.MQTTPasswordBroker; g.broker = "" },
		"another device on the topic": func(g *testRig) { g.r.TopicTaken().Mark(7, "plug") },
	} {
		t.Run(name, func(t *testing.T) {
			g := newTestRig(t)
			mod(g)
			err := g.r.TestMQTTPassword(t.Context(), 7)
			var blocked *PasswordTestBlockedError
			if !errors.As(err, &blocked) || blocked.Reason == "" || !strings.Contains(err.Error(), blocked.Reason) {
				t.Fatalf("want a blocked test with a reason, got %v", err)
			}
			if len(g.tm.tested) != 0 || g.http.n() != 0 || len(g.marker.ids) != 0 || len(g.marker.ok) != 0 {
				t.Fatalf("sent %q, http %d, marks %v/%v", g.tm.tested, g.http.n(), g.marker.ids, g.marker.ok)
			}
		})
	}
}

// Why (#609): the test is how a refused password is checked again, so the refusal mark must not stop it. Its
// result is recorded: an acceptance for the password that was tested (its set time, so a password changed
// meanwhile is not marked) and clears the mark; a refusal sets it. The error never holds the password.
func TestThePasswordTestRecordsItsResult(t *testing.T) {
	g := newTestRig(t)
	set := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	g.devs.d.MQTTPasswordBadAt, g.devs.d.MQTTPasswordSetAt = set.Add(time.Hour), set
	if err := g.r.TestMQTTPassword(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if len(g.tm.tested) != 1 || g.tm.tested[0] != "own-pw-1" || len(g.marker.ok) != 1 || !g.marker.tested[0].Equal(set) {
		t.Fatalf("tested %q, ok marks %v at %v", g.tm.tested, g.marker.ok, g.marker.tested)
	}
	g.tm.terr = mqttcmd.ErrPasswordRefused
	err := g.r.TestMQTTPassword(t.Context(), 7)
	if !errors.Is(err, mqttcmd.ErrPasswordRefused) || len(g.marker.ids) != 1 || g.marker.ids[0] != 7 || len(g.marker.ok) != 1 {
		t.Fatalf("%v, bad marks %v, ok marks %v", err, g.marker.ids, g.marker.ok)
	}
	if strings.Contains(err.Error(), "own-pw-1") {
		t.Fatalf("the error holds the password: %v", err)
	}
	g.tm.terr = mqttcmd.ErrTransfer
	if err := g.r.TestMQTTPassword(t.Context(), 7); !errors.Is(err, mqttcmd.ErrTransfer) || len(g.marker.ids) != 1 || len(g.marker.ok) != 1 {
		t.Fatalf("a failed test marked the password: %v, %v/%v", err, g.marker.ids, g.marker.ok)
	}
}

type errDevices struct{}

func (errDevices) GetDevice(context.Context, model.DeviceID) (model.Device, error) {
	return model.Device{}, errors.New("no such device")
}

type errFacts struct{}

func (errFacts) FactsFor(context.Context, model.Device) (Facts, error) {
	return Facts{}, errors.New("no facts")
}

func (errFacts) HTTPState(model.DeviceID) HTTPState { return HTTPState{} }

// Why (#609): without the channel's test, without the device or without its facts the test sends nothing and
// says why; a refusal the channel decided before publishing is a blocked test in TasmoFleet's own words, and a
// result the store cannot write is logged, not lost silently, while the caller still learns the result.
func TestThePasswordTestFailsClosed(t *testing.T) {
	g := newTestRig(t)
	g.r.SetMQTT(&g.dl.fakeMQTT) // a channel without the test
	var blocked *PasswordTestBlockedError
	if err := g.r.TestMQTTPassword(t.Context(), 7); !errors.As(err, &blocked) {
		t.Fatalf("no tester: %v", err)
	}
	g = newTestRig(t)
	g.r.o.Devices = errDevices{}
	if err := g.r.TestMQTTPassword(t.Context(), 7); err == nil || len(g.tm.tested) != 0 {
		t.Fatalf("no device: %v, tested %q", err, g.tm.tested)
	}
	g = newTestRig(t)
	g.r.o.Facts = errFacts{}
	if err := g.r.TestMQTTPassword(t.Context(), 7); err == nil || len(g.tm.tested) != 0 {
		t.Fatalf("no facts: %v, tested %q", err, g.tm.tested)
	}
	g = newTestRig(t)
	g.tm.terr = fmt.Errorf("%w: %w", mqttcmd.ErrRefused, mqttcmd.ErrBufferFull)
	if err := g.r.TestMQTTPassword(t.Context(), 7); !errors.As(err, &blocked) || len(g.marker.ids) != 0 {
		t.Fatalf("refused by the channel: %v, marks %v", err, g.marker.ids)
	}
	g = newTestRig(t)
	g.marker.err = errors.New("disk full")
	if err := g.r.TestMQTTPassword(t.Context(), 7); err != nil || len(g.marker.ok) != 1 {
		t.Fatalf("an accepted test whose mark failed: %v, %v", err, g.marker.ok)
	}
	g.tm.terr = mqttcmd.ErrPasswordRefused
	if err := g.r.TestMQTTPassword(t.Context(), 7); !errors.Is(err, mqttcmd.ErrPasswordRefused) || len(g.marker.ids) != 1 {
		t.Fatalf("a refused test whose mark failed: %v, %v", err, g.marker.ids)
	}
}

// setAtMarker is the store's MarkMQTTPasswordBadFor: it marks only while the stored password is the one that
// was sent (its set time), and answers store.ErrNotFound otherwise.
type setAtMarker struct {
	okMarker
	devs *fakeDevices
}

func (m *setAtMarker) MarkMQTTPasswordBadFor(ctx context.Context, id model.DeviceID, tested, at time.Time) error {
	m.devs.mu.Lock()
	stored := m.devs.d.MQTTPasswordSetAt
	m.devs.mu.Unlock()
	if !stored.Equal(tested) {
		return store.ErrNotFound
	}
	return m.MarkMQTTPasswordBad(ctx, id, at)
}

// Why (#609): the learner replaces a device's password on its own, so the device can refuse the old password
// after a newer one was stored. That refusal is about the password that was sent: marking the new one would
// block every MQTT transfer of the device for a password it never received. A refusal of the password still
// stored is recorded as before.
func TestARefusalMarksOnlyThePasswordThatWasSent(t *testing.T) {
	g := newTestRig(t)
	sent := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	g.devs.d.MQTTPasswordSetAt = sent
	mk := &setAtMarker{devs: g.devs}
	g.r.o.Marker = mk
	g.tm.terr = mqttcmd.ErrPasswordRefused
	g.tm.during = func() { // a newer backup was learned while the device answered
		g.devs.mu.Lock()
		g.devs.d.MQTTPassword, g.devs.d.MQTTPasswordSetAt = "newpw", sent.Add(time.Minute)
		g.devs.mu.Unlock()
	}
	if err := g.r.TestMQTTPassword(t.Context(), 7); !errors.Is(err, mqttcmd.ErrPasswordRefused) {
		t.Fatal(err)
	}
	if len(g.tm.tested) != 1 || g.tm.tested[0] != "own-pw-1" || len(mk.ids) != 0 {
		t.Fatalf("tested %q, the newer password was marked: %v", g.tm.tested, mk.ids)
	}
	g.tm.during = nil
	if err := g.r.TestMQTTPassword(t.Context(), 7); !errors.Is(err, mqttcmd.ErrPasswordRefused) {
		t.Fatal(err)
	}
	if len(g.tm.tested) != 2 || g.tm.tested[1] != "newpw" || len(mk.ids) != 1 || mk.ids[0] != 7 {
		t.Fatalf("tested %q, a refusal of the stored password was not marked: %v", g.tm.tested, mk.ids)
	}
}

// Why (#609, "tested at their next transfer"): a typed or bulk-set password is "Not tested" until the device
// accepts it. The learner ignores a backup that shows the password it already holds, so without this a device
// that backs up over MQTT every night would stay "Not tested" for ever. A download that finishes proves the device
// took the password (it checks before it starts), so it is recorded for the password that was sent, and not again
// while it stands; a refused download records nothing.
func TestAFinishedBackupOverMQTTRecordsThePasswordAsAccepted(t *testing.T) {
	g := newTestRig(t)
	set := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	g.devs.d.MQTTPasswordSetAt = set
	if _, err := g.r.DownloadConfig(t.Context(), g.target()); err != nil {
		t.Fatal(err)
	}
	if len(g.marker.ok) != 1 || g.marker.ok[0] != 7 || !g.marker.tested[0].Equal(set) {
		t.Fatalf("ok marks %v at %v, want one for the password set at %v", g.marker.ok, g.marker.tested, set)
	}
	g.devs.d.MQTTPasswordOKAt = set.Add(time.Hour) // recorded already: no second write
	if _, err := g.r.DownloadConfig(t.Context(), g.target()); err != nil || len(g.marker.ok) != 1 {
		t.Fatalf("a second backup wrote again: %v, %v", err, g.marker.ok)
	}
	g.devs.d.MQTTPasswordOKAt = time.Time{}
	g.tm.derr = mqttcmd.ErrPasswordRefused
	if _, err := g.r.DownloadConfig(t.Context(), g.target()); err == nil || len(g.marker.ok) != 1 {
		t.Fatalf("a refused backup recorded an acceptance: %v, %v", err, g.marker.ok)
	}
}

// Why (#609): the audit and the notice after a test say accepted, refused, blocked or error. Only a refusal
// ("Error 1") is a statement about the password; a blocked test sent nothing, and a silent or broken transfer says
// nothing about it. Mixing them up would tell the person to change a password that was never checked.
func TestThePasswordTestOutcomeSeparatesWhatSaysSomethingAboutThePassword(t *testing.T) {
	for want, err := range map[string]error{
		"accepted": nil,
		"refused":  fmt.Errorf("x: %w", mqttcmd.ErrPasswordRefused),
		"blocked":  fmt.Errorf("x: %w", &PasswordTestBlockedError{Reason: "r"}),
		"error":    mqttcmd.ErrTransfer,
	} {
		if got := PasswordTestOutcome(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}

// Why (#609): a file read over Type 8 is a finished transfer too, and it carries the same password: the device
// checked it before it sent the file, so it is recorded as accepted like the dump's.
func TestAFileReadOverMQTTRecordsThePasswordAsAccepted(t *testing.T) {
	f, _ := newFileRig(t)
	ok := &okMarker{}
	f.r.o.Marker = ok
	if _, err := f.r.MQTTDownloadFile(t.Context(), f.devs.d, "/autoexec.be", 100); err != nil {
		t.Fatal(err)
	}
	if len(ok.ok) != 1 || ok.ok[0] != 7 {
		t.Fatalf("ok marks %v, want one for device 7", ok.ok)
	}
}

// Why (#609): recording the acceptance is bookkeeping. When the store cannot write it, the backup that was taken
// must still be returned: losing a good backup for a failed note would be worse than "Not tested".
func TestAFailedAcceptedMarkNeverFailsTheBackup(t *testing.T) {
	g := newTestRig(t)
	g.marker.err = errors.New("database is locked")
	got, err := g.r.DownloadConfig(t.Context(), g.target())
	if err != nil || string(got) != "DUMP" || len(g.marker.ok) != 1 {
		t.Fatalf("%q, %v, marks %v", got, err, g.marker.ok)
	}
}
