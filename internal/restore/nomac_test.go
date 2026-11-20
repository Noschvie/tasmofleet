package restore

import (
	"errors"
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#846): the person who meets "no MAC" is told one thing, whether a backup or a restore met it: probe the device
// first. The restore refuses a device it cannot identify with the sentence the backup uses for its files, and sends
// nothing to a host it cannot tell from the device.
func TestRestoreRefusesADeviceWithoutAStoredMACWithTheSentenceTheBackupUses(t *testing.T) {
	t.Parallel()
	e := newFlow(t, "15.6.0")
	err := e.m.checkIdentity(t.Context(), model.Device{Address: e.device.Address, Port: e.device.Port})
	if !errors.Is(err, ErrIdentity) {
		t.Fatalf("got %v, want ErrIdentity", err)
	}
	const want = "the inventory has no MAC for this device (probe it first)"
	if !strings.Contains(err.Error(), want) { // the backup's note is pinned to the same sentence in the backup package
		t.Errorf("restore says %q, want %q", err, want)
	}
	if n := len(e.cl.sent("Status 5")); n != 0 {
		t.Errorf("the host was asked %d times for a MAC it could not be compared with", n)
	}
}

// Why (#846): a restore that cannot store the live MAC of a device without one (no inventory to adopt it into) refuses
// with the same sentence too, and asks the host nothing.
func TestARestoreThatCannotAdoptAMACSaysProbeItFirstLikeTheBackup(t *testing.T) {
	t.Parallel()
	e := newFlow(t, "15.6.0")
	e.m.o.Hardware = nil
	dev := model.Device{Address: e.device.Address, Port: e.device.Port}
	err := e.m.adoptMAC(t.Context(), "alice", &dev, true, "")
	if !errors.Is(err, ErrIdentity) || !strings.Contains(err.Error(), "the inventory has no MAC for this device (probe it first)") {
		t.Errorf("got %v", err)
	}
	if n := len(e.cl.sent("Status 5")); n != 0 {
		t.Errorf("the host was asked %d times for a MAC that could not be stored", n)
	}
}
