package restore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// Why (#1005, review): a dump carries GroupTopic2 to GroupTopic4, and nothing refreshes the stored ones before the next
// read (INFO1 after the restart carries GroupTopic1 only). Every upload that ran or may have run, over either transport,
// must mark them unread, or another device's MQTT writes could reach this one through a group topic the restore set. An
// upload that certainly never reached the device must not: it changed nothing, and the mark holds back the fleet's MQTT
// writes until the device has been read again. Once the device is back the restore reads them itself, before the
// verifying backup, which they would otherwise keep off MQTT (a device only MQTT reaches could not be verified).
func TestARestoreUploadMakesGroupTopics2To4Unread(t *testing.T) {
	for _, tc := range []struct {
		name string
		mqtt bool
		set  func(e *flowEnv, u *fakeUploader)
		want bool // marked unread, then read again before the verification
	}{
		{"HTTP, applied", false, func(*flowEnv, *fakeUploader) {}, true},
		{"HTTP, reply lost", false, func(e *flowEnv, _ *fakeUploader) { e.cl.afterErr = tasmota.ErrTimeout }, true},
		{"HTTP, refused", false, func(e *flowEnv, _ *fakeUploader) {
			e.cl.swallowErr = fmt.Errorf("%w: File invalid", tasmota.ErrUploadRefused)
		}, false},
		{"HTTP, never sent", false, func(e *flowEnv, _ *fakeUploader) {
			e.cl.swallowErr = fmt.Errorf("%w: %w", tasmota.ErrNotSent, tasmota.ErrAuth)
		}, false},
		{"MQTT, applied", true, func(*flowEnv, *fakeUploader) {}, true},
		{"MQTT, Done lost", true, func(_ *flowEnv, u *fakeUploader) { u.err = mqttcmd.ErrOutcomeUnknown }, true},
		{"MQTT, not accepted", true, func(_ *flowEnv, u *fakeUploader) { u.apply, u.err = false, errors.New("test: FileUpload refused") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlow(t, "15.6.0")
			a := e.dumpBackup(t, e.dumpA)
			e.sim.SetConfigDump(e.dumpB)
			u := &fakeUploader{e: e, apply: true}
			m := e.m
			if tc.mqtt {
				m = e.mqttManager(t, u, transport.MQTT)
			}
			tc.set(e, u)
			var mu sync.Mutex
			var events []string
			m.o.ForgetGroupTopics = func(id model.DeviceID) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, fmt.Sprintf("forget %d", id))
			}
			m.o.ReadGroupTopics = func(_ context.Context, d model.Device) error {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, fmt.Sprintf("read %d", d.ID))
				return nil
			}
			job, err := m.Start(t.Context(), "alice", a.ID)
			if err != nil {
				t.Fatal(err)
			}
			e.finish(t, job.ID)
			mu.Lock()
			defer mu.Unlock()
			var want []string
			if tc.want {
				want = []string{fmt.Sprintf("forget %d", e.dev), fmt.Sprintf("read %d", e.dev)}
			}
			if !slices.Equal(events, want) {
				t.Errorf("group topics: %v, want %v", events, want)
			}
		})
	}
}

// Why (#1005): the read before the verification only spares the verifying backup a wait; a device that does not answer
// it (or answers without all four keys) must not fail a restore that the verification can still prove, here over HTTP,
// which the unread group topics do not hold back.
func TestAFailedGroupTopicReadLeavesTheRestoreToTheVerification(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	var mu sync.Mutex
	asked := 0
	e.m.o.ReadGroupTopics = func(context.Context, model.Device) error {
		mu.Lock()
		defer mu.Unlock()
		asked++
		return errors.New("test: the device's group topics could not be read")
	}
	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreDone || !done.Verified {
		t.Fatalf("job %+v: a failed group topic read must leave the outcome to the verification", done)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != 1 {
		t.Errorf("group topics read %d times before the verification, want once", asked)
	}
}
