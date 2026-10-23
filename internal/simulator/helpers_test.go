package simulator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const fixtures = "../../testdata/fixtures"

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newTestFleet starts a fleet whose OTA timeline runs at 2 % of real time:
// the shape of the timeline is what the tests check, not its length.
func newTestFleet(t *testing.T, mqttURL string, x Extra) Fleet {
	t.Helper()
	if x.TimeScale == 0 {
		x.TimeScale = 0.02
	}
	f, err := NewFleetWith(FleetOptions{FixturesDir: fixtures, MQTTURL: mqttURL, TelePeriod: 100 * time.Millisecond}, x)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func add(t *testing.T, f Fleet, spec DeviceSpec) Controller {
	t.Helper()
	d, err := f.Add(ctxT(t), spec)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := Control(d)
	if !ok {
		t.Fatal("device is not a Controller")
	}
	return c
}

// cmWith sends one /cm command with a client timeout.
func cmWith(d Device, cmnd string, timeout time.Duration, extra url.Values) (body string, code int, err error) {
	q := url.Values{"cmnd": {cmnd}}
	for k, v := range extra {
		q[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+d.Addr()+"/cm?"+q.Encode(), nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, err
}

func cm(t *testing.T, d Device, cmnd string) string {
	t.Helper()
	b, code, err := cmWith(d, cmnd, 3*time.Second, nil)
	if err != nil || code != 200 {
		t.Fatalf("%s: code %d err %v", cmnd, code, err)
	}
	return b
}

// firmwareServer serves synthetic images by file name and counts hits.
type firmwareServer struct {
	*httptest.Server
	hits map[string]int
}

func newFirmwareServer(t *testing.T, files map[string]string) *firmwareServer { // name -> version
	t.Helper()
	fs := &firmwareServer{hits: map[string]int{}}
	imgs := map[string][]byte{}
	for name, ver := range files {
		b, err := NewImage(fixtures, name, ver)
		if err != nil {
			t.Fatal(err)
		}
		imgs[name] = b
	}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		fs.hits[name]++
		b, ok := imgs[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func settled(t *testing.T, c Controller, boots int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := c.WaitFor(ctx, func(c Controller) bool { return c.Boots() >= boots && c.Phase() == PhaseRun })
	if err != nil {
		t.Fatalf("device did not finish %d reboot(s): phase %s boots %d history %v", boots, c.Phase(), c.Boots(), c.History())
	}
}

func phases(c Controller) []Phase {
	var out []Phase
	for _, h := range c.History() {
		out = append(out, h.Phase)
	}
	return out
}

func cmWithPath(d Device, path, query string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+d.Addr()+path+"?"+query, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, err
}
