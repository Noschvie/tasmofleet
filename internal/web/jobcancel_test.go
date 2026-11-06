package web

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (GHSA-fmj9-3vh6-rq53): the admin-only kinds (GPIO template transactions, kinds this build does
// not know) are admin-only because cancelling them half way leaves devices half configured. An
// authorization check must not depend on the database being healthy: when the job cannot be read
// the cancel is not started, for an operator or an admin, and a job that does not exist is a 404
// that never reaches the engine (it cancels a running job without reading the database).
func TestJobCancelFailsClosedWhenTheJobCannotBeRead(t *testing.T) {
	e, o, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindTemplate, State: model.JobAwaitReboot, CreatedAt: time.Now()}}

	j.getErr = errors.New("database is busy")
	for _, role := range []string{"operator", "admin"} {
		if r, _ := e.do(t, "POST", "/jobs/1/cancel", role, url.Values{}); r.StatusCode < 500 || len(o.cancelled) != 0 {
			t.Fatalf("%s: a job that could not be read was cancelled anyway: %d, cancelled %v", role, r.StatusCode, o.cancelled)
		}
	}

	j.getErr = nil
	if r, _ := e.do(t, "POST", "/jobs/99/cancel", "operator", url.Values{}); r.StatusCode != 404 || len(o.cancelled) != 0 {
		t.Fatalf("unknown job: %d, cancelled %v", r.StatusCode, o.cancelled)
	}
	// and the check itself still holds with a healthy database
	if r, _ := e.do(t, "POST", "/jobs/1/cancel", "operator", url.Values{}); r.StatusCode != 403 || len(o.cancelled) != 0 {
		t.Fatalf("operator cancelling a template job: %d, cancelled %v", r.StatusCode, o.cancelled)
	}
	if r, _ := e.do(t, "POST", "/jobs/1/cancel", "admin", url.Values{}); r.StatusCode != 303 || len(o.cancelled) != 1 {
		t.Fatalf("admin cancelling a template job: %d, cancelled %v", r.StatusCode, o.cancelled)
	}
}
