package policy

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// asyncDevice behaves like Tasmota over HTTP: a Backlog only queues its commands (the reply comes
// back at once, CmndBacklog), a direct command runs immediately, and the queued ones run later,
// one per SetOption34 delay. Time passes in Wait (the Fixer's injected clock), which drains the
// whole queue: the Fixer only ever waits seconds, the queue needs 200 ms per line.
// A key in refuse is a setting the device does not accept: its command is queued and
// answered but never changes the value.
type asyncDevice struct {
	mu        sync.Mutex
	runtime   map[string]string // RAM
	flash     map[string]string // what survives a power cycle
	queue     []string
	refuse    map[string]bool
	waited    time.Duration
	waits     int
	reads     int
	sentOrder []string
}

func newAsyncDevice(saveData string) *asyncDevice {
	d := &asyncDevice{runtime: map[string]string{"SaveData": saveData, "PowerOnState": "0", "SetOption0": "0"}, refuse: map[string]bool{}}
	d.flash = cloneMap(d.runtime)
	return d
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (d *asyncDevice) run(line string) {
	key, val, _ := strings.Cut(strings.TrimSpace(line), " ")
	if strings.EqualFold(key, "SaveData") {
		if val != "" {
			d.runtime["SaveData"] = val
		}
		d.flash = cloneMap(d.runtime)
		return
	}
	if !d.refuse[key] {
		d.runtime[key] = val
	}
}

func (d *asyncDevice) Command(_ context.Context, _ tasmota.Target, c string) (json.RawMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sentOrder = append(d.sentOrder, c)
	if rest, ok := strings.CutPrefix(c, "Backlog "); ok {
		d.queue = append(d.queue, strings.Split(rest, ";")...)
		return json.RawMessage(`{}`), nil
	}
	d.run(c)
	return json.RawMessage(`{}`), nil
}

func (d *asyncDevice) ReadKeys(_ context.Context, _ model.DeviceID, keys []string) (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	out := map[string]string{}
	for _, k := range keys {
		out[k] = d.runtime[k]
	}
	return out, nil
}

func (d *asyncDevice) wait(_ context.Context, w time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.waits++
	d.waited += w
	for _, l := range d.queue {
		d.run(l)
	}
	d.queue = nil
	return nil
}

func (d *asyncDevice) powerCycle() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtime, d.queue = cloneMap(d.flash), nil
}

func (d *asyncDevice) fixer() *Fixer {
	return &Fixer{Client: d, Reader: d, Wait: d.wait}
}

// Why (#559): over HTTP the reply to a Backlog comes before the queue has run, so the first
// read-back sees the old values of every line but the first. A three-line Make compliant
// reported all three devices' lines as failed although the device applied everything.
func TestVerifyWaitsForTheBacklogQueueToRun(t *testing.T) {
	d := newAsyncDevice("1")
	lines := []string{"PowerOnState 3", "SetOption0 1", "SaveData 1"}
	f := d.fixer()
	if err := f.Apply(context.Background(), model.Device{ID: 1}, lines); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Verify(context.Background(), model.Device{ID: 1}, lines); err != nil {
		t.Fatalf("the device applied everything, Verify must wait for it: %v", err)
	}
	if d.reads < 2 {
		t.Fatalf("the first read cannot have matched, %d reads", d.reads)
	}
}

// Why (#559): the retry must not hide a real failure. A device that refuses a value is
// reported as failed, with what it really has (the last read), after a few seconds at most.
func TestVerifyStillFailsForAValueTheDeviceRefuses(t *testing.T) {
	d := newAsyncDevice("1")
	d.refuse["SetOption0"] = true
	lines := []string{"PowerOnState 3", "SetOption0 1", "SaveData 1"}
	f := d.fixer()
	if err := f.Apply(context.Background(), model.Device{ID: 1}, lines); err != nil {
		t.Fatal(err)
	}
	got, err := f.Verify(context.Background(), model.Device{ID: 1}, lines)
	if err == nil || !strings.Contains(err.Error(), "SetOption0 is 0, expected 1") {
		t.Fatalf("a refused value must fail: %v", err)
	}
	if strings.Contains(err.Error(), "PowerOnState") || got["PowerOnState"] != "3" {
		t.Fatalf("only the refused value is wrong, and the last read wins: %v %v", got, err)
	}
	if d.reads != 1+settleReads || d.waited != settleReads*settleWait {
		t.Fatalf("a refused value is read %d times, %v apart, no more: %d reads, waited %v", 1+settleReads, settleWait, d.reads, d.waited)
	}
	if d.waited > 10*time.Second {
		t.Fatalf("the wait for a failure is bounded to a few seconds, waited %v", d.waited)
	}
}

// Why (#559): a command that is not a Backlog line is run at once, before the queued lines. A
// save sent behind a Backlog the plain way writes flash without the later lines, which are then
// lost at the next power cycle (SaveData 0 is the fleet policy). The save has to queue behind them.
func TestApplySaveRunsAfterTheQueuedLines(t *testing.T) {
	d := newAsyncDevice("0")
	lines := []string{"PowerOnState 3", "SetOption0 1"} // no SaveData of their own: Apply adds the save
	f := d.fixer()
	if err := f.Apply(context.Background(), model.Device{ID: 1}, lines); err != nil {
		t.Fatal(err)
	}
	_ = d.wait(context.Background(), time.Second) // time passes: the queue runs (the error is always nil)
	d.powerCycle()
	if d.runtime["PowerOnState"] != "3" || d.runtime["SetOption0"] != "1" {
		t.Fatalf("lines lost at the power cycle, the save ran before them: %v (sent %q)", d.runtime, d.sentOrder)
	}
	if d.runtime["SaveData"] != "0" {
		t.Fatalf("the save must keep the SaveData value: %v", d.runtime)
	}
}
