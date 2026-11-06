package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

// #659: the silent fleet reads leave a device alone like an offline one when its HTTP API is off and MQTT cannot
// read it (no read, no error); a device that can be read is read as before.

type idReader struct {
	mu   sync.Mutex
	read []model.DeviceID
}

func (r *idReader) ReadKeys(_ context.Context, id model.DeviceID, _ []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.read = append(r.read, id)
	return map[string]string{"SaveData": "0"}, nil
}

// Why: Policy "Run now" read every device, so a device that can only refuse waited for its timeout and failed.
func TestOnDemandPolicyCheckSkipsADeviceNothingCanRead(t *testing.T) {
	rd := &idReader{}
	devs := []model.Device{{ID: 1, Name: "api-off"}, {ID: 2, Name: "fine"}}
	rep := &policyReporter{
		rules: []policy.Rule{{Command: "SaveData", Want: "0"}}, backups: fakeBackupSvc{}, reader: rd,
		devices: fakeLister(devs), now: time.Now, skip: func(id model.DeviceID) bool { return id == 1 },
	}
	rep.refresh(t.Context(), "admin", devs)
	if len(rd.read) != 1 || rd.read[0] != 2 {
		t.Fatalf("read %v, want only device 2", rd.read)
	}
}

// Why: a skipped device is not a failed one. The reader must not count it as tried (the backoff would then delay the
// first read after the API is back by minutes).
func TestGroupTopicReaderSkipsADeviceNothingCanReadWithoutCountingAFailure(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	st := &gtStore{now: func() time.Time { return now }, devs: []model.Device{{ID: 1, Topic: "a", Address: "192.0.2.1"}}}
	c := &gtClient{}
	r := newTestReader(st, c, &now, online)
	skipped := true
	r.skip = func(model.DeviceID) bool { return skipped }
	r.tick(t.Context())
	if len(c.asked) != 0 || len(r.tried) != 0 {
		t.Fatalf("asked %v, tried %v", c.asked, r.tried)
	}
	skipped = false // the API is back
	r.tick(t.Context())
	if len(c.asked) != 1 {
		t.Fatalf("not read at once when readable again: %v", c.asked)
	}
}
