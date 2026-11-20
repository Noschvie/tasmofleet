package mqttcmd

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestMain runs one command through a fake bus before any test. Why: the first command of a process
// costs about 130 ms under -race (8 ms without; measured, 320 ms on a CI runner), once. Serial tests run
// before the parallel ones, so a serial test with a timing bound would meet that cost: TestBacklogEnds-
// WhenQuietAndTimesOutWithoutAnyReply (250 ms, a 60 ms quiet period) did on its first run. The warm-up
// takes the cost away from the tests; it changes no window and no bound.
func TestMain(m *testing.M) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) { go b.result(p.topic, `{"Hostname":"x"}`) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := newChan(b)
	_, _ = c.Do(ctx, plug(), "Hostname") // the result does not matter, only that the path ran once
	c.Close()
	cancel()
	os.Exit(m.Run())
}
