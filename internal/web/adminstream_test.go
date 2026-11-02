package web

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// demotableGuard is fakeGuard whose re-checked principal carries the role of the request's
// X-Test-Role header, until demoted is set: then the same session is only an operator (a changed
// group mapping, or another admin lowering the role).
type demotableGuard struct {
	fakeGuard
	demoted *atomic.Bool
}

func (g demotableGuard) Principal(r *http.Request) (model.Principal, error) {
	p, err := g.fakeGuard.Principal(r)
	if err != nil {
		return p, err
	}
	p.Role = model.RoleOperator
	if r.Header.Get("X-Test-Role") == "admin" && !g.demoted.Load() {
		p.Role = model.RoleAdmin
	}
	return p, nil
}

// endsWhenDemoted opens an admin stream, checks that it stays open across several re-checks while
// the session is still an admin, then demotes the session and wants the stream closed.
func endsWhenDemoted(t *testing.T, e *env, demoted *atomic.Bool, path string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", e.srv.URL+"/tf"+path, nil)
	req.Header.Set("X-Test-Role", "admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
		t.Fatal("stream ended at a re-check although the session is still an admin")
	case <-time.After(600 * time.Millisecond): // several re-checks
	}
	demoted.Store(true)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream stayed open for a demoted admin")
	}
}

// Why (GHSA-wq89-cqcx-3h3h): role changes have to take effect within one re-check interval
// everywhere, or demoting someone is no reliable way to withdraw access. The scan stream sends
// addresses, MACs and names of the network's devices; a fresh request to it from an operator is a
// 403, so an open one must end too. The firmware prefetch stream already does.
func TestAdminStreamsEndWhenTheSessionIsDemoted(t *testing.T) {
	opts := func(demoted *atomic.Bool) func(*Deps) {
		return func(d *Deps) { d.SSERecheck, d.Guard = 30*time.Millisecond, demotableGuard{demoted: demoted} }
	}
	t.Run("network scan", func(t *testing.T) {
		demoted := &atomic.Bool{}
		e, fd := setupDiscovery(t, opts(demoted))
		fd.mu.Lock()
		fd.scan.State = discovery.ScanRunning // it runs on, so only the role can end the stream
		fd.mu.Unlock()
		endsWhenDemoted(t, e, demoted, "/discovery/scan/abc123/stream")
	})
	t.Run("firmware prefetch", func(t *testing.T) {
		demoted := &atomic.Bool{}
		fw := &fakeMgrFW{releases: baseRelease(), slow: make(chan struct{})}
		defer close(fw.slow)
		e := setupWith(t, func(d *Deps) {
			opts(demoted)(d)
			d.Firmware, d.Artifacts = fw, fw
			d.Inventory = newInv(mixedFleet()...)
			d.Jobs = &fakeJobs{}
		})
		r, _ := e.do(t, "POST", "/firmware/prefetch", "admin", url.Values{"release": {"16.1.0"}})
		endsWhenDemoted(t, e, demoted, strings.TrimPrefix(r.Header.Get("Location"), "/tf")+"/stream")
	})
}
