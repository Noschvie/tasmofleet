package mqttcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hellwand/tasmofleet/internal/mqttbus"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// collectBacklog gathers one reply per queued command. Tasmota v16 publishes nothing for the
// Backlog itself; the queue is worked off (200 ms apart by default) and every command publishes
// its own RESULT or STATUSn. The replies are merged in queue order, later keys winning, which is
// what /cm returns over HTTP. It ends when every queued command has answered, BacklogQuiet after
// the last reply when some did not (a command without a reply, e.g. Delay), or at BacklogMax.
// Without any reply within ReplyWait it is a timeout: the outcome is unknown. A light part has no answer of its own
// (Reply): it is not waited for, and the Backlog that holds one ends with a Status 11 (confirmLights) whose STATUS11
// it needs, or it is an unknown outcome at BacklogMax however many other parts answered (#813).
func (c *Channel) collectBacklog(ctx context.Context, w *waiter, req *request) (json.RawMessage, error) {
	got := make([][]byte, len(req.backlog))
	kept := make([]bool, len(req.backlog)) // a light part that has its light state (ownLightState)
	n, want := 0, 0
	confirm := -1 // index of the Status 11 that confirms the light parts
	for i, sub := range req.backlog {
		if !sub.light {
			want++
		}
		if sub.confirms {
			confirm = i
		}
	}
	unconfirmed := func() bool { return confirm >= 0 && got[confirm] == nil }
	first := time.NewTimer(c.o.ReplyWait)
	defer first.Stop()
	quiet := time.NewTimer(time.Hour)
	quiet.Stop()
	defer quiet.Stop()
	limit := time.NewTimer(c.o.BacklogMax)
	defer limit.Stop()
	firstC := first.C
	for n < want {
		select {
		case m := <-w.ch:
			if keep(req, got, kept, m) {
				continue
			}
			i, v := assign(req, got, kept, m)
			if i < 0 {
				continue
			}
			if v.err != nil {
				return nil, fmt.Errorf("mqtt: %s in a Backlog: %w", req.backlog[i].name, v.err)
			}
			got[i] = v.reply
			n++
			firstC = nil
			quiet.Reset(c.o.BacklogQuiet)
		case <-w.off:
			return nil, fmt.Errorf("mqtt: %w (during the Backlog, outcome unknown): %w", ErrWentOffline, tasmota.ErrTimeout)
		case <-c.done:
			return nil, c.closedAfterSend("the Backlog")
		case <-firstC:
			return nil, fmt.Errorf("mqtt: no answer to the Backlog: %w", tasmota.ErrTimeout)
		case <-quiet.C:
			if unconfirmed() {
				continue // the light parts are not confirmed yet: wait for the STATUS11 until BacklogMax
			}
			return mergeObjects(got), nil
		case <-limit.C:
			if n == 0 {
				return nil, fmt.Errorf("mqtt: no answer to the Backlog: %w", tasmota.ErrTimeout)
			}
			if unconfirmed() {
				return nil, fmt.Errorf("mqtt: the light commands of the Backlog were not confirmed by its %s: %w", ConfirmRead, tasmota.ErrTimeout)
			}
			return mergeObjects(got), nil
		case <-ctx.Done():
			return nil, fmt.Errorf("mqtt: the Backlog was sent and not fully answered before the caller gave up (outcome unknown): %w: %w", tasmota.ErrTimeout, ctx.Err())
		}
	}
	return mergeObjects(got), nil
}

// keep gives m to the first queued command that has neither answered nor kept its light state, when that command is a
// light part and m its light state (ownLightState). Tasmota runs the parts in order, so the next message is that
// part's response; given to assign, it would answer the next part whose key it carries, and that part's own reply,
// a Command Error included, would be dropped (#813). A kept light state is not merged: the trailing STATUS11 has the
// state after every part.
func keep(req *request, got [][]byte, kept []bool, m mqttbus.Message) bool {
	i := 0
	for i < len(req.backlog) && (got[i] != nil || kept[i]) {
		i++
	}
	if i == len(req.backlog) || !req.backlog[i].ownLightState(m) {
		return false
	}
	kept[i] = true
	return true
}

// assign gives m to the first queued command that has not answered yet and recognises it.
func assign(req *request, got [][]byte, kept []bool, m mqttbus.Message) (int, verdict) {
	for i, sub := range req.backlog {
		if got[i] != nil || kept[i] {
			continue
		}
		if v, ok := sub.match(m, false); ok {
			return i, v
		}
	}
	return -1, verdict{}
}

// mergeObjects merges JSON objects in order. A key that repeats keeps its first position and its
// last value. Replies that are not objects cannot occur (match only accepts objects or STATUS
// objects), nil entries (no reply) are skipped.
func mergeObjects(parts [][]byte) json.RawMessage {
	var order []string
	vals := map[string]json.RawMessage{}
	for _, p := range parts {
		if p == nil {
			continue
		}
		keys, values, ok := topLevel(p)
		if !ok {
			continue
		}
		for _, k := range keys {
			if _, seen := vals[k]; !seen {
				order = append(order, k)
			}
			vals[k] = values[k]
		}
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range order {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k) // a string cannot fail
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vals[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}
