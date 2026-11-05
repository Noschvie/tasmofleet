package simulator

import (
	"testing"
	"time"
)

// Why (#559): Tasmota's Backlog only queues; the main loop runs one command per SetOption34
// delay (200 ms). A client that reads back right after the /cm reply sees the old values of the
// later lines. The simulator used to run the whole Backlog before it answered, which hid
// exactly that from every test.
func TestBacklogOverHTTPAnswersBeforeTheQueueHasRun(t *testing.T) {
	t.Parallel()
	// TimeScale 5: one line per second, so the last line is 2 s away from the reply and a slow
	// machine cannot make the read below see it applied.
	d := add(t, newTestFleet(t, "", Extra{TimeScale: 5}), DeviceSpec{Name: "slow", Profile: ProfileESP8266_4M, RebootGap: 50 * time.Millisecond})
	eq(t, "backlog reply", cm(t, d, "Backlog TelePeriod 61; SetOption0 1; PowerOnState 3"), `{}`)
	eq(t, "last line right after the reply", cm(t, d, "PowerOnState") == `{"PowerOnState":3}`, false)
	// Every command the device handles pushes the queue back (a read each 50 ms would starve it,
	// as on a real Tasmota), so wait without talking to it.
	deadline := time.Now().Add(15 * time.Second)
	time.Sleep(2500 * time.Millisecond)
	for cm(t, d, "PowerOnState") != `{"PowerOnState":3}` {
		if time.Now().After(deadline) {
			t.Fatal("the queued commands never ran")
		}
		time.Sleep(1500 * time.Millisecond)
	}
	eq(t, "first line", cm(t, d, "TelePeriod"), `{"TelePeriod":61}`)
	eq(t, "second line", cm(t, d, "SetOption0"), `{"SetOption0":"ON"}`)
}

// Why (#559): a direct command does not wait for the queue. A save sent right behind a Backlog
// writes flash before the later lines ran, so they are lost at the next power cycle; a save
// queued behind them (a Backlog of its own) runs after them. Fixer.Apply depends on this order.
func TestSaveBehindTheBacklogQueueKeepsTheLaterLines(t *testing.T) {
	t.Parallel()
	for name, save := range map[string]string{"direct save": "SaveData 0", "queued save": "Backlog SaveData 0"} {
		survives := save != "SaveData 0"
		d := add(t, newTestFleet(t, "", Extra{TimeScale: 1}), DeviceSpec{Name: "order", Profile: ProfileESP8266_4M, RebootGap: 50 * time.Millisecond})
		cm(t, d, "SaveData 0")
		cm(t, d, "Backlog TelePeriod 61; SetOption0 1; PowerOnState 3")
		cm(t, d, save)
		time.Sleep(2 * time.Second) // the queue (and the queued save) has run
		d.PowerCycle()
		got := cmUp(t, d, "PowerOnState") == `{"PowerOnState":3}`
		if got != survives {
			t.Errorf("%s: last line survived the power cycle = %v, want %v", name, got, survives)
		}
	}
}
