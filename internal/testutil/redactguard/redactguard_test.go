package redactguard

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/hellwand/tasmofleet/internal/tasmota"
)

type echo struct{ tasmota.Client }

func (echo) Command(_ context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	if cmnd == "Status 6" {
		return json.RawMessage(`{"StatusMQT":{"MqttUser":"alice"}}`), nil
	}
	return json.RawMessage(`{"MqttUser":"alice"}`), nil
}

// recorder is a testing.TB that keeps the errors instead of failing the test.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// Why: the guard stands in for the router in the tests of TasmoFleet's own reads. It must mask what the router masks
// and flag a bare secret command, or a test of such a read passes while production compares "***"; and it must let a
// Status block through unmasked, which is how such a read gets the value.
func TestTheGuardMasksLikeTheRouterAndFlagsABareSecretRead(t *testing.T) {
	r := &recorder{TB: t}
	g := Wrap(r, echo{})
	raw, err := g.Command(t.Context(), tasmota.Target{}, "MqttUser")
	if err != nil || string(raw) != `{"MqttUser":"***"}` || len(r.errs) != 1 {
		t.Fatalf("bare MqttUser: %s %v, errors %q", raw, err, r.errs)
	}
	raw, err = g.Command(t.Context(), tasmota.Target{}, "Status 6")
	if err != nil || string(raw) != `{"StatusMQT":{"MqttUser":"alice"}}` || len(r.errs) != 1 {
		t.Fatalf("Status 6: %s %v, errors %q", raw, err, r.errs)
	}
	if _, err := g.Command(t.Context(), tasmota.Target{}, "MqttUser bob"); err != nil || len(r.errs) != 1 {
		t.Fatalf("a write is not a read: %v, errors %q", err, r.errs)
	}
}
