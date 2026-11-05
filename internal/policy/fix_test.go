package policy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

type fakeCmd struct {
	mu   sync.Mutex
	sent []string
	err  func(cmd string) error
	// hang (may be nil) says a command never answers: it returns only when its context is done
	hang func(cmd string) bool
	// what the context of each command looked like when it was sent (parallel to sent)
	ctxErrs   []error
	deadlines []time.Time
}

func (f *fakeCmd) Command(ctx context.Context, _ tasmota.Target, c string) (json.RawMessage, error) {
	f.mu.Lock()
	f.sent = append(f.sent, c)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	dl, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, dl)
	hang := f.hang != nil && f.hang(c)
	var err error
	if f.err != nil {
		err = f.err(c)
	}
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

type fakeRead struct {
	vals    map[string]string
	errs    []error // returned one per call before values are served
	calls   [][]string
	ctxErrs []error // the state of the context of each call
}

func (r *fakeRead) ReadKeys(ctx context.Context, _ model.DeviceID, keys []string) (map[string]string, error) {
	r.calls = append(r.calls, keys)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	if len(r.errs) > 0 {
		err := r.errs[0]
		r.errs = r.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := r.vals[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func newFixer(c *fakeCmd, r *fakeRead) *Fixer {
	return &Fixer{Client: c, Reader: r, Wait: func(context.Context, time.Duration) error { return nil }}
}

// saves counts the saves Apply adds behind the chunks (a Backlog of a SaveData alone, #559); a
// SaveData line inside a chunk is not one.
func saves(sent []string) int {
	n := 0
	for _, s := range sent {
		if strings.HasPrefix(s, "Backlog SaveData") && !strings.Contains(s, ";") {
			n++
		}
	}
	return n
}

func count(sent []string, prefix string) int {
	n := 0
	for _, s := range sent {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// Why: every write to flash wears the chip. The whole batch must end in
// exactly one SaveData that re-sends the current value, never one per command.
func TestApplyBatchesIntoBacklogWithOneSave(t *testing.T) {
	c, r := &fakeCmd{}, &fakeRead{vals: map[string]string{"SaveData": "0"}}
	err := newFixer(c, r).Apply(context.Background(), model.Device{ID: 1}, []string{"PowerOnState 0", "SetOption0 0"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Backlog PowerOnState 0; SetOption0 0", "Backlog SaveData 0"}
	if !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %q, want %q", c.sent, want)
	}
}

// Why: SaveData's own handler writes everything, and a restart saves too. An
// extra SaveData would be a second flash write for nothing.
func TestApplyAddsNoSecondSave(t *testing.T) {
	for name, lines := range map[string][]string{
		"SaveData rule": {"SetOption0 0", "SaveData 0"},
		"restart":       {"SetOption0 0", "Hostname plug"},
	} {
		c, r := &fakeCmd{}, &fakeRead{}
		if err := newFixer(c, r).Apply(context.Background(), model.Device{}, lines); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(c.sent) != 1 || saves(c.sent) > 1 || len(r.calls) != 0 {
			t.Fatalf("%s: sent %q, reads %v", name, c.sent, r.calls)
		}
	}
}

// Why: if the current SaveData value is unknown we cannot save without
// changing it, and a half-applied device is worse than an untouched one.
func TestApplySendsNothingWhenSaveDataIsUnknown(t *testing.T) {
	c := &fakeCmd{}
	err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{}, []string{"PowerOnState 0"})
	if err == nil || len(c.sent) != 0 {
		t.Fatalf("err %v, sent %q", err, c.sent)
	}
}

// Why: the plan is stored and replayed (retry); a tampered or stale line must
// be refused before it reaches a device.
func TestApplyRefusesInvalidLines(t *testing.T) {
	for _, lines := range [][]string{nil, {"Restart 1"}, {"PowerOnState 9"}, {"PowerOnState"}, {"PowerOnState 0; Restart 1"}, {"SetOption0 ON"}, {"PowerOnState 0", "PowerOnState 1"}} {
		c := &fakeCmd{}
		if err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, lines); err == nil || len(c.sent) != 0 {
			t.Errorf("%q: err %v, sent %q", lines, err, c.sent)
		}
	}
}

// Why: a long plan must not exceed what a Backlog line can carry.
func TestApplySplitsLongPlans(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "SetOption"+string(rune('0'+i%10))+string(rune('0'+i/10))+" 1")
	}
	c := &fakeCmd{}
	if err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, lines); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 4 || count(c.sent, "Backlog ") != 4 || c.sent[3] != "Backlog SaveData 0" {
		t.Fatalf("sent %q", c.sent)
	}
	for _, s := range c.sent[:3] {
		if strings.Count(s, ";") >= maxChunkCommands || len(s) > maxChunkBytes+len("Backlog ") {
			t.Errorf("chunk too large: %q", s)
		}
	}
}

// Why: a failed send must stop the run; later commands on a half-applied
// device would make it worse, and the error must say what failed.
func TestApplyStopsOnTheFirstFailedSend(t *testing.T) {
	c := &fakeCmd{err: func(string) error { return tasmota.ErrUnreachable }}
	err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, []string{"SetOption0 0"})
	if !errors.Is(err, tasmota.ErrUnreachable) || len(c.sent) != 1 {
		t.Fatalf("err %v, sent %q", err, c.sent)
	}
}

// manyLines is n plain settings (SetOption00..), enough for several Backlog chunks.
func manyLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "SetOption" + string(rune('0'+i%10)) + string(rune('0'+i/10)) + " 1"
	}
	return lines
}

// failNthBacklog fails the n-th Backlog (1-based) and nothing else.
func failNthBacklog(n int, err error) func(string) error {
	seen := 0
	return func(cmd string) error {
		if !strings.HasPrefix(cmd, "Backlog ") {
			return nil
		}
		if seen++; seen == n {
			return err
		}
		return nil
	}
}

// Why (#503): a device with SaveData 0 keeps what the first chunks changed in RAM only, so a
// later chunk that fails must not end the run before the save: the changes that are live would
// vanish on the next restart and the person would not know the device is partly changed. The
// save goes out, and the error says what is on the device and what is not, so nobody reads
// "sending ..." as "nothing happened".
func TestApplyWritesEarlierChunksToFlashWhenALaterOneFails(t *testing.T) {
	cause := tasmota.ErrTimeout
	c := &fakeCmd{err: failNthBacklog(2, cause)}
	r := &fakeRead{vals: map[string]string{"SaveData": "0"}}
	lines := manyLines(20) // chunks of 8, 8 and 4
	err := newFixer(c, r).Apply(context.Background(), model.Device{ID: 1}, lines)
	if err == nil {
		t.Fatal("a failed chunk was reported as success")
	}
	if count(c.sent, "Backlog ") != 3 || c.sent[len(c.sent)-1] != "Backlog SaveData 0" || saves(c.sent) != 1 {
		t.Fatalf("sent %q: want the two Backlogs that were tried, then exactly one SaveData with the current value", c.sent)
	}
	if !errors.Is(err, cause) {
		t.Errorf("the cause is lost: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"partly sent: 8 of 20 settings were sent and are written to flash, not read back (SetOption00, SetOption10,",
		"SetOption60, SetOption70)",
		"the failed step (SetOption80, SetOption90, SetOption01, SetOption11, SetOption21, SetOption31, SetOption41, SetOption51) may or may not have reached the device",
		"; not sent: SetOption61, SetOption71, SetOption81, SetOption91: sending SetOption80 1;",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %v", want, msg)
		}
	}
	if strings.Contains(msg, "NOT written") {
		t.Errorf("the save went out, the error must not say otherwise: %v", msg)
	}
}

// Why (#503): when the save after the failed chunk fails too, the changes live only in RAM. The
// error must say so ("a restart loses them"), not just report the chunk.
func TestApplyReportsThatPartlyAppliedChangesAreNotSavedWhenTheSaveFailsToo(t *testing.T) {
	secondBacklog := failNthBacklog(2, tasmota.ErrTimeout)
	c := &fakeCmd{err: func(cmd string) error {
		if strings.HasPrefix(cmd, "Backlog SaveData") {
			return tasmota.ErrUnreachable
		}
		return secondBacklog(cmd)
	}}
	err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{ID: 1}, manyLines(20))
	if err == nil || !strings.Contains(err.Error(), "NOT written to flash") || !strings.Contains(err.Error(), "a restart of the device loses them") {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(err, tasmota.ErrTimeout) {
		t.Errorf("the failed chunk's error is lost: %v", err)
	}
}

// Why (#503, review): the person reads this error to decide what to do, so it says only what is
// known. A chunk the device answered is "sent" (nothing is read back yet); the chunk that failed
// may or may not have arrived; lines the router refused on purpose (they could cut off a device
// reached over MQTT only) and lines after the failure were not sent.
func TestApplyErrorSeparatesSentFailedAndLeftOutLines(t *testing.T) {
	c := &fakeCmd{err: func(cmd string) error {
		switch {
		case strings.Contains(cmd, "Topic") || strings.Contains(cmd, "MqttHost"):
			return transport.ErrCutOff
		case cmd == "Backlog PowerOnState 0":
			return errors.New("refused")
		}
		return nil
	}}
	r := &fakeRead{vals: map[string]string{"SaveData": "0"}}
	lines := append(manyLines(8), "Topic other", "PowerOnState 0", "MqttHost 10.0.0.9")
	err := newFixer(c, r).Apply(context.Background(), model.Device{ID: 1}, lines)
	if err == nil {
		t.Fatal("a failed step was reported as success")
	}
	msg := err.Error()
	for _, want := range []string{
		"partly sent: 8 of 11 settings were sent and are written to flash, not read back (SetOption00,",
		"; the failed step (PowerOnState) may or may not have reached the device",
		"; not sent, left out on purpose (reached over MQTT only): Topic, MqttHost: sending PowerOnState 0: refused",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %v", want, msg)
		}
	}
	if strings.Contains(msg, "; not sent: ") {
		t.Errorf("nothing came after the failed step, yet: %v", msg)
	}
	// Topic and MqttHost would have restarted (and so saved) the device; they were left out, so the rest is saved by hand
	if c.sent[len(c.sent)-1] != "Backlog SaveData 0" {
		t.Errorf("sent %q: the lines that went out must be saved", c.sent)
	}
}

// Why (#503, review): the run that failed may have used up its whole deadline (policyjob gives
// the apply one context for every chunk), and a save that shares it fails with the same
// "deadline exceeded" at exactly the moment it is needed. The save, and the read of the SaveData
// value it needs, get a context of their own: not done, with a deadline of its own.
func TestApplySavesAfterAFailureWithAContextOfItsOwn(t *testing.T) {
	for name, lines := range map[string][]string{
		"SaveData value read up front":     manyLines(20),
		"SaveData value read for the save": append(manyLines(10), "Hostname plug"), // a restart in the batch: no read up front
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second)) // the run's deadline is gone
			defer cancel()
			c := &fakeCmd{err: failNthBacklog(2, context.DeadlineExceeded)}
			r := &fakeRead{vals: map[string]string{"SaveData": "0"}}
			fx := newFixer(c, r)
			fx.Stop = t.Context() // TasmoFleet is running
			err := fx.Apply(ctx, model.Device{ID: 1}, lines)
			if err == nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v", err)
			}
			save := len(c.sent) - 1
			if c.sent[save] != "Backlog SaveData 0" {
				t.Fatalf("sent %q: the lines that went out were not saved", c.sent)
			}
			if c.ctxErrs[save] != nil {
				t.Errorf("the save used the context of the failed run: %v", c.ctxErrs[save])
			}
			if left := time.Until(c.deadlines[save]); left <= 0 || left > saveTimeout {
				t.Errorf("the save has %v left, want a deadline within %v", left, saveTimeout)
			}
			if n := len(r.ctxErrs); name == "SaveData value read for the save" && (n != 1 || r.ctxErrs[0] != nil) {
				t.Errorf("the SaveData value was read with the context of the failed run: %v", r.ctxErrs)
			}
			if !strings.Contains(err.Error(), "written to flash") || strings.Contains(err.Error(), "NOT") {
				t.Errorf("the save went out: %v", err)
			}
		})
	}
}

// Why (#503, review): the engine cancels a running job when TasmoFleet stops, and a container gets
// only a few seconds (Docker and Unraid kill it after 10) before it is killed. A save that waits
// for a device for up to saveTimeout would get the process killed in the middle of shutdown, so a
// run that was cancelled does not save: nothing more is sent or read, the call returns at once,
// and the error says that the lines are not in flash because TasmoFleet was stopping. (The job
// is resumed at the next start and sends and saves the whole plan again.)
func TestApplyDoesNotSaveWhenTheRunWasCancelledBecauseTasmoFleetIsStopping(t *testing.T) {
	for name, lines := range map[string][]string{
		"SaveData value read up front": manyLines(20),
		"no value read up front":       append(manyLines(10), "Hostname plug"),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			second := failNthBacklog(2, context.Canceled)
			c := &fakeCmd{err: func(cmd string) error {
				if err := second(cmd); err != nil {
					stop() // shutdown: the engine cancels its jobs
					return err
				}
				return nil
			}}
			r := &fakeRead{vals: map[string]string{"SaveData": "0"}}
			readsBefore := 0
			if name == "SaveData value read up front" {
				readsBefore = 1
			}
			start := time.Now()
			err := newFixer(c, r).Apply(ctx, model.Device{ID: 1}, lines)
			if time.Since(start) > time.Second {
				t.Errorf("Apply took %v after the stop", time.Since(start))
			}
			if err == nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v", err)
			}
			if count(c.sent, "Backlog ") != 2 || saves(c.sent) != 0 || len(r.calls) != readsBefore {
				t.Fatalf("sent %q, %d reads: after the stop nothing more may be sent or read", c.sent, len(r.calls))
			}
			for _, want := range []string{"partly sent: 8 of ", "NOT written to flash because TasmoFleet was stopping", "a restart of the device loses them"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lacks %q: %v", want, err)
				}
			}
		})
	}
	// The stop is Fixer.Stop, not only the run's context: a run that failed on its own deadline
	// while TasmoFleet is stopping does not save either.
	expired, expiredCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expiredCancel()
	stopped, stopNow := context.WithCancel(context.Background())
	stopNow()
	c1 := &fakeCmd{err: failNthBacklog(2, context.DeadlineExceeded)}
	fx := newFixer(c1, &fakeRead{vals: map[string]string{"SaveData": "0"}})
	fx.Stop = stopped
	err1 := fx.Apply(expired, model.Device{ID: 1}, manyLines(20))
	if err1 == nil || saves(c1.sent) != 0 || !strings.Contains(err1.Error(), "NOT written to flash because TasmoFleet was stopping") {
		t.Fatalf("err %v, sent %q", err1, c1.sent)
	}
	// A run that failed for another reason is still saved: only the stop is special.
	c := &fakeCmd{err: failNthBacklog(2, tasmota.ErrTimeout)}
	err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{ID: 1}, manyLines(20))
	if err == nil || c.sent[len(c.sent)-1] != "Backlog SaveData 0" || strings.Contains(err.Error(), "stopping") {
		t.Fatalf("err %v, sent %q", err, c.sent)
	}
}

// Why (#503, review): a stop that arrives while the save is already running must end it at once,
// not after saveTimeout, or the container is killed in the middle of shutdown. The run itself
// failed on its deadline (so the save is attempted), the device never answers the SaveData, and
// TasmoFleet stops: Apply returns promptly and says why the lines are not in flash.
func TestApplyEndsTheSaveAtOnceWhenTasmoFleetStopsWhileItRuns(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second)) // the run's deadline is gone
	defer cancel()
	stop, stopNow := context.WithCancel(context.Background())
	defer stopNow()
	saving := make(chan struct{})
	c := &fakeCmd{
		err: failNthBacklog(2, context.DeadlineExceeded),
		hang: func(cmd string) bool {
			if strings.HasPrefix(cmd, "Backlog SaveData") {
				close(saving)
				return true
			}
			return false
		},
	}
	fx := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}})
	fx.Stop = stop
	done := make(chan error, 1)
	go func() { done <- fx.Apply(ctx, model.Device{ID: 1}, manyLines(20)) }()
	select {
	case <-saving:
	case <-time.After(10 * time.Second):
		t.Fatal("the save never started")
	}
	stoppedAt := time.Now()
	stopNow() // TasmoFleet stops while the device does not answer the SaveData
	select {
	case err := <-done:
		if took := time.Since(stoppedAt); took > 2*time.Second {
			t.Errorf("Apply took %v to return after the stop; the save has %v", took, saveTimeout)
		}
		if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "NOT written to flash because TasmoFleet was stopping") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Apply still waits for the save after the stop")
	}
}

// Why (#503): nothing was applied when the first chunk fails, so there is nothing to save and
// the plain error stays (the device is untouched; a SaveData would only wear the flash).
func TestApplySavesNothingWhenTheFirstChunkFails(t *testing.T) {
	c := &fakeCmd{err: failNthBacklog(1, tasmota.ErrTimeout)}
	err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{ID: 1}, manyLines(20))
	if !errors.Is(err, tasmota.ErrTimeout) || strings.Contains(err.Error(), "partly sent") || saves(c.sent) != 0 || len(c.sent) != 1 {
		t.Fatalf("err %v, sent %q", err, c.sent)
	}
}

// Why (#503): when the batch holds a SaveData or a restarting line no SaveData value was read up
// front, but the plain chunks that went out before the failure still sit in RAM; the value is read
// then and the save goes out. When the read fails the error says the changes are not saved. A chunk
// that itself saved (SaveData line) needs no second save.
func TestApplySavesPartlyAppliedChunksOfABatchWithItsOwnSave(t *testing.T) {
	restarting := append(manyLines(10), "Hostname plug") // chunk 1: 8 lines, chunk 2: 2 lines and the restart
	saving := append(manyLines(10), "SaveData 0")        // chunk 1: 8 lines, chunk 2: 2 lines and the SaveData
	t.Run("restarting batch", func(t *testing.T) {
		c := &fakeCmd{err: failNthBacklog(2, errors.New("refused"))}
		r := &fakeRead{vals: map[string]string{"SaveData": "0"}}
		err := newFixer(c, r).Apply(context.Background(), model.Device{ID: 1}, restarting)
		if err == nil || !strings.Contains(err.Error(), "partly sent: 8 of 11") || c.sent[len(c.sent)-1] != "Backlog SaveData 0" || len(r.calls) != 1 {
			t.Fatalf("err %v, sent %q, reads %v", err, c.sent, r.calls)
		}
	})
	t.Run("SaveData value unreadable", func(t *testing.T) {
		c := &fakeCmd{err: failNthBacklog(2, errors.New("refused"))}
		err := newFixer(c, &fakeRead{errs: []error{tasmota.ErrTimeout}}).Apply(context.Background(), model.Device{ID: 1}, saving)
		if err == nil || !strings.Contains(err.Error(), "NOT written to flash") || saves(c.sent) != 0 {
			t.Fatalf("err %v, sent %q", err, c.sent)
		}
	})
	t.Run("SaveData value not reported", func(t *testing.T) {
		c := &fakeCmd{err: failNthBacklog(2, errors.New("refused"))}
		err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{ID: 1}, saving)
		if err == nil || !strings.Contains(err.Error(), "NOT written to flash") || saves(c.sent) != 0 {
			t.Fatalf("err %v, sent %q: no SaveData without a value to re-send", err, c.sent)
		}
	})
	t.Run("a chunk that saved itself", func(t *testing.T) {
		// chunk 1 carries the SaveData line, chunk 2 (the restart) fails: what is on the device is saved
		lines := append([]string{"SaveData 0"}, manyLines(9)...) // chunk 1: SaveData + 7, chunk 2: 2 + Hostname
		lines = append(lines, "Hostname plug")
		c := &fakeCmd{err: failNthBacklog(2, errors.New("refused"))}
		r := &fakeRead{}
		err := newFixer(c, r).Apply(context.Background(), model.Device{ID: 1}, lines)
		if err == nil || !strings.Contains(err.Error(), "written to flash") || saves(c.sent) != 0 || len(r.calls) != 0 {
			t.Fatalf("err %v, sent %q, reads %v", err, c.sent, r.calls)
		}
	})
}

// Why: Hostname and Topic restart the device, so the reply may never arrive.
// That is not a failure; the read-back decides.
func TestApplyToleratesTheRestartCuttingTheReply(t *testing.T) {
	c := &fakeCmd{err: func(string) error { return tasmota.ErrTimeout }}
	if err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{}, []string{"Hostname plug"}); err != nil {
		t.Fatal(err)
	}
}

// Why: a SaveData that fails leaves the changes in RAM only; that must be
// reported, not swallowed.
func TestApplyReportsAFailedSave(t *testing.T) {
	c := &fakeCmd{err: func(cmd string) error {
		if strings.HasPrefix(cmd, "Backlog SaveData") {
			return tasmota.ErrTimeout
		}
		return nil
	}}
	err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, []string{"SetOption0 0"})
	if err == nil || !strings.Contains(err.Error(), "flash") {
		t.Fatalf("err %v", err)
	}
}

// Why: success means the device now reports the wanted values, not that the
// commands were accepted.
func TestVerifyComparesWithTheDevice(t *testing.T) {
	lines := []string{"PowerOnState 0", "SetOption0 0"}
	got, err := newFixer(nil, &fakeRead{vals: map[string]string{"PowerOnState": "0", "SetOption0": "0"}}).Verify(context.Background(), model.Device{}, lines)
	if err != nil || got["PowerOnState"] != "0" {
		t.Fatalf("%v %v", got, err)
	}
	got, err = newFixer(nil, &fakeRead{vals: map[string]string{"PowerOnState": "0", "SetOption0": "1"}}).Verify(context.Background(), model.Device{}, lines)
	if err == nil || !strings.Contains(err.Error(), "SetOption0 is 1, expected 0") || got["SetOption0"] != "1" {
		t.Fatalf("mismatch must fail and return what was read: %v %v", got, err)
	}
	if _, err = newFixer(nil, &fakeRead{vals: map[string]string{"PowerOnState": "0"}}).Verify(context.Background(), model.Device{}, lines); err == nil || !strings.Contains(err.Error(), "not reported") {
		t.Fatalf("a missing value is not compliance: %v", err)
	}
}

// Why: a device that restarts needs a moment; giving up on the first miss
// would fail a fix that worked.
func TestVerifyRetriesWhileTheDeviceRestarts(t *testing.T) {
	r := &fakeRead{vals: map[string]string{"Hostname": "plug"}, errs: []error{tasmota.ErrUnreachable, tasmota.ErrUnreachable, nil}}
	waits := 0
	f := newFixer(nil, r)
	f.Wait = func(context.Context, time.Duration) error { waits++; return nil }
	if _, err := f.Verify(context.Background(), model.Device{}, []string{"Hostname plug"}); err != nil || waits != 2 {
		t.Fatalf("err %v after %d waits", err, waits)
	}
	r = &fakeRead{errs: []error{tasmota.ErrUnreachable, tasmota.ErrUnreachable, tasmota.ErrUnreachable}}
	f = newFixer(nil, r)
	f.VerifyAttempts = 3
	if _, err := f.Verify(context.Background(), model.Device{}, []string{"Hostname plug"}); !errors.Is(err, tasmota.ErrUnreachable) || len(r.calls) != 3 {
		t.Fatalf("err %v after %d reads", err, len(r.calls))
	}
}

// Why: SetOption68 (and the other restarting SetOptions) set restart_flag = 2 like Hostname, so
// the reply may never arrive and the restart saves everything. Without that knowledge a
// following SaveData would hit the reboot and come back as an error.
func TestApplyTreatsARestartingSetOptionLikeHostname(t *testing.T) {
	c := &fakeCmd{err: func(string) error { return tasmota.ErrTimeout }}
	if err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{}, []string{"SetOption68 1"}); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 1 || c.sent[0] != "Backlog SetOption68 1" {
		t.Fatalf("sent %q, want only the Backlog (the restart saves, no SaveData after it)", c.sent)
	}
}

// Why: SetOption55 restarts the device only when switched off. Switched on it must keep the
// normal path: SaveData is sent, and a timeout is a real failure, not a restart.
func TestApplyDoesNotTreatSetOption55OnAsARestart(t *testing.T) {
	c := &fakeCmd{err: func(string) error { return tasmota.ErrTimeout }}
	err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, []string{"SetOption55 1"})
	if !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("err %v, want the timeout reported", err)
	}
	c = &fakeCmd{}
	if err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, []string{"SetOption55 1"}); err != nil || saves(c.sent) != 1 {
		t.Fatalf("err %v, sent %q, want one SaveData", err, c.sent)
	}
	c = &fakeCmd{err: func(string) error { return tasmota.ErrTimeout }}
	if err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{}, []string{"SetOption55 0"}); err != nil {
		t.Fatalf("SetOption55 0 restarts, its timeout is tolerated: %v", err)
	}
}

// Why: a restart fires once the Backlog queue is empty (support_tasmota.ino:1580,
// CommandsReady), so restarting commands only work if nothing is sent to the device after
// them. They must all sit in the last run(s), never mixed into an earlier run with plain
// commands, and two restarting commands in one Backlog both run before the reboot.
func TestApplyKeepsRestartingCommandsTogetherInTheLastBacklog(t *testing.T) {
	c := &fakeCmd{}
	// seven plain commands fill a run to one short of the limit; plain chunking would put the
	// first restarting command into it and the other two into the next run
	lines := []string{"SetOption1 0", "SetOption2 0", "SetOption4 0", "SetOption5 0", "SetOption6 0", "SetOption7 0", "SaveData 0", "SetOption37 1", "SetOption85 1", "Hostname plug"}
	if err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{}, lines); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Backlog SetOption1 0; SetOption2 0; SetOption4 0; SetOption5 0; SetOption6 0; SetOption7 0; SaveData 0",
		"Backlog SetOption37 1; SetOption85 1; Hostname plug",
	}
	if !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %q, want %q", c.sent, want)
	}
}

// Why (D13): Make compliant never carries a person's consent, so on a device reached over MQTT only the
// router refuses the lines that can cut it off (a new Topic or MqttHost). The rest is applied and saved
// first, the refused lines go last in a chunk of their own, and the result names what was left out so the
// job can verify only what was sent.
func TestApplyLeavesOutWhatCouldCutAnMQTTOnlyDeviceOff(t *testing.T) {
	c := &fakeCmd{err: func(cmd string) error {
		if strings.Contains(cmd, "Topic") || strings.Contains(cmd, "MqttHost") {
			return transport.ErrCutOff
		}
		return nil
	}}
	r := &fakeRead{vals: map[string]string{"SaveData": "0"}}
	err := newFixer(c, r).Apply(context.Background(), model.Device{ID: 1}, []string{"Topic other", "PowerOnState 0", "MqttHost 10.0.0.9"})
	var sk *SkippedError
	if !errors.As(err, &sk) || sk.Applied != 1 || !reflect.DeepEqual(sk.SkippedLines(), []string{"Topic other", "MqttHost 10.0.0.9"}) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "applied 1; skipped Topic, MqttHost: reached over MQTT only (use the console)") {
		t.Errorf("text: %v", err)
	}
	// Topic and MqttHost would have restarted (and so saved) the device; they were left out, so it is saved by hand.
	want := []string{"Backlog Topic other; PowerOnState 0; MqttHost 10.0.0.9", "Backlog PowerOnState 0", "Backlog SaveData 0"}
	if !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %q, want %q: the chunk of always, then what the refusal left of it, then the save", c.sent, want)
	}
	// nothing but cut-off lines: nothing is saved and the result says nothing was applied
	c2 := &fakeCmd{err: func(string) error { return transport.ErrCutOff }}
	err = newFixer(c2, r).Apply(context.Background(), model.Device{ID: 1}, []string{"Topic other"})
	if !errors.As(err, &sk) || sk.AppliedAny() || saves(c2.sent) != 0 {
		t.Fatalf("err = %v, sent %q", err, c2.sent)
	}
	// over HTTP the same lines go out exactly as they always did: the chunks of chunks(), nothing reordered
	c3 := &fakeCmd{}
	lines := []string{"Topic other", "PowerOnState 0", "MqttHost 10.0.0.9"}
	if err := newFixer(c3, r).Apply(context.Background(), model.Device{ID: 1}, lines); err != nil {
		t.Fatal(err)
	}
	parsed, _ := parseLines(lines)
	var wantHTTP []string
	for _, ch := range chunks(parsed) {
		texts := make([]string, len(ch))
		for i, l := range ch {
			texts[i] = l.text
		}
		wantHTTP = append(wantHTTP, "Backlog "+strings.Join(texts, "; "))
	}
	if !reflect.DeepEqual(c3.sent, wantHTTP) || len(wantHTTP) != 1 {
		t.Fatalf("an HTTP device got %q, want %q", c3.sent, wantHTTP)
	}
}
