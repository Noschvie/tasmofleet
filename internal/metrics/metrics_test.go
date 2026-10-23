package metrics

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func scrape(t *testing.T, h http.Handler, auth string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// Prometheus rejects exposition output that violates the text format, which
// would silently blank every dashboard; so the output is parsed line by line.
func TestExpositionIsValidTextFormat(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("x_total", "Things.", "kind")
	g := r.Gauge("x_gauge", "Level.", "dev")
	c.With("a").Inc()
	c.With("a").Add(2)
	c.With("b").Inc()
	g.With("p1").Set(1.5)
	_, body := scrape(t, r.Handler(), "")

	sample := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*(\{([a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\.)*",?)+\})? -?[0-9.e+-]+$`)
	typed := map[string]string{}
	helped := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# HELP "):
			helped[strings.Fields(line)[2]] = true
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(line)
			typed[f[2]] = f[3]
		case line == "":
			t.Errorf("empty line in exposition")
		default:
			if !sample.MatchString(line) {
				t.Errorf("not a valid sample line: %q", line)
			}
			name := line[:strings.IndexAny(line, "{ ")]
			if typed[name] == "" || !helped[name] {
				t.Errorf("sample %q lacks HELP/TYPE before it", name)
			}
		}
	}
	if typed["x_total"] != "counter" || typed["x_gauge"] != "gauge" {
		t.Errorf("types: %v", typed)
	}
	if !strings.Contains(body, `x_total{kind="a"} 3`) || !strings.Contains(body, `x_gauge{dev="p1"} 1.5`) {
		t.Errorf("values missing:\n%s", body)
	}
}

// Exposition must be stable so diffs and tests do not flap.
func TestOutputIsSortedAndDeterministic(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("b_total", "B.", "l")
	r.Gauge("a_gauge", "A.")
	c.With("z").Inc()
	c.With("m").Inc()
	_, one := scrape(t, r.Handler(), "")
	_, two := scrape(t, r.Handler(), "")
	if one != two {
		t.Fatal("output differs between scrapes")
	}
	if strings.Index(one, "a_gauge") > strings.Index(one, "b_total") || strings.Index(one, `l="m"`) > strings.Index(one, `l="z"`) {
		t.Errorf("not sorted:\n%s", one)
	}
}

// Device names are user input; an unescaped quote would break the whole scrape.
func TestLabelValuesAreEscaped(t *testing.T) {
	r := NewRegistry()
	r.Gauge("g", "G.", "device").With("a\"b\\c\nd").Set(1)
	_, body := scrape(t, r.Handler(), "")
	if !strings.Contains(body, `g{device="a\"b\\c\nd"} 1`) {
		t.Errorf("label not escaped:\n%s", body)
	}
}

// A Prometheus counter must never go down, otherwise rate() reports resets.
func TestCounterRejectsNegativeAdd(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("c_total", "C.").With()
	c.Add(5)
	c.Add(-3)
	_, body := scrape(t, r.Handler(), "")
	if !strings.Contains(body, "c_total 5\n") {
		t.Errorf("counter decreased:\n%s", body)
	}
}

// Metric registration happens at startup; a wrong label count is a programming
// error that must fail loud, not produce a mislabelled series.
func TestWrongLabelCountPanics(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("g", "G.", "a", "b")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	g.With("only-one")
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	r := NewRegistry()
	r.Gauge("g", "G.")
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	r.Counter("g", "G again.")
}

func TestResetRemovesStaleSeries(t *testing.T) {
	// Deleted devices must not keep exporting a stale backup timestamp.
	r := NewRegistry()
	g := r.Gauge("ts", "T.", "device")
	g.With("old").Set(1)
	g.Reset()
	g.With("new").Set(2)
	_, body := scrape(t, r.Handler(), "")
	if strings.Contains(body, `"old"`) || !strings.Contains(body, `"new"`) {
		t.Errorf("reset failed:\n%s", body)
	}
}

// /metrics exposes device names and fleet state, so with a token configured
// anything without the exact token is refused (constant-time compare).
func TestBearerToken(t *testing.T) {
	r := NewRegistry()
	r.Gauge("g", "G.").With().Set(1)
	open := r.Handler()
	if code, _ := scrape(t, open, ""); code != 200 {
		t.Fatalf("no token configured must be open, got %d", code)
	}
	r.SetBearerToken("s3cret")
	h := r.Handler()
	cases := []struct {
		auth string
		want int
	}{
		{"", 401}, {"Bearer wrong", 401}, {"Bearer s3cret ", 401}, {"Basic s3cret", 401},
		{"bearer s3cret", 200}, {"Bearer s3cret", 200},
	}
	for _, c := range cases {
		if code, body := scrape(t, h, c.auth); code != c.want {
			t.Errorf("auth %q: got %d want %d", c.auth, code, c.want)
		} else if code == 401 && strings.Contains(body, "tasmofleet") {
			t.Errorf("401 body leaks metrics")
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 should carry WWW-Authenticate")
	}
}

func TestContentTypeAndMethod(t *testing.T) {
	r := NewRegistry()
	h := r.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content type %q", ct)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST got %d", rec.Code)
	}
}

// Scrapes run concurrently with updates from many goroutines.
func TestConcurrentUse(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("c_total", "C.", "l")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.With("x").Inc()
				scrape(t, r.Handler(), "")
			}
		}()
	}
	wg.Wait()
	_, body := scrape(t, r.Handler(), "")
	if !strings.Contains(body, `c_total{l="x"} 1600`) {
		t.Errorf("lost updates:\n%s", body)
	}
}

// The standard set is the contract with deploy/grafana: names and types.
func TestStandardSetExposesContract(t *testing.T) {
	r := NewRegistry()
	s := NewStandard(r, "v1.2.3", true)
	s.Devices.With("up").Set(3)
	s.OTAJobsTotal.With("succeeded").Inc()
	s.HTTPRequests.With("/devices", "200").Inc()
	_, body := scrape(t, r.Handler(), "")
	for name, typ := range ContractMetrics {
		if !strings.Contains(body, "# TYPE "+name+" "+typ+"\n") {
			t.Errorf("contract metric %s (%s) not declared", name, typ)
		}
	}
	if !strings.Contains(body, `tasmofleet_build_info{decoder="true",version="v1.2.3"} 1`) {
		t.Errorf("build_info missing:\n%s", body)
	}
}
