package web

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// retryEnv is two failed command transactions that each hold a destructive command (7 and 9), a
// console that can hand out its own confirmation token, and the fakes behind them.
func retryEnv(t *testing.T) (*env, *fakeOTA, *fakeJobs) {
	t.Helper()
	o := &fakeOTA{items: map[model.DeviceID]ota.PreviewItem{}, retryRes: ota.RetryResult{Batch: 10, Devices: []model.DeviceID{1}}}
	j := &fakeJobs{jobs: []model.Job{
		{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindCommand, Commands: "Reset 1", State: model.JobFailed, CreatedAt: time.Now()},
		{ID: 2, BatchID: 9, DeviceID: 1, Kind: model.KindCommand, Commands: "Reset 1", State: model.JobFailed, CreatedAt: time.Now()},
	}}
	e := setupWith(t, func(d *Deps) { d.OTA, d.Jobs, d.Batches, d.DeviceCmd = o, j, fakeBatches{}, &fakeCmd{reply: `{}`} })
	return e, o, j
}

// Why (GHSA-cpfg-6c57-xg33): the confirmation of a destructive retry is the only thing between a
// click and sending a destructive command to many devices again, so it has to confirm exactly the
// transaction the admin looked at. A token the server issued for another transaction, or for a
// console command, is no confirmation for this one.
func TestRetryConfirmationBelongsToItsTransaction(t *testing.T) {
	e, o, _ := retryEnv(t)
	_, retry7 := e.do(t, "GET", "/transactions/7/retry", "admin", nil)
	tok7 := confirmToken(t, retry7)
	_, console := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("Reset 1"))
	tokConsole := confirmToken(t, console)

	for name, tok := range map[string]string{"token of transaction 7": tok7, "token of a console command": tokConsole} {
		if r, _ := e.do(t, "POST", "/transactions/9/retry", "admin", url.Values{"token": {tok}}); r.StatusCode != 409 || len(o.retryCalls) != 0 {
			t.Fatalf("transaction 9 retried with the %s: %d, retries %v", name, r.StatusCode, o.retryCalls)
		}
	}
	// the right token still confirms its own transaction
	_, retry9 := e.do(t, "GET", "/transactions/9/retry", "admin", nil)
	if r, _ := e.do(t, "POST", "/transactions/9/retry", "admin", url.Values{"token": {confirmToken(t, retry9)}}); r.StatusCode != 303 || len(o.retryCalls) != 1 || o.retryCalls[0].batch != 9 {
		t.Fatalf("confirmed retry of 9: %d %v", r.StatusCode, o.retryCalls)
	}
}

// Why (GHSA-cpfg-6c57-xg33): the checks of a retry must not depend on the database answering. When
// the job lookup fails the request fails; it must not fall through to the engine, which checks
// the role but never the confirmation.
func TestRetryDoesNotStartWhenTheJobLookupFails(t *testing.T) {
	e, o, j := retryEnv(t)
	// needAdminForBatch asks for one job and passes; the lookup for the whole transaction fails
	j.listErr = func(f store.JobFilter) error {
		if f.Limit == 0 {
			return errors.New("database is busy")
		}
		return nil
	}
	r, _ := e.do(t, "POST", "/transactions/7/retry", "admin", url.Values{})
	if r.StatusCode < 500 || len(o.retryCalls) != 0 {
		t.Fatalf("a retry started without its confirmation while the job lookup failed: %d, retries %v", r.StatusCode, o.retryCalls)
	}
}
