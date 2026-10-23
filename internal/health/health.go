// Package health tracks the reachability of every inventory device from
// MQTT (LWT, STATE, RESULT, POWER) and, as a fallback, an HTTP poll of
// "Status 11" (never "Status 0": it bursts 12 messages and has been seen to
// corrupt payloads on ESP8266 with <20 kB heap).
//
// State rules (thresholds configurable, see spec "Health model"):
//
//	online  - LWT Online and any message within StaleAfter, or last HTTP poll ok
//	stale   - LWT Online but nothing within StaleAfter (default 2*TelePeriod+60s = 660s)
//	offline - LWT Offline, or OfflineAfter consecutive HTTP poll failures (default 3)
//	unknown - nothing observed since start
//
// The HTTP poll runs only for devices without MQTT topic or in stale state,
// with jitter and a small concurrency limit (default 4).
package health

import (
	"context"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Options configure the tracker.
type Options struct {
	StaleAfter   time.Duration
	OfflineAfter int
	PollInterval time.Duration // default 5m
	PollParallel int
}

// Tracker exposes the current health of all devices.
type Tracker interface {
	Get(id model.DeviceID) model.Health
	All() map[model.DeviceID]model.Health
	// Run consumes bus messages and runs the poller until ctx is done.
	Run(ctx context.Context) error
}
