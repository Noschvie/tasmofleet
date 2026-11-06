package app

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// Why (#497): a container stop with a browser tab open must look like an orderly stop to the
// supervisor. http.Server.Shutdown does not cancel the contexts of running requests, so an open
// event stream held it until the grace period ran out and Run returned "context deadline exceeded"
// (exit status 1, read as a crash).
func TestRunStopsQuicklyAndCleanlyWithAnOpenEventStream(t *testing.T) {
	addr := freeAddr(t)
	cfg, err := config.Load([]string{
		"TASMOFLEET_DATA_DIR=" + t.TempDir(), "TASMOFLEET_CONFIG_FILE=/dev/null", "TASMOFLEET_AUTH_MODE=header",
		"TASMOFLEET_DEVICE_NETWORKS=127.0.0.0/8", "TASMOFLEET_TRUSTED_PROXIES=127.0.0.0/8", "TASMOFLEET_LISTEN=" + addr,
		"TASMOFLEET_ALLOW_INSECURE_HTTP=true", "TASMOFLEET_ADMIN_GROUPS=admins",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()

	req, err := http.NewRequestWithContext(t.Context(), "GET", "http://"+addr+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream") // what EventSource sends
	req.Header.Set("X-authentik-username", "tester")
	req.Header.Set("X-authentik-groups", "admins")
	var resp *http.Response
	for deadline := time.Now().Add(10 * time.Second); ; {
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not come up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("/events: %d %q, want an open event stream", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatalf("event stream not open: %v", err)
	}

	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after a stop with an open stream = %v, want nil", err)
		}
	case <-time.After(shutdownGrace / 2):
		t.Fatal("Run did not return within half the shutdown grace with an open event stream")
	}
}

// Why (#497): the store is closed right after run returns. An early error return used to leave
// only through cancelBG, so health, MQTT and identity goroutines could still read and write a
// store that was being closed. Every exit path must wait for the background tasks.
func TestRunWaitsForBackgroundTasksWhenASetupStepFails(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0") // the UI listener cannot bind: an early error after the tasks started
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	cfg, err := config.Load([]string{
		"TASMOFLEET_DATA_DIR=" + t.TempDir(), "TASMOFLEET_CONFIG_FILE=/dev/null", "TASMOFLEET_AUTH_MODE=local",
		"TASMOFLEET_DEVICE_NETWORKS=127.0.0.0/8", "TASMOFLEET_LISTEN=" + taken.Addr().String(), "TASMOFLEET_ALLOW_INSECURE_HTTP=true",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := openServices(cfg, slog.New(slog.DiscardHandler), "test", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	a := &runtime{svc: svc, cfg: svc.Cfg, log: svc.Log}
	before := backgroundTasks() // other tests may leave some behind
	// The real tasks stop within microseconds of the cancel, mostly before anyone can look, so a
	// missing wait would seldom show. This task stops only once run waits for the background tasks:
	// a run that does not wait returns while it still runs, every time.
	release := make(chan struct{})
	a.goBG("held", func() error { <-release; return nil })
	ran := make(chan error, 1)
	go func() { ran <- a.run(t.Context()) }()
	stacks := make([]byte, 4<<20)
	for deadline := time.Now().Add(10 * time.Second); !runWaitsForBackgroundTasks(stacks); {
		select {
		case err := <-ran:
			close(release)
			t.Fatalf("run returned (%v) while a background task was still running; the store would close under it", err)
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("run neither returned nor waited for the background tasks")
		}
	}
	close(release)
	if err = <-ran; err == nil || !strings.Contains(err.Error(), "ui listener") {
		t.Fatalf("run = %v, want the listener error", err)
	}
	if n := backgroundTasks(); n > before {
		t.Fatalf("%d background tasks still running after run returned; the store would close under them", n-before)
	}
}

// backgroundTasks counts the goroutines currently inside a goBG task.
func backgroundTasks() int {
	buf := make([]byte, 4<<20)
	return strings.Count(string(buf[:goruntime.Stack(buf, true)]), "(*runtime).goBG.func1")
}

// runWaitsForBackgroundTasks reports whether a goroutine is inside run, waiting for the goBG tasks.
// buf holds the stack dump.
func runWaitsForBackgroundTasks(buf []byte) bool {
	for _, g := range strings.Split(string(buf[:goruntime.Stack(buf, true)]), "\n\n") {
		if strings.Contains(g, "sync.(*WaitGroup).Wait(") && strings.Contains(g, "(*runtime).run(") {
			return true
		}
	}
	return false
}

// Why (#497): a stop ends event streams at once, but must not cut a normal request half way: a
// Settings save sends one command per field on its request context, and a stop in the middle
// could apply MqttHost but not MqttPassword. Normal requests keep Shutdown's grace period.
func TestStopEndsEventStreamsButLetsANormalRequestFinish(t *testing.T) {
	stopCtx, stop := context.WithCancel(context.Background())
	defer stop()
	inSlow, streamOpen := make(chan struct{}), make(chan struct{})
	var slowCut atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(inSlow)
		select {
		case <-r.Context().Done():
			slowCut.Store(true)
		case <-time.After(300 * time.Millisecond):
		}
		_, _ = w.Write([]byte("done"))
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_ = http.NewResponseController(w).Flush()
		close(streamOpen)
		<-r.Context().Done()
	})
	srv := httptest.NewServer(endStreamsOnStop(stopCtx, mux))
	defer srv.Close()

	get := func(path, accept string) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL+path, nil)
		req.Header.Set("Accept", accept)
		return http.DefaultClient.Do(req)
	}
	slowRes := make(chan string, 1)
	go func() {
		resp, err := get("/slow", "text/html")
		if err != nil {
			slowRes <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		slowRes <- string(b)
	}()
	stream, err := get("/events", "text/event-stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	<-inSlow
	<-streamOpen

	stop()
	streamEnded := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, stream.Body); close(streamEnded) }()
	select {
	case <-streamEnded:
	case <-time.After(2 * time.Second):
		t.Fatal("the event stream did not end at stop")
	}
	if got := <-slowRes; got != "done" || slowCut.Load() {
		t.Fatalf("the normal request was cut by the stop: body %q, cut %v", got, slowCut.Load())
	}
}
