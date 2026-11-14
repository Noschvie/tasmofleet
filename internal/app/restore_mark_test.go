package app

import (
	"testing"

	"github.com/hellwand/tasmofleet/internal/transport"
)

// Why (#607): a restore over MQTT that changed the device's MQTT connection while HTTP answered must reach the router's
// memory, so the device's next commands go over HTTP while it may not be back on the broker (as after such a write,
// #605 T10). Without this wiring the restore's mark would go nowhere and the next commands into the void.
func TestTheRestoreMarksAChangedMQTTConnectionInTheRouter(t *testing.T) {
	rt := transport.NewRouter(transport.Options{})
	a := &runtime{svc: &Services{Client: writerClient{}, Router: rt}}
	mark := a.restoreOptions().MarkMQTTConnChanged
	if mark == nil {
		t.Fatal("the restore has no way to mark the device")
	}
	mark(7)
	if _, _, marked := rt.MQTTConnChanged().HTTPFailure(7); !marked {
		t.Error("the router does not know that the device's MQTT connection changed")
	}
}
