package devicecmd

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// Why (#826, CLAUDE.md safety rule 4): a console command whose arguments the audit hides goes over the transport its
// confirmation named (transport.WithRoute), and over no other: when the router's route is not the announced one,
// nothing is sent and the console asks again. In every case the secret stays out of the audit, the log and the error
// the page shows, and the audit says which transport carried it ("none" for the refusal).
func TestASecretGoesOnlyOverTheAnnouncedRouteAndNeverReachesTheAuditOrLog(t *testing.T) {
	const secret = "s3cret-826"
	for _, tc := range []struct {
		name       string
		onBroker   bool // the device is usable on our broker: the router's route is MQTT, else HTTP
		announced  transport.Path
		wantErr    bool
		via        string
		http, mqtt int32
	}{
		{"announced MQTT, route MQTT", true, transport.MQTT, false, "mqtt", 0, 1},
		{"announced HTTP, route HTTP", false, transport.HTTP, false, "http", 1, 0},
		{"announced HTTP, route MQTT", true, transport.HTTP, true, "none", 0, 0},
		{"announced MQTT, route HTTP", false, transport.MQTT, true, "none", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := model.Device{ID: 7, Address: "192.0.2.7", Topic: "plug"}
			if tc.onBroker {
				dev.MQTT = model.MQTTOurs
			}
			mem := transport.NewMemory(time.Minute, time.Now)
			var log bytes.Buffer
			var httpCalls atomic.Int32
			mq := &countingMQTT{}
			r := transport.NewRouter(transport.Options{HTTP: countingHTTP{n: &httpCalls}, Devices: routerDevices{devices{7: dev}}, Facts: stubFacts{mem: mem}, Memory: mem,
				Log: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))})
			r.SetMQTT(mq)
			cmd := "WebPassword " + secret
			if got, want := r.TypedRoute(t.Context(), 7, cmd), map[bool]transport.Path{true: transport.MQTT, false: transport.HTTP}[tc.onBroker]; got != want {
				t.Fatalf("TypedRoute = %s, want %s", got, want)
			}
			rec := &recorder{}
			svc := New(Options{Devices: devices{7: dev}, Client: r, Audit: rec})
			_, err := svc.Run(transport.WithRoute(t.Context(), tc.announced), admin(), 7, cmd)
			if (err != nil) != tc.wantErr || tc.wantErr && !errors.Is(err, transport.ErrRouteChanged) {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if h, m := httpCalls.Load(), mq.n.Load(); h != tc.http || m != tc.mqtt {
				t.Fatalf("http=%d mqtt=%d, want http=%d mqtt=%d", h, m, tc.http, tc.mqtt)
			}
			ev := rec.all()
			if len(ev) != 1 || !strings.Contains(ev[0].Detail, `"via":"`+tc.via+`"`) {
				t.Fatalf("audit = %+v, want via %s", ev, tc.via)
			}
			for what, s := range map[string]string{"audit detail": ev[0].Detail, "audit result": ev[0].Result, "log": log.String(), "error": errText(err)} {
				if strings.Contains(s, secret) {
					t.Errorf("the secret reached the %s: %s", what, s)
				}
			}
		})
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Why (#826): a command the MQTT channel never publishes, typed while the device's HTTP is known to fail, goes nowhere
// (its route is None). The audit must say so ("via":"none"), not name HTTP, a transport that never carried it.
func TestACommandThatGoesNowhereIsAuditedAsSentNowhere(t *testing.T) {
	const secret = "s3cret-826"
	dev := model.Device{ID: 7, Address: "192.0.2.7", Topic: "plug", MQTT: model.MQTTOurs}
	mem := transport.NewMemory(time.Minute, time.Now)
	mem.Fail(7, transport.HTTPFailUnreachable)
	var httpCalls atomic.Int32
	mq := &countingMQTT{}
	r := transport.NewRouter(transport.Options{HTTP: countingHTTP{n: &httpCalls}, Devices: routerDevices{devices{7: dev}}, Facts: stubFacts{mem: mem}, Memory: mem})
	r.SetMQTT(mq)
	cmd := "Rule1 ON Power1#State DO WebPassword " + secret + " ENDON"
	if got := r.TypedRoute(t.Context(), 7, cmd); got != transport.None {
		t.Fatalf("TypedRoute = %s, want none", got)
	}
	rec := &recorder{}
	svc := New(Options{Devices: devices{7: dev}, Client: r, Audit: rec})
	_, err := svc.Run(transport.WithRoute(t.Context(), transport.None), admin(), 7, cmd)
	if err == nil || httpCalls.Load() != 0 || mq.n.Load() != 0 {
		t.Fatalf("err=%v http=%d mqtt=%d, want an error and nothing sent", err, httpCalls.Load(), mq.n.Load())
	}
	ev := rec.all()
	if len(ev) != 1 || !strings.Contains(ev[0].Detail, `"via":"none"`) {
		t.Fatalf("audit = %+v, want via none", ev)
	}
	if strings.Contains(ev[0].Detail, secret) || strings.Contains(errText(err), secret) {
		t.Errorf("the secret reached the audit or the error: %+v %v", ev[0], err)
	}
}
