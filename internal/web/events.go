package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// sessionChecker is implemented by *auth.Service: it re-resolves the
// principal from the stored session, so logout, revocation, a password
// change or a removed group are seen.
type sessionChecker interface {
	Principal(r *http.Request) (model.Principal, error)
}

// acquireStream reserves a stream slot for the user.
func (h *handler) acquireStream(user string) bool {
	h.sseMu.Lock()
	defer h.sseMu.Unlock()
	if h.sseTotal >= h.d.MaxStreams || h.sseUsers[user] >= h.d.MaxStreamsPerUser {
		return false
	}
	h.sseTotal++
	h.sseUsers[user]++
	return true
}

func (h *handler) releaseStream(user string) {
	h.sseMu.Lock()
	defer h.sseMu.Unlock()
	h.sseTotal--
	if h.sseUsers[user]--; h.sseUsers[user] <= 0 {
		delete(h.sseUsers, user)
	}
}

// sseRetry is the reconnect delay the event stream asks browsers for.
const sseRetry = 4 * time.Second

// events streams hub events as SSE. Health changes carry ready-made HTML
// fragments (htmx-ext-sse swaps them into the row cells named
// "health-<id>" / "uptime-<id>" / "wifi-<id>"), so the device table updates in place and
// keeps the user's row selection. Inventory changes and dropped events ask
// the table to reload.
func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	user := actor(r)
	base := httpx.BasePath(r.Context())
	if !h.acquireStream(user) {
		http.Error(w, "too many open event streams", http.StatusTooManyRequests)
		return
	}
	defer h.releaseStream(user)
	checker, _ := h.d.Guard.(sessionChecker)
	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// The browser reconnects a dropped stream after this many milliseconds; live.js shows the
	// same delay in the "Reconnecting" banner (sseRetryMs there).
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", sseRetry.Milliseconds()); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}
	ch, cancel := h.d.Hub.Subscribe(events.TopicHealth, events.TopicDevice, events.TopicJob, events.TopicBackup)
	defer cancel()
	// states is what this stream last said about each device's state. Health messages arrive with
	// every telemetry; "state" goes out only when the state really changed (the dashboard reloads on it).
	states := map[model.DeviceID]model.HealthState{}
	if h.d.Health != nil {
		for id, hl := range h.d.Health.All() {
			states[id] = hl.State
		}
	}
	tick := time.NewTicker(h.d.SSERecheck)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if checker != nil {
				// A detached context: the principal cached in the request's
				// context is the one from connect time.
				if _, err := checker.Principal(r.Clone(httpx.Detach(r.Context()))); err != nil {
					return
				}
			}
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			var buf bytes.Buffer
			h.encodeEvent(&buf, ev, base)
			if hl, ok := ev.Payload.(model.Health); ok && !ev.Resync {
				if prev, seen := states[hl.DeviceID]; !seen || prev != hl.State {
					states[hl.DeviceID] = hl.State
					writeSSE(&buf, "state", fmt.Sprint(hl.DeviceID))
				}
			}
			if buf.Len() == 0 {
				continue
			}
			if _, err := buf.WriteTo(w); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

func (h *handler) encodeEvent(buf *bytes.Buffer, ev events.Event, base string) {
	if ev.Resync {
		writeSSE(buf, "resync", "")
		return
	}
	switch p := ev.Payload.(type) {
	case model.Health:
		if p.State == model.HealthOffline && p.LastSeen.IsZero() && h.d.Inventory != nil {
			// the list shows a device that never answered as Unknown (shownHealth)
			if d, err := h.d.Inventory.Get(context.Background(), p.DeviceID); err == nil {
				p = shownHealth(d, p)
			}
		}
		ctx := context.Background()
		id := devID(p.DeviceID)
		cells := []struct {
			name string
			c    templ.Component
		}{
			{"health", h.statusCell(p)}, {"reach", h.reachCell(p)},
			{"uptime", h.uptimeCell(vitalsOf(p))}, {"wifi", wifiCell(vitalsOf(p))}, {"restart", restartCell(vitalsOf(p))},
			{"power", powerCellView(h.powerCellOf(base, p.DeviceID, p))},
		}
		var frag bytes.Buffer
		for _, c := range cells {
			frag.Reset()
			if err := c.c.Render(ctx, &frag); err == nil {
				writeSSE(buf, c.name+"-"+id, frag.String())
			}
		}
	case model.Device:
		writeSSE(buf, "device", "")
	case model.Job:
		writeSSE(buf, "job", fmt.Sprint(p.ID))
		writeSSE(buf, fmt.Sprintf("job-%d", p.ID), "") // the batch page refreshes just this job
	case model.Backup:
		writeSSE(buf, "backup", "")
	}
}

// writeSSE writes one event. Newlines in data become separate data: lines.
func writeSSE(buf *bytes.Buffer, name, data string) {
	buf.WriteString("event: " + name + "\n")
	for line := range strings.SplitSeq(data, "\n") {
		buf.WriteString("data: " + line + "\n")
	}
	buf.WriteString("\n")
}
