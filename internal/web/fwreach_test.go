package web

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

type fakeForget struct{ calls []string }

func (f *fakeForget) Forget(_ context.Context, actor string, id model.DeviceID) error {
	f.calls = append(f.calls, actor+":"+string(rune('0'+id)))
	return nil
}

// Why (D11): "Forget" drops a remembered "no" so the next update tries the download again. It is an
// administrator decision: an operator must not be able to hide a real firewall problem.
func TestFWReachForgetIsAdminOnly(t *testing.T) {
	f := &fakeForget{}
	e := setupWith(t, func(d *Deps) { d.FWReach = f })
	form := url.Values{"csrf": {"tok"}}

	resp, _ := e.do(t, "POST", "/devices/1/fwreach/forget", "operator", form)
	if resp.StatusCode != http.StatusForbidden || len(f.calls) != 0 {
		t.Fatalf("operator: status %d, calls %v", resp.StatusCode, f.calls)
	}
	resp, _ = e.do(t, "POST", "/devices/1/fwreach/forget", "admin", form)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/tf/devices/1" || len(f.calls) != 1 {
		t.Fatalf("admin: status %d, location %q, calls %v", resp.StatusCode, resp.Header.Get("Location"), f.calls)
	}
	resp, _ = e.do(t, "POST", "/devices/99/fwreach/forget", "admin", form)
	if resp.StatusCode != http.StatusNotFound || len(f.calls) != 1 {
		t.Fatalf("unknown device: status %d, calls %v", resp.StatusCode, f.calls)
	}
}
