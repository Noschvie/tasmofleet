package web

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/events"
	"github.com/hellwand/tasmofleet/internal/model"
)

type parsedSSE struct{ name, data string }

// parseEventSource reads a stream the way a browser's EventSource does: a line ends at CRLF, LF or a
// lone CR (WHATWG HTML, "Server-sent events"), an empty line dispatches the event.
func parseEventSource(stream string) []parsedSSE {
	stream = strings.ReplaceAll(stream, "\r\n", "\n")
	stream = strings.ReplaceAll(stream, "\r", "\n")
	var out []parsedSSE
	name, data, hasData := "message", "", false
	for line := range strings.SplitSeq(stream, "\n") {
		switch {
		case line == "":
			if hasData {
				out = append(out, parsedSSE{name, strings.TrimSuffix(data, "\n")})
			}
			name, data, hasData = "message", "", false
		case strings.HasPrefix(line, ":"):
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				name = value
			case "data":
				data += value + "\n"
				hasData = true
			}
		}
	}
	return out
}

// Why (GHSA-fvgh-83hf-5jr9): data and event names reach the stream from places a compromised
// device or broker client controls (a relay state from MQTT). EventSource ends a line at a lone
// CR too, so a writer that only knows "\n" lets that text start extra events (replace another
// device's power cell, make every page reload). Whatever the input, one writeSSE call must be
// exactly one event for the browser.
func TestWriteSSEIsOneEventWhateverTheInput(t *testing.T) {
	forged := "ON\r\revent: power-2\rdata: Relay 1 is OFF\r"
	for _, tc := range []struct {
		label, name, data, wantData string
	}{
		{"lone CR", "power-1", forged, "ON\n\nevent: power-2\ndata: Relay 1 is OFF\n"},
		{"CRLF", "power-1", "a\r\nevent: resync\r\n\r\n", "a\nevent: resync\n\n"},
		{"LF", "power-1", "a\nevent: resync\n\n", "a\nevent: resync\n\n"},
		{"plain", "power-1", "<span>ON</span>", "<span>ON</span>"},
		{"CR or LF in the name", "power-1\r\nevent: resync\rdata: x", "d", "d"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			var buf bytes.Buffer
			writeSSE(&buf, tc.name, tc.data)
			got := parseEventSource(buf.String())
			if len(got) != 1 {
				t.Fatalf("EventSource sees %d events, want 1: %q", len(got), got)
			}
			if !strings.HasPrefix(got[0].name, "power-1") {
				t.Errorf("event name %q, want it to stay power-1...", got[0].name)
			}
			if got[0].data != tc.wantData {
				t.Errorf("data %q, want %q", got[0].data, tc.wantData)
			}
		})
	}
}

// Why (GHSA-fvgh-83hf-5jr9): end to end. A relay state carrying CR that reaches the stream through
// a health update (the toggle button puts it into its aria-label) must not make the browser see
// an event for another device or a page-wide reload.
func TestStreamRelayStateWithCRForgesNoEvent(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	hl := model.Health{DeviceID: 1, State: model.HealthOnline, LastSeen: time.Now(),
		Power: map[string]string{"POWER1": "ON\r\revent: power-2\rdata: Relay 1 is OFF\r\revent: resync\rdata: x\r"}}
	got := readStream(t, e, "999999999", &events.Event{Topic: events.TopicHealth, Payload: hl}, "devstate-1")
	evs := parseEventSource(got)
	if !strings.Contains(got, "event: power-1\n") {
		t.Fatalf("stream did not carry the power cell:\n%q", got)
	}
	for _, ev := range evs {
		if !strings.HasSuffix(ev.name, "-1") {
			t.Errorf("forged event %q (data %q): the update was about device 1 only", ev.name, ev.data)
		}
	}
}
