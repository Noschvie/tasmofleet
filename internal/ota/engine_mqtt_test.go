package ota

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// fakePaths answers every device with one Paths value.
type fakePaths struct {
	paths transport.Paths
	err   error
	asked []PathQuery
}

func (f *fakePaths) UpdatePath(_ context.Context, _ model.Device, q PathQuery) (transport.Paths, error) {
	f.asked = append(f.asked, q)
	return f.paths, f.err
}

func pathsOf(p transport.Path, text string) transport.Paths {
	return transport.Paths{Update: p, Reasons: []transport.Reason{{Action: transport.ActUpdate, Path: p, Text: text}}}
}

// pathSpy records the path every device call of the engine was pinned to.
type pathSpy struct {
	*fakeAPI
	mu   sync.Mutex
	seen map[string]int
}

func (s *pathSpy) note(ctx context.Context, what string) {
	p, ok := transport.PathFrom(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	if ok {
		s.seen[what+"/"+string(p)]++
	} else {
		s.seen[what+"/unpinned"]++
	}
}

func (s *pathSpy) ProbeIdentity(ctx context.Context, d model.Device) (model.Identity, string, int64, error) {
	s.note(ctx, "probe_identity")
	return s.fakeAPI.ProbeIdentity(ctx, d)
}
func (s *pathSpy) Probe(ctx context.Context, d model.Device) (model.Identity, int64, error) {
	s.note(ctx, "probe")
	return s.fakeAPI.Probe(ctx, d)
}
func (s *pathSpy) SetOtaURL(ctx context.Context, d model.Device, u string) error {
	s.note(ctx, "set_ota_url")
	return s.fakeAPI.SetOtaURL(ctx, d, u)
}
func (s *pathSpy) Upgrade(ctx context.Context, d model.Device) (string, error) {
	s.note(ctx, "upgrade")
	return s.fakeAPI.Upgrade(ctx, d)
}

// only reports whether every recorded call (and at least one) carried the pin p.
func (s *pathSpy) only(p transport.Path) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, c := range s.seen {
		if !strings.HasSuffix(k, "/"+string(p)) {
			return false
		}
		n += c
	}
	return n > 0
}

func newPathHarness(t *testing.T, fp *fakePaths) (*harness, *pathSpy) {
	t.Helper()
	h := newHarness(t, 2, plugDevice())
	spy := &pathSpy{fakeAPI: h.api}
	h.e = NewEngine(Options{Machine: testCfg(), MaxParallel: 2, Power: h.power, Paths: fp}, h.st, h.st, spy, h.fw, h.backups, h.clk, nil)
	h.e.sel = h.sel
	return h, spy
}

// Why (D2): an update with no path must not become a job that fails later on a device that was never
// reachable. Submit refuses with the chooser's sentence and creates nothing.
func TestSubmitRefusesWithoutUpdatePath(t *testing.T) {
	fp := &fakePaths{paths: pathsOf(transport.None, "no path: the device reaches no firmware server TasmoFleet can serve")}
	h, _ := newPathHarness(t, fp)
	_, err := h.e.Submit(context.Background(), "alice", []Request{{DeviceID: 1, Release: "16.0.0", SkipBackup: true}})
	var re *RefusedError
	if !errors.As(err, &re) || len(re.Refused) != 1 || !strings.Contains(re.Refused[0].Reason, "reaches no firmware server") {
		t.Fatalf("err = %v, want a refusal that carries the reason", err)
	}
	if jobs, _ := h.st.ListJobs(context.Background(), store.JobFilter{DeviceID: 1}); len(jobs) != 0 {
		t.Fatalf("a refused update created %d job(s)", len(jobs))
	}
	if h.rec.count("upgrade") != 0 {
		t.Fatal("Upgrade sent for a device without a path")
	}
}

// Why (D9, D5): the path is chosen once, stored with the job, and pins every device call of the job.
func TestJobStoresTheChosenPathAndPinsEveryCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		path transport.Path
		want model.Transport
		pin  transport.Path
	}{
		{"download over MQTT", transport.DownloadMQTT, model.TransportMQTT, transport.MQTT},
		{"download over HTTP", transport.DownloadHTTP, model.TransportHTTP, transport.HTTP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, spy := newPathHarness(t, &fakePaths{paths: pathsOf(tc.path, "because")})
			id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
			if j := h.job(t, id); j.Transport != tc.want {
				t.Fatalf("job transport = %q, want %q", j.Transport, tc.want)
			}
			spy.seen = nil // the preview probe is unpinned on purpose: it is what chooses
			h.run(t, h.e)
			if j := h.job(t, id); j.State != model.JobSucceeded {
				t.Fatalf("job %s %q", j.State, j.LastError)
			}
			if !spy.only(tc.pin) {
				t.Fatalf("calls %v, want all pinned to %s", spy.seen, tc.pin)
			}
		})
	}
}

// Why: a restarted process has no chooser state; the job continues on the path it started with.
func TestResumedJobKeepsItsTransport(t *testing.T) {
	h, spy := newPathHarness(t, &fakePaths{paths: pathsOf(transport.DownloadMQTT, "because")})
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
	spy.seen = nil
	// "restart": a new engine without any chooser runs the stored job
	h.e = NewEngine(Options{Machine: testCfg(), MaxParallel: 2, Power: h.power}, h.st, h.st, spy, h.fw, h.backups, h.clk, nil)
	h.e.sel = h.sel
	h.run(t, h.e)
	if j := h.job(t, id); j.State != model.JobSucceeded || j.Transport != model.TransportMQTT {
		t.Fatalf("job %s transport %q %q", j.State, j.Transport, j.LastError)
	}
	if !spy.only(transport.MQTT) {
		t.Fatalf("calls %v, want all pinned to mqtt", spy.seen)
	}
}

// Why (D9): over MQTT the device downloads. A push request on such a device must not stay a push: the
// job stores the download and the row says why.
func TestMQTTPathTurnsAPushIntoTheDownload(t *testing.T) {
	e := &Engine{paths: &fakePaths{paths: pathsOf(transport.DownloadMQTT, "over MQTT")}}
	it := PreviewItem{Decision: "upgrade", Delivery: model.DeliveryPush, ViaSafeboot: true}
	e.choosePath(context.Background(), model.Device{}, Request{Delivery: model.DeliveryPush}, planResult{delivery: model.DeliveryPush, viaSafeboot: true}, &it)
	if it.Delivery != model.DeliveryPull || it.ViaSafeboot || it.Transport != model.TransportMQTT || !strings.Contains(it.DeliveryNote, "downloads the file") {
		t.Fatalf("item %+v", it)
	}
}

// Why (D2/D19): the HTTP decision handed to the chooser is today's, unchanged: a push that fell back is
// reported as the download with the engine's own note.
func TestHTTPPushDecisionHandedToTheChooser(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    planResult
		req  model.Delivery
		want transport.Path
		note string
	}{
		{"pull", planResult{delivery: model.DeliveryPull}, model.DeliveryPull, transport.DownloadHTTP, ""},
		{"direct push", planResult{delivery: model.DeliveryPush}, model.DeliveryPush, transport.PushHTTP, ""},
		{"push through safeboot", planResult{delivery: model.DeliveryPush, viaSafeboot: true}, model.DeliveryPush, transport.PushSafeboot, ""},
		{"push that fell back", planResult{delivery: model.DeliveryPull, fallback: "ESP8266 cannot take an upload"}, model.DeliveryPush, transport.DownloadHTTP, "ESP8266 cannot take an upload"},
	} {
		got, note := httpPushDecision(tc.p, tc.req)
		if got != tc.want || note != tc.note {
			t.Errorf("%s: %s %q, want %s %q", tc.name, got, note, tc.want, tc.note)
		}
	}
}

// Why (D9): the report says the device cannot connect. One Upgrade 1 only, and the history names the
// reason, so the person (and the FWReach learning) can tell it from a download that merely failed.
func TestEngineConnectionFailedReportEndsTheJobAfterOneUpgrade(t *testing.T) {
	dev := plugDevice()
	dev.ignore = true // accepts Upgrade, never flashes
	h := newHarness(t, 2, dev)
	h.api.onUpdate = func(model.DeviceID) { h.e.Hint(1, "Failed HTTP error: connection failed") }
	id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
	h.run(t, h.e)
	j := h.job(t, id)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, ErrTextFWUnreachable) {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if n := h.rec.count("upgrade"); n != 1 {
		t.Fatalf("Upgrade sent %d times, want 1", n)
	}
	evs, _ := h.st.JobEvents(context.Background(), id)
	last := evs[len(evs)-1]
	if last.Event != string(EvUpgradeReport) || !FWUnreachableReport(last.Detail) {
		t.Fatalf("last event %+v: the notifier learns FWReach from it", last)
	}
}

func TestFWUnreachableReport(t *testing.T) {
	for text, want := range map[string]bool{
		"Failed HTTP error: connection failed": true,
		"Failed Connection Failed":             true,
		"Failed HTTP error: 404":               false,
		"Successful":                           false,
		"connection failed":                    false, // not a UPGRADE failure report
	} {
		if got := FWUnreachableReport(text); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}

func TestDeclaredLanguage(t *testing.T) {
	for in, want := range map[string]string{"en": "", "EN": "", " de ": "DE", "PL": "PL"} {
		if got, err := declaredLanguage(in); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "deu", "4M", "d1"} {
		if _, err := declaredLanguage(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

// Why (D10): a language the person chose is the language of the identity (Select then keeps it); the
// retry of such a job carries the choice, a job whose language was read does not need it.
func TestRetryRequestKeepsTheLanguageChoice(t *testing.T) {
	j := model.Job{DeviceID: 1, IdentityBefore: model.Identity{Language: "DE", LanguageKnown: true}}
	if r := retryRequest(j); r.Language != "DE" {
		t.Fatalf("language %q", r.Language)
	}
	j.IdentityBefore.Language = ""
	if r := retryRequest(j); r.Language != fwselect.LanguageEnglish {
		t.Fatalf("English must be carried as an explicit choice, got %q", r.Language)
	}
	if r := retryRequest(model.Job{DeviceID: 1}); r.Language != "" {
		t.Fatalf("no language was known: %q", r.Language)
	}
}

// Why: the transport's write rule treats a GroupTopic read before the device's "last probe" as stale. A
// job over MQTT that stamped the identity itself would block its own OtaUrl write, so it stores none;
// an HTTP job still does (its probe is the HTTP one that fills the language).
func TestMQTTJobDoesNotStampTheStoredIdentity(t *testing.T) {
	for _, tc := range []struct {
		path      transport.Path
		wantStamp bool
	}{{transport.DownloadMQTT, false}, {transport.DownloadHTTP, true}} {
		h, _ := newPathHarness(t, &fakePaths{paths: pathsOf(tc.path, "because")})
		id := submitOne(t, h, Request{DeviceID: 1, Release: "16.0.0", SkipBackup: true})
		h.run(t, h.e)
		if j := h.job(t, id); j.State != model.JobSucceeded {
			t.Fatalf("job %s %q", j.State, j.LastError)
		}
		d, _ := h.st.GetDevice(context.Background(), 1)
		if stamped := !d.Identity.DetectedAt.IsZero(); stamped != tc.wantStamp {
			t.Errorf("%s: identity stamped = %v, want %v", tc.path, stamped, tc.wantStamp)
		}
	}
}
