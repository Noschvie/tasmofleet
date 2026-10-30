package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// dlMQTT is the command channel with FileDownload: it records the passwords it was given.
type dlMQTT struct {
	fakeMQTT
	pws  []string
	data []byte
	derr error
}

func (m *dlMQTT) Download(_ context.Context, _ model.Device, pw string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pws = append(m.pws, pw)
	return m.data, m.derr
}

type fakeMarker struct {
	mu  sync.Mutex
	ids []model.DeviceID
}

func (f *fakeMarker) MarkMQTTPasswordBad(_ context.Context, id model.DeviceID, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = append(f.ids, id)
	return nil
}

type dlRig struct {
	*rig
	dl     *dlMQTT
	marker *fakeMarker
	broker string
}

// newDLRig is a device on our broker whose HTTP is known to fail and that has an own MQTT password.
func newDLRig(t *testing.T) *dlRig {
	t.Helper()
	g := newRig(t)
	d := &dlRig{rig: g, dl: &dlMQTT{fakeMQTT: fakeMQTT{reply: "{}"}, data: []byte("DUMP")}, marker: &fakeMarker{}, broker: "broker-pw-1"}
	g.http.dump = []byte("HTTPDUMP")
	g.r.o.Marker = d.marker
	g.r.o.BrokerPassword = func() string { return d.broker }
	g.r.SetMQTT(d.dl)
	g.devs.d.MQTTPasswordMode, g.devs.d.MQTTPassword = model.MQTTPasswordOwn, "own-pw-1"
	g.mem.Fail(7, HTTPFailUnreachable)
	return d
}

func TestBackupOverMQTTWhenHTTPFailsUsesTheOwnPassword(t *testing.T) {
	// Why (#257): the dump comes over MQTT with the device's own password, and HTTP is not tried.
	g := newDLRig(t)
	ctx, tr := WithTrace(t.Context())
	got, err := g.r.DownloadConfig(ctx, g.target())
	if err != nil || string(got) != "DUMP" || tr.Via() != "mqtt" {
		t.Fatalf("%q, %v, via %s", got, err, tr.Via())
	}
	if len(g.dl.pws) != 1 || g.dl.pws[0] != "own-pw-1" || g.http.dl != 0 {
		t.Fatalf("passwords %v, http downloads %d", g.dl.pws, g.http.dl)
	}
}

func TestBrokerModeFollowsAChangedBrokerPassword(t *testing.T) {
	// Why (D6): broker mode resolves MQTT_PASSWORD at send time, so a change on the settings page counts at once.
	g := newDLRig(t)
	g.devs.d.MQTTPasswordMode, g.devs.d.MQTTPassword = model.MQTTPasswordBroker, ""
	for _, pw := range []string{"first", "second"} {
		g.broker = pw
		if _, err := g.r.DownloadConfig(t.Context(), g.target()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(g.dl.pws, ",") != "first,second" {
		t.Fatalf("passwords %v", g.dl.pws)
	}
}

func TestBackupWithoutAUsablePasswordPublishesNothingAndSaysWhy(t *testing.T) {
	// Why (acceptance): not set, refused before, or broker mode without a broker password: no FileDownload,
	// HTTP gets its chance, and the error carries the reason.
	for name, mod := range map[string]func(*dlRig){
		"not set":        func(g *dlRig) { g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone },
		"refused before": func(g *dlRig) { g.devs.d.MQTTPasswordBadAt = time.Now() },
		"broker, none":   func(g *dlRig) { g.devs.d.MQTTPasswordMode = model.MQTTPasswordBroker; g.broker = "" },
	} {
		t.Run(name, func(t *testing.T) {
			g := newDLRig(t)
			g.http.err = tasmota.ErrUnreachable
			mod(g)
			_, err := g.r.DownloadConfig(t.Context(), g.target())
			if err == nil || len(g.dl.pws) != 0 || !strings.Contains(err.Error(), "password") {
				t.Fatalf("%v, downloads %v: the reason must be in the error", err, g.dl.pws)
			}
		})
	}
}

func TestRefusedPasswordMarksItBadAndNamesTheFix(t *testing.T) {
	// Why (D6): Error 1 blocks further transfers (the mark), and the person is told what to do; the
	// caller can still tell it with errors.Is.
	g := newDLRig(t)
	g.dl.derr = mqttcmd.ErrPasswordRefused
	_, err := g.r.DownloadConfig(t.Context(), g.target())
	if !errors.Is(err, mqttcmd.ErrPasswordRefused) || len(g.marker.ids) != 1 || g.marker.ids[0] != 7 {
		t.Fatalf("%v, marked %v", err, g.marker.ids)
	}
	if !strings.Contains(err.Error(), "Edit page") || strings.Contains(err.Error(), "own-pw-1") {
		t.Fatalf("error %q", err)
	}
}

func TestAStartedMQTTTransferNeverFallsBackToHTTP(t *testing.T) {
	// Why: no failover after a started transfer; the error is the caller's.
	g := newDLRig(t)
	g.dl.derr = mqttcmd.ErrTransfer
	if _, err := g.r.DownloadConfig(t.Context(), g.target()); !errors.Is(err, mqttcmd.ErrTransfer) || g.http.dl != 0 {
		t.Fatalf("%v, http downloads %d", err, g.http.dl)
	}
}

func TestPinnedBackupPaths(t *testing.T) {
	// Why (D5): a job that chose its path is never rerouted. Pinned HTTP never touches MQTT even when
	// HTTP is known to fail; pinned MQTT never touches HTTP and applies the transfer conditions.
	g := newDLRig(t)
	if got, err := g.r.DownloadConfig(WithPath(t.Context(), HTTP), g.target()); err != nil || string(got) != "HTTPDUMP" || len(g.dl.pws) != 0 {
		t.Fatalf("pinned HTTP: %q %v %v", got, err, g.dl.pws)
	}
	if got, err := g.r.DownloadConfig(WithPath(t.Context(), MQTT), g.target()); err != nil || string(got) != "DUMP" || g.http.dl != 1 {
		t.Fatalf("pinned MQTT: %q %v, http downloads %d", got, err, g.http.dl)
	}
	g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone
	if _, err := g.r.DownloadConfig(WithPath(t.Context(), MQTT), g.target()); !errors.Is(err, tasmota.ErrUnreachable) || len(g.dl.pws) != 1 {
		t.Fatalf("pinned MQTT without a password: %v, downloads %v", err, g.dl.pws)
	}
}

func TestWorkingHTTPIsUsedForBackupsAsBefore(t *testing.T) {
	// Why: MQTT is the fallback; a device HTTP reaches is backed up over /dl, and a target without a
	// device never uses MQTT.
	g := newDLRig(t)
	g.mem.Clear(7)
	if got, err := g.r.DownloadConfig(t.Context(), g.target()); err != nil || string(got) != "HTTPDUMP" || len(g.dl.pws) != 0 {
		t.Fatalf("%q %v %v", got, err, g.dl.pws)
	}
}

func TestBackupDoesNotSendWhenTheTopicChangedUnderIt(t *testing.T) {
	// Why: facts and send look at one snapshot; an edit in between must not publish the password to the old topic.
	g := newDLRig(t)
	g.r.o.Facts = changingFacts{inner: g.f, devs: g.devs}
	if _, err := g.r.DownloadConfig(t.Context(), g.target()); err == nil || len(g.dl.pws) != 0 {
		t.Fatalf("%v, downloads %v", err, g.dl.pws)
	}
}

func TestHTTPThatNeverSentFailsOverToAMQTTBackup(t *testing.T) {
	// Why (D4): a proof of "nothing left" (here: an address outside the device networks) gives MQTT the
	// same chance a command gets; an unknown outcome (timeout) does not, a backup is read-only but the
	// rule stays one rule.
	for err, want := range map[error]int{tasmota.ErrForbiddenAddr: 1, tasmota.ErrTimeout: 0} {
		g := newDLRig(t)
		g.mem.Clear(7)
		g.http.err = err
		_, _ = g.r.DownloadConfig(t.Context(), g.target())
		if len(g.dl.pws) != want {
			t.Errorf("%v: %d MQTT downloads, want %d", err, len(g.dl.pws), want)
		}
	}
}

func TestRefusedByTheChannelIsExplainedInItsOwnWords(t *testing.T) {
	// Why (review M1): a refusal before anything was sent (a pending firmware upload, an unsendable password
	// character) reaches the person as a sentence written by mqttcmd, never as the error text.
	g := newDLRig(t)
	g.dl.derr = fmt.Errorf("%w: %w: and a password-looking detail", mqttcmd.ErrRefused, mqttcmd.ErrFirmwarePending)
	_, err := g.r.DownloadConfig(t.Context(), g.target())
	var blocked *BackupBlockedError
	if !errors.As(err, &blocked) || blocked.Attempted || !strings.Contains(blocked.UserReason(), "wait for the device to restart") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(blocked.UserReason(), "password-looking") || len(g.marker.ids) != 0 {
		t.Fatalf("reason %q, marked %v", blocked.UserReason(), g.marker.ids)
	}
}

func TestPasswordRefusedAfterTheSnapshotIsNotPublished(t *testing.T) {
	// Why (review L1): the refused mark is checked on the fresh read right before the send, not only on
	// the snapshot the facts were built from.
	g := newDLRig(t)
	g.r.o.Facts = markAfterFacts{inner: g.f, devs: g.devs}
	if _, err := g.r.DownloadConfig(t.Context(), g.target()); err == nil || len(g.dl.pws) != 0 {
		t.Fatalf("%v, downloads %v", err, g.dl.pws)
	}
}

type markAfterFacts struct {
	inner *facts
	devs  *fakeDevices
}

func (c markAfterFacts) HTTPState(id model.DeviceID) HTTPState { return c.inner.HTTPState(id) }
func (c markAfterFacts) FactsFor(ctx context.Context, d model.Device) (Facts, error) {
	f, err := c.inner.FactsFor(ctx, d)
	c.devs.mu.Lock()
	c.devs.d.MQTTPasswordBadAt = time.Now() // another backup's Error 1 lands between the facts and the send
	c.devs.mu.Unlock()
	return f, err
}

func TestATraceInsideATraceReportsToBoth(t *testing.T) {
	// Why: the backup service traces its own run for the audit while a bulk job traces the whole step.
	outer, otr := WithTrace(t.Context())
	inner, itr := WithTrace(outer)
	noteTrace(inner, MQTT)
	if otr.Via() != "mqtt" || itr.Via() != "mqtt" {
		t.Fatalf("outer %s inner %s", otr.Via(), itr.Via())
	}
}
