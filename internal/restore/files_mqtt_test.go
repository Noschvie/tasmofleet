package restore

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// The file system question of a restore over MQTT (#875): UfsType, UfsSize and UfsFree go to the device on its topic
// first, as the backup's do since #855/#865. The files themselves stay on HTTP.

// Topic answers to the Ufs commands for topicFS.
const (
	ufsPass   = ""       // the device on its topic answers (the simulator's file system)
	ufsNone   = "none"   // the build has no file system: UfsType answers "Unknown"
	ufsSilent = "silent" // the topic leaves the question unanswered
	ufsCancel = "cancel" // TasmoFleet shuts down while the question is asked
)

// topicFS is a credsWatch (every HTTP request is recorded while on) whose device answers the Ufs commands on its topic
// as reply says, and whose router knows the device's HTTP to fail when gated (transport.Router.FilesUsable).
type topicFS struct {
	*credsWatch
	reply  string
	gated  bool
	cancel context.CancelFunc // ufsCancel: cancels the caller's context during the question

	mu   sync.Mutex
	mqtt []string // the Ufs commands asked on the topic
}

func (f *topicFS) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if pin, _ := transport.PathFrom(ctx); pin != transport.HTTP && strings.HasPrefix(c, "Ufs") {
		f.mu.Lock()
		f.mqtt = append(f.mqtt, c)
		f.mu.Unlock()
		switch f.reply {
		case ufsNone:
			return nil, tasmota.ErrUnknownCommand
		case ufsSilent:
			return nil, tasmota.ErrTimeout
		case ufsCancel:
			f.cancel()
			return nil, ctx.Err()
		}
	}
	return f.credsWatch.Command(ctx, t, c)
}

func (f *topicFS) FilesUsable(tasmota.Target) error {
	if f.gated {
		return transport.ErrFilesNeedHTTP
	}
	return nil
}

func (f *topicFS) onTopic() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mqtt...)
}

// watchTopicFS makes the restore service's client a topicFS over a credsWatch that records from now on.
func watchTopicFS(t *testing.T, e *flowEnv, m *Manager, reply string) *topicFS {
	t.Helper()
	f := &topicFS{credsWatch: watchCreds(t, e, m), reply: reply}
	m.o.Client = f
	f.on.Store(true)
	return f
}

const whyNoFlash = "the device has no flash file system, so only the settings are restored"

// Why (#875): a restore over MQTT of a device whose build has no file system used to say "files need HTTP, and ..."
// whenever HTTP could not be used: the person was told to fix HTTP for files the device cannot hold at all. The
// device says on its topic that it has no file system; the confirmation page and the run say that, and nothing is
// asked over HTTP: no identity question, no file system question.
func TestARestoreOverMQTTOfADeviceWithoutAFileSystemSaysSoAndAsksNothingOverHTTP(t *testing.T) {
	for name, set := range map[string]func(*flowEnv, *topicFS){
		"no single HTTP address": func(e *flowEnv, _ *topicFS) {
			e.m.o.Resolve = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}, nil
			}
		},
		"HTTP known to fail":      func(_ *flowEnv, f *topicFS) { f.gated = true },
		"HTTP finds another host": func(e *flowEnv, _ *topicFS) { e.httpMAC.Store("aa:bb:cc:99:99:99") },
		"HTTP does not answer":    func(e *flowEnv, _ *topicFS) { e.httpDown.Store(true) },
	} {
		t.Run(name, func(t *testing.T) {
			e, a := filesFlow(t)
			m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
			f := watchTopicFS(t, e, m, ufsNone)
			set(e, f)
			as, err := m.Assess(t.Context(), a.ID)
			if err != nil || as.Files.Restores() || as.Files.Why != whyNoFlash {
				t.Fatalf("confirmation page: %+v, %v", as.Files, err)
			}
			if got := f.requests(); len(got) != 0 {
				t.Errorf("confirmation page asked over HTTP: %v", got)
			}
			if got := f.onTopic(); !slices.Contains(got, "UfsType") {
				t.Errorf("the topic was not asked: %v", got)
			}
			// The page listed the files when the device still had a file system; it was reflashed since.
			done := e.finish(t, e.startFiles(t, a.ID, 3).ID)
			if done.State != store.RestoreDone {
				t.Fatalf("job: %+v", done)
			}
			if files, _ := SplitDetail(done.Detail); files != "Files not restored: "+whyNoFlash {
				t.Errorf("the job must say the device has no file system: %q", done.Detail)
			}
			if up := e.cl.uploads(); len(up) != 0 {
				t.Errorf("files written: %v", up)
			}
			if got := f.requests(); len(got) != 0 {
				t.Errorf("the run asked over HTTP: %v", got)
			}
		})
	}
	t.Run("cancelled while the topic is asked", func(t *testing.T) {
		e, a := filesFlow(t)
		m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
		f := watchTopicFS(t, e, m, ufsCancel)
		ctx, cancel := context.WithCancel(transport.WithPath(t.Context(), transport.MQTT))
		defer cancel()
		f.cancel = cancel
		dev, err := e.st.GetDevice(t.Context(), e.dev)
		if err != nil {
			t.Fatal(err)
		}
		version := func(c context.Context) (string, error) { return m.liveVersion(c, dev) }
		_, err = m.planFiles(ctx, a.ID, tasmota.TargetFor(dev), version, m.filesRoute(Job{Transport: model.TransportMQTT}, dev))
		if err == nil || ctx.Err() == nil {
			t.Fatalf("a restore cancelled during the question must end with the cancellation: %v", err)
		}
		if got := f.requests(); len(got) != 0 {
			t.Errorf("asked over HTTP after the cancellation: %v", got)
		}
	})
}

// Why (#875, the transport rule): the file system question of a restore over MQTT goes to the device on its topic
// first; when the topic answers, nothing about the file system is asked over HTTP. When the topic leaves it
// unanswered it is asked over HTTP as before, and only after the HTTP identity check: the HTTP question carries the
// stored web password, which must never reach a host that was not shown to be this device.
func TestTheFileSystemQuestionOfAnMQTTRestoreGoesToTheTopicFirst(t *testing.T) {
	fsOverHTTP := func(reqs []string) []string {
		var out []string
		for _, r := range reqs {
			if r == "FSInfo" || strings.HasPrefix(r, "Ufs") {
				out = append(out, r)
			}
		}
		return out
	}
	t.Run("the topic answers", func(t *testing.T) {
		e, a := filesFlow(t)
		m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
		f := watchTopicFS(t, e, m, ufsPass)
		as, err := m.Assess(t.Context(), a.ID)
		if err != nil || !as.Files.Restores() || len(as.Files.Files) != 3 {
			t.Fatalf("confirmation page: %+v, %v", as.Files, err)
		}
		if got := fsOverHTTP(f.requests()); len(got) != 0 {
			t.Errorf("confirmation page asked the file system over HTTP: %v", got)
		}
		if got := f.onTopic(); !slices.Contains(got, "UfsType") {
			t.Errorf("the topic was not asked: %v", got)
		}
		done := e.finish(t, e.startFiles(t, a.ID, 3).ID)
		if done.State != store.RestoreDone || len(e.cl.uploads()) == 0 {
			t.Fatalf("job: %+v, uploads %v", done, e.cl.uploads())
		}
		if got := fsOverHTTP(f.requests()); len(got) != 0 {
			t.Errorf("the run asked the file system over HTTP: %v", got)
		}
	})
	// Guard (passes without #875 too): what the topic leaves unanswered goes to HTTP only after the identity check.
	t.Run("the topic is silent", func(t *testing.T) {
		e, a := filesFlow(t)
		m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
		f := watchTopicFS(t, e, m, ufsSilent)
		as, err := m.Assess(t.Context(), a.ID)
		if err != nil || !as.Files.Restores() || len(as.Files.Files) != 3 {
			t.Fatalf("confirmation page: %+v, %v", as.Files, err)
		}
		reqs := f.requests()
		if len(reqs) < 2 || reqs[0] != "Status 5" || slices.Index(reqs, "FSInfo") < 1 {
			t.Errorf("HTTP must ask the MAC first, then the file system: %v", reqs)
		}
	})
	// Guard (passes without #875 too): a silent topic and an HTTP known to fail leave the files out with HTTP's reason,
	// without one HTTP request and without failing the page.
	t.Run("the topic is silent and HTTP known to fail", func(t *testing.T) {
		e, a := filesFlow(t)
		m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
		f := watchTopicFS(t, e, m, ufsSilent)
		f.gated = true
		as, err := m.Assess(t.Context(), a.ID)
		if err != nil || as.Files.Restores() || as.Files.Why != whyFilesNoHTTP {
			t.Fatalf("confirmation page: %+v, %v", as.Files, err)
		}
		if got := f.requests(); len(got) != 0 {
			t.Errorf("HTTP was asked %v", got)
		}
	})
}
