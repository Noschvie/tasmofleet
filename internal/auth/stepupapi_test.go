package auth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Why: other features (web import) demand the same step-up as account
// management. The exported API must share its state with /users/stepup,
// check the same password, expire after 5 minutes and stay per session.
func TestStepUpAPIAgreesWithAccountStepUp(t *testing.T) {
	e := localEnv(t)
	adm := e.login("admin", testPassword)
	if r := adm.post("/stepped", url.Values{}); !strings.Contains(r.body, "stepped=false") {
		t.Fatalf("fresh session must not be stepped up: %s", r.body)
	}
	if r := adm.post("/stepped", url.Values{"current": {"wrong"}}); !strings.Contains(r.body, "ok=false") || strings.Contains(r.body, "stepped=true") {
		t.Fatalf("wrong password: %s", r.body)
	}
	if r := adm.post("/stepped", url.Values{"current": {testPassword}}); !strings.Contains(r.body, "stepped=true") {
		t.Fatalf("right password: %s", r.body)
	}
	if r := adm.get("/users"); r.code != http.StatusOK || !strings.Contains(r.body, "ops") {
		t.Fatalf("the step-up must count for account management too: %d", r.code)
	}
	if other := e.login("admin", testPassword); strings.Contains(other.post("/stepped", url.Values{}).body, "stepped=true") {
		t.Fatal("step-up leaked to another session")
	}
	e.clock.Advance(6 * time.Minute)
	if r := adm.post("/stepped", url.Values{}); !strings.Contains(r.body, "stepped=false") {
		t.Fatalf("step-up must expire: %s", r.body)
	}
}
