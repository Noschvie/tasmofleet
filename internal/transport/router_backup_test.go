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
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// dlMQTT is the command channel with FileDownload: it records the passwords it was given.
type dlMQTT struct {
	fakeMQTT
	pws    []string
	data   []byte
	derr   error
	during func() // runs while the device answers, before the result comes back
}

func (m *dlMQTT) Download(_ context.Context, _ model.Device, pw string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pws = append(m.pws, pw)
	if m.during != nil {
		m.during()
	}
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
	// Why (acceptance, #997): not set, refused before, or broker mode without a broker password: no FileDownload, and
	// the error carries the reason. HTTP is known to fail here, so /dl is not sent either: it could only time out.
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
			if g.http.dl != 0 {
				t.Fatalf("/dl sent %d times to a device HTTP is known not to reach", g.http.dl)
			}
		})
	}
}

// Why (#609): a backup that sent the old password and got Error 1 after the learner stored a newer one must
// not mark the newer one, or every later transfer of the device is blocked for a password it never received.
func TestARefusedBackupMarksOnlyThePasswordItSent(t *testing.T) {
	g := newDLRig(t)
	sent := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	g.devs.d.MQTTPasswordSetAt = sent
	mk := &setAtMarker{devs: g.devs}
	g.r.o.Marker = mk
	g.dl.derr = mqttcmd.ErrPasswordRefused
	g.dl.during = func() {
		g.devs.mu.Lock()
		g.devs.d.MQTTPassword, g.devs.d.MQTTPasswordSetAt = "newpw", sent.Add(time.Minute)
		g.devs.mu.Unlock()
	}
	if _, err := g.r.DownloadConfig(t.Context(), g.target()); !errors.Is(err, mqttcmd.ErrPasswordRefused) {
		t.Fatal(err)
	}
	if len(g.dl.pws) != 1 || g.dl.pws[0] != "own-pw-1" || len(mk.ids) != 0 {
		t.Fatalf("sent %q, the newer password was marked: %v", g.dl.pws, mk.ids)
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
	// rule stays one rule. HTTP goes first here because the device's MQTT commands went unanswered (#607).
	for err, want := range map[error]int{tasmota.ErrForbiddenAddr: 1, tasmota.ErrTimeout: 0} {
		g := newDLRig(t)
		g.mem.Clear(7)
		g.r.unanswered.Fail(7, HTTPFailUnreachable)
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
	// Why: a refusal after something was published (the opening abort) is an attempt, not "nothing sent".
	g.dl.derr = fmt.Errorf("%w: %w: %w", mqttcmd.ErrPublished, mqttcmd.ErrRefused, mqttcmd.ErrFirmwarePending)
	_, err = g.r.DownloadConfig(t.Context(), g.target())
	if !errors.As(err, &blocked) || !blocked.Attempted {
		t.Fatalf("a refusal after a publish must be Attempted: %v", err)
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

// Why (#997, the maintainer's decision of 2026-11-19): a device that only MQTT could back up, but whose MQTT password
// is not usable, has no way to be backed up. Sending /dl to an address that does not answer only waits out the
// download timeout and then says "took too long", which hides the real cause. So nothing is published, no /dl is
// sent, and the backup ends at once with the reason ("needs an MQTT password"), as a refusal decided before anything
// was sent (Attempted false: the person may retry right after setting the password). HTTP failing counts as not
// reachable, and so does an HTTP probe without an answer.
func TestABackupOnlyMQTTCouldCarryNeedsThePasswordAndSendsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mod    func(*dlRig)
		probes int // Status 11 over HTTP: only when HTTP is not known to work
		why    string
	}{
		{"HTTP failing, no password", func(g *dlRig) { g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone }, 0, "TasmoFleet cannot reach the device over HTTP"},
		{"HTTP failing, password refused", func(g *dlRig) { g.devs.d.MQTTPasswordBadAt = time.Now() }, 0, "refused its MQTT password"},
		{"HTTP unknown, the probe gets no answer", func(g *dlRig) {
			g.mem.Clear(7)
			g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone
			g.http.err = tasmota.ErrTimeout
		}, 1, "no usable answer over HTTP"},
		{"HTTP unknown, the probe is refused", func(g *dlRig) {
			g.mem.Clear(7)
			g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone
			g.http.err = tasmota.ErrAuth
		}, 1, "refused the web password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDLRig(t)
			tc.mod(g)
			ctx, tr := WithTrace(t.Context())
			_, err := g.r.DownloadConfig(ctx, g.target())
			var blocked *BackupBlockedError
			if !errors.As(err, &blocked) || blocked.Attempted {
				t.Fatalf("%v: want a BackupBlockedError decided before anything was sent", err)
			}
			if !strings.HasPrefix(blocked.UserReason(), "Needs an MQTT password: ") || !strings.Contains(blocked.Reason, tc.why) {
				t.Errorf("reason %q: must start with the cause and say %q", blocked.Reason, tc.why)
			}
			if g.http.dl != 0 || len(g.dl.pws) != 0 || g.dl.n() != 0 {
				t.Errorf("/dl %d, FileDownload %v, published %d: nothing may be sent", g.http.dl, g.dl.pws, g.dl.n())
			}
			if g.http.n() != tc.probes || (tc.probes == 1 && g.http.calls[0] != "Status 11") {
				t.Errorf("HTTP commands %v, want %d Status 11", g.http.calls, tc.probes)
			}
			if !errors.Is(err, tasmota.ErrUnreachable) || tr.Via() != "none" {
				t.Errorf("%v via %s: the run reads as unreachable with nothing sent", err, tr.Via())
			}
			if strings.Contains(err.Error(), "own-pw-1") || strings.Contains(err.Error(), "broker-pw-1") {
				t.Errorf("a password in the error: %q", err)
			}
		})
	}
}

// Why (#997 (2), and the #605 pitfall "HTTP not known to fail read as HTTP works"): a device HTTP reaches keeps backing up
// over /dl without an MQTT password, exactly as before. Whether HTTP reaches it is settled by one bounded Status 11
// right before the decision when nothing fresh is on record; a fresh answer on record needs no probe, and the
// backup's own 20 s download timeout is never the probe.
func TestHTTPUnknownIsSettledByOneProbeBeforeTheBackup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		httpOK   bool
		probeErr error
		probes   int
		dl       int
	}{
		{"HTTP answered lately: no probe", true, nil, 0, 1},
		{"HTTP unknown, the probe answers", false, nil, 1, 1},
		{"HTTP unknown, the probe gets no answer", false, tasmota.ErrTimeout, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDLRig(t)
			g.mem.Clear(7)
			g.f.httpOK = tc.httpOK
			g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone
			g.http.err = tc.probeErr
			got, err := g.r.DownloadConfig(t.Context(), g.target())
			if g.http.n() != tc.probes || g.http.dl != tc.dl || len(g.dl.pws) != 0 {
				t.Fatalf("HTTP commands %v, /dl %d, FileDownload %d; want %d probes, %d /dl, no FileDownload", g.http.calls, g.http.dl, len(g.dl.pws), tc.probes, tc.dl)
			}
			if tc.dl == 1 && (err != nil || string(got) != "HTTPDUMP") {
				t.Fatalf("%q, %v: a device HTTP reaches backs up over /dl", got, err)
			}
		})
	}
}

// Why (#997 (3)): when the backup went over HTTP because MQTT could not carry it, and HTTP then fails, the error keeps
// the MQTT reason: the run, the Backups table and the audit name the missing password (or whatever blocks MQTT), not
// only the HTTP failure. errors.Is still finds the HTTP error, so callers that decide by it are unchanged. HTTP chosen
// for another reason (MQTT commands unanswered) has no MQTT reason to add.
func TestAnHTTPFailureAfterMQTTWasBlockedKeepsTheMQTTReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*dlRig)
		why  string // "" : the HTTP error stays as it is
	}{
		{"no password", func(g *dlRig) { g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone }, "enter the device's MQTT password to back up over MQTT"},
		{"GroupTopic unread", func(g *dlRig) { g.f.groupKnown = false }, "GroupTopic has not been read recently"},
		{"MQTT commands unanswered", func(g *dlRig) { g.r.unanswered.Fail(7, HTTPFailUnreachable) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newDLRig(t)
			g.mem.Clear(7)
			g.f.httpOK = true
			tc.mod(g)
			g.http.err = tasmota.ErrTimeout
			_, err := g.r.DownloadConfig(t.Context(), g.target())
			if !errors.Is(err, tasmota.ErrTimeout) || g.http.dl != 1 {
				t.Fatalf("%v, /dl %d: the HTTP error must still be found", err, g.http.dl)
			}
			var blocked *BackupBlockedError
			if tc.why == "" {
				if errors.As(err, &blocked) {
					t.Fatalf("%v: no MQTT reason to add", err)
				}
				return
			}
			if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, tc.why) || !blocked.Attempted {
				t.Fatalf("%v: want the MQTT reason %q, and Attempted (/dl was sent)", err, tc.why)
			}
			if !strings.Contains(blocked.Reason, "did not answer the backup over HTTP in time") {
				t.Errorf("reason %q: must say what HTTP did", blocked.Reason)
			}
		})
	}
}
