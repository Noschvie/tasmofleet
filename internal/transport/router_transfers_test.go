package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// Why (#607, the product owner's transport rule of 2026-11-14): a device usable on TasmoFleet's broker is backed up
// over MQTT first, also while HTTP works (unknown or answering), so every backup (scheduled, before an update,
// manual, bulk, the safety and check backups of a restore) takes the same way as every other write. HTTP's /dl is
// not asked at all, and the audit's "via" says mqtt.
func TestABackupGoesOverMQTTFirstWhileHTTPWorks(t *testing.T) {
	for name, httpOK := range map[string]bool{"HTTP unknown": false, "HTTP answered lately": true} {
		t.Run(name, func(t *testing.T) {
			g := newDLRig(t)
			g.mem.Clear(7)
			g.f.httpOK = httpOK
			ctx, tr := WithTrace(t.Context())
			got, err := g.r.DownloadConfig(ctx, g.target())
			if err != nil || string(got) != "DUMP" || tr.Via() != "mqtt" {
				t.Fatalf("%q, %v, via %s", got, err, tr.Via())
			}
			if len(g.dl.pws) != 1 || g.http.dl != 0 {
				t.Fatalf("MQTT downloads %d, HTTP downloads %d: the backup must go over MQTT only", len(g.dl.pws), g.http.dl)
			}
		})
	}
}

// Why (#607): HTTP carries a backup only when MQTT cannot. A device without a usable MQTT password (none set, refused
// before) or whose MQTT commands just went unanswered is backed up over /dl while HTTP works, and nothing is
// published: a FileDownload start carries the password, and an unanswered device would only time out.
func TestABackupMQTTCannotCarryGoesOverHTTPWhileHTTPWorks(t *testing.T) {
	for name, mod := range map[string]func(*dlRig){
		"no password":       func(g *dlRig) { g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone },
		"password refused":  func(g *dlRig) { g.devs.d.MQTTPasswordBadAt = time.Now() },
		"MQTT unanswered":   func(g *dlRig) { g.r.unanswered.Fail(7, HTTPFailUnreachable) },
		"MQTT conn changed": func(g *dlRig) { g.r.MQTTConnChanged().Fail(7, HTTPFailUnreachable) },
	} {
		t.Run(name, func(t *testing.T) {
			g := newDLRig(t)
			g.mem.Clear(7)
			mod(g)
			ctx, tr := WithTrace(t.Context())
			got, err := g.r.DownloadConfig(ctx, g.target())
			if err != nil || string(got) != "HTTPDUMP" || tr.Via() != "http" || len(g.dl.pws) != 0 {
				t.Fatalf("%q, %v, via %s, MQTT downloads %d", got, err, tr.Via(), len(g.dl.pws))
			}
		})
	}
}

// Why (#607, T2): a backup over MQTT first that provably published nothing (the channel refused it before the first
// message, the bus was down, the device's last will said Offline) is sent over HTTP once while HTTP is not failing,
// as a write is. Nothing reached the device, so this is no second send.
func TestABackupMQTTDidNotPublishGoesOverHTTPOnce(t *testing.T) {
	for name, derr := range map[string]error{
		"refused before a publish": fmt.Errorf("%w: %w", mqttcmd.ErrRefused, mqttcmd.ErrFirmwarePending),
		"bus down":                 fmt.Errorf("%w: not connected", tasmota.ErrUnreachable),
	} {
		t.Run(name, func(t *testing.T) {
			g := newDLRig(t)
			g.mem.Clear(7)
			g.dl.derr = derr
			got, err := g.r.DownloadConfig(t.Context(), g.target())
			if err != nil || string(got) != "HTTPDUMP" || g.http.dl != 1 || len(g.dl.pws) != 1 {
				t.Fatalf("%q, %v, HTTP downloads %d, MQTT downloads %d", got, err, g.http.dl, len(g.dl.pws))
			}
		})
	}
}

// Why (#607, T3; "a transfer that published anything never moves to the other transport mid-run"): once anything of a
// FileDownload reached the broker (the opening abort, the start, a chunk request), the device's transfer state is
// touched and the password may have been checked; the backup fails with MQTT's error and HTTP is never asked, also
// while HTTP works. The next backup decides again.
func TestABackupThatPublishedNeverMovesToHTTP(t *testing.T) {
	for name, derr := range map[string]error{
		"refused after the opening abort": fmt.Errorf("%w: %w: %w", mqttcmd.ErrPublished, mqttcmd.ErrRefused, mqttcmd.ErrFirmwarePending),
		"unreachable after a publish":     fmt.Errorf("%w: %w", mqttcmd.ErrPublished, tasmota.ErrUnreachable),
		"timed out":                       fmt.Errorf("%w: %w", mqttcmd.ErrPublished, tasmota.ErrTimeout),
		"transfer broke":                  mqttcmd.ErrTransfer,
		"password refused":                mqttcmd.ErrPasswordRefused,
	} {
		t.Run(name, func(t *testing.T) {
			g := newDLRig(t)
			g.mem.Clear(7)
			g.dl.derr = derr
			ctx, tr := WithTrace(t.Context())
			if _, err := g.r.DownloadConfig(ctx, g.target()); err == nil || g.http.dl != 0 {
				t.Fatalf("%v, HTTP downloads %d: a transfer that published must not go on over HTTP", err, g.http.dl)
			}
			if tr.Via() != "mqtt" {
				t.Errorf("via %s: a transfer that published crossed the broker", tr.Via())
			}
		})
	}
}

// Why (#607, the rule's "no reply on MQTT"): a FileDownload that got no answer fails its run and never moves to HTTP
// (it published), but like an unanswered read or write it sends the device's next actions over HTTP for a while. A
// person who presses "Back up" again right after must not go over MQTT into the same silence while HTTP works.
func TestAnUnansweredBackupOverMQTTSendsTheNextOneOverHTTP(t *testing.T) {
	g := newDLRig(t)
	g.mem.Clear(7)
	g.dl.derr = fmt.Errorf("%w: %w", mqttcmd.ErrPublished, tasmota.ErrTimeout)
	if _, err := g.r.DownloadConfig(t.Context(), g.target()); !errors.Is(err, tasmota.ErrTimeout) || g.http.dl != 0 {
		t.Fatalf("%v, HTTP downloads %d: the unanswered run fails without HTTP", err, g.http.dl)
	}
	g.dl.derr = nil
	got, err := g.r.DownloadConfig(t.Context(), g.target())
	if err != nil || string(got) != "HTTPDUMP" || g.http.dl != 1 || len(g.dl.pws) != 1 {
		t.Fatalf("%q, %v, HTTP downloads %d, MQTT downloads %d: the next backup goes over /dl", got, err, g.http.dl, len(g.dl.pws))
	}
}

// Why (#607; Tasmota has no file transfer over MQTT, see "What still uses HTTP" in docs/security/mqtt-transport.md):
// file calls go over HTTP also for a backup or restore pinned to MQTT, so a backup over MQTT first keeps its files
// archive and a restore over MQTT writes its files, while HTTP is not known to fail. The pin keeps the dump on its
// transport; the files have only one.
func TestFileCallsGoOverHTTPUnderAnMQTTPinWhileHTTPIsNotFailing(t *testing.T) {
	r, h, tg, _ := newFSRig(t)
	ctx, tr := WithTrace(WithPath(t.Context(), MQTT))
	if _, err := r.FSInfo(ctx, tg); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ListFiles(ctx, tg, "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DownloadFile(ctx, tg, "/a", 10); err != nil {
		t.Fatal(err)
	}
	if err := r.UploadFile(ctx, tg, "/a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if len(h.fsCalls) != 4 || tr.Via() != "http" {
		t.Fatalf("HTTP saw %v, via %s", h.fsCalls, tr.Via())
	}
}

// Why (#324, #607): while the device's HTTP is known to fail, a file call sends nothing and says that files need HTTP
// (ErrFilesNeedHTTP), whatever the pin: each call would only wait for a timeout, and the backup then says why it holds
// no files instead of failing.
func TestFileCallsSendNothingWhileHTTPIsKnownToFail(t *testing.T) {
	r, h, tg, mem := newFSRig(t)
	mem.Fail(7, HTTPFailUnreachable)
	for _, pin := range []string{"none", "mqtt"} {
		ctx := t.Context()
		if pin == "mqtt" {
			ctx = WithPath(ctx, MQTT)
		}
		for name, err := range map[string]error{
			"info":   func() error { _, e := r.FSInfo(ctx, tg); return e }(),
			"list":   func() error { _, e := r.ListFiles(ctx, tg, "/"); return e }(),
			"get":    func() error { _, e := r.DownloadFile(ctx, tg, "/a", 1); return e }(),
			"upload": r.UploadFile(ctx, tg, "/a", nil),
		} {
			if !errors.Is(err, ErrFilesNeedHTTP) {
				t.Errorf("pin %s, %s: %v, want ErrFilesNeedHTTP", pin, name, err)
			}
		}
	}
	if len(h.fsCalls) != 0 {
		t.Errorf("HTTP was reached: %v", h.fsCalls)
	}
}

// failingFacts cannot gather facts after its first n calls.
type failingFacts struct {
	inner *facts
	n     *int
}

func (f failingFacts) HTTPState(id model.DeviceID) HTTPState { return f.inner.HTTPState(id) }
func (f failingFacts) FactsFor(ctx context.Context, d model.Device) (Facts, error) {
	if *f.n <= 0 {
		return Facts{}, errors.New("facts unavailable")
	}
	*f.n--
	return f.inner.FactsFor(ctx, d)
}

// Why (#607): without a judgement the backup takes the established path and never guesses MQTT: no command channel,
// or facts that cannot be read, mean /dl; and after an HTTP request that provably never left, MQTT gets its chance only
// on facts the router could read (else HTTP's error stands).
func TestABackupWithoutAJudgementGoesOverHTTP(t *testing.T) {
	t.Run("no command channel", func(t *testing.T) {
		g := newDLRig(t)
		g.mem.Clear(7)
		g.r.SetMQTT(nil)
		if got, err := g.r.DownloadConfig(t.Context(), g.target()); err != nil || string(got) != "HTTPDUMP" || len(g.dl.pws) != 0 {
			t.Fatalf("%q, %v, MQTT downloads %d", got, err, len(g.dl.pws))
		}
	})
	t.Run("no facts", func(t *testing.T) {
		g := newDLRig(t)
		g.mem.Clear(7)
		n := 0
		g.r.o.Facts = failingFacts{inner: g.f, n: &n}
		if got, err := g.r.DownloadConfig(t.Context(), g.target()); err != nil || string(got) != "HTTPDUMP" || len(g.dl.pws) != 0 {
			t.Fatalf("%q, %v, MQTT downloads %d", got, err, len(g.dl.pws))
		}
	})
	t.Run("no facts after HTTP sent nothing", func(t *testing.T) {
		g := newDLRig(t)
		g.mem.Clear(7)
		g.r.unanswered.Fail(7, HTTPFailUnreachable) // HTTP first
		g.http.err = tasmota.ErrForbiddenAddr
		n := 1
		g.r.o.Facts = failingFacts{inner: g.f, n: &n}
		if _, err := g.r.DownloadConfig(t.Context(), g.target()); !errors.Is(err, tasmota.ErrForbiddenAddr) || len(g.dl.pws) != 0 {
			t.Fatalf("%v, MQTT downloads %d", err, len(g.dl.pws))
		}
	})
}

// Why (#607, #261): a device MQTT cannot back up (no password) whose HTTP provably sent nothing has no way left; the
// error says why MQTT could not take the backup, so the page names the fix.
func TestABackupNeitherTransportCanCarrySaysWhyMQTTCouldNot(t *testing.T) {
	g := newDLRig(t)
	g.mem.Clear(7)
	g.devs.d.MQTTPasswordMode = model.MQTTPasswordNone
	g.http.err = tasmota.ErrForbiddenAddr
	_, err := g.r.DownloadConfig(t.Context(), g.target())
	var blocked *BackupBlockedError
	if !errors.As(err, &blocked) || !strings.Contains(blocked.Reason, "MQTT password") || len(g.dl.pws) != 0 {
		t.Fatalf("%v, MQTT downloads %d", err, len(g.dl.pws))
	}
}
