package simulator

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Why: the Stability e2e tests need a device that restarts for a named reason and one whose link got
// worse; both must show in exactly the places Tasmota reports them (Status 1, Status 11, STATE).
func TestRestartReportsTheGivenReasonAndABumpedBootCount(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "loop", Profile: ProfileESP8266_4M, RebootGap: 50 * time.Millisecond})
	c, _ := Control(d)
	before := c.BootCount()
	c.Restart("Exception")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.WaitFor(ctx, func(x Controller) bool { return x.Boots() == 1 && x.Phase() == PhaseRun }); err != nil {
		t.Fatal(err)
	}
	got := cm(t, d, "Status 1")
	if !strings.Contains(got, `"RestartReason":"Exception"`) {
		t.Errorf("Status 1: %s", got)
	}
	if c.BootCount() != before+1 {
		t.Errorf("BootCount %d, want %d", c.BootCount(), before+1)
	}
	// the reason is for that one boot only
	c.Restart("")
	if err := c.WaitFor(ctx, func(x Controller) bool { return x.Boots() == 2 && x.Phase() == PhaseRun }); err != nil {
		t.Fatal(err)
	}
	if got := cm(t, d, "Status 1"); strings.Contains(got, "Exception") {
		t.Errorf("the next boot still reports Exception: %s", got)
	}
}

func TestSetWifiChangesSignalRSSIAndLinkCount(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "lamp", Profile: ProfileESP8266_4M})
	c, _ := Control(d)
	c.SetWifi(-84, 14)
	got := cm(t, d, "Status 11")
	for _, want := range []string{`"Signal":-84`, `"RSSI":32`, `"LinkCount":14`} {
		if !strings.Contains(got, want) {
			t.Errorf("Status 11 lacks %s: %s", want, got)
		}
	}
	c.SetWifi(0, 0)
	if !strings.Contains(cm(t, d, "Status 11"), `"Signal":-84`) {
		t.Error("0 must keep the current value")
	}
}
