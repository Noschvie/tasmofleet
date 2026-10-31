package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Stability history (#325): the tracker notices restarts and keeps an hourly Wi-Fi sample.
//
//   - Over MQTT a restart is the device's own tele/INFO3 ("RestartReason", "BootCount"): exact, with
//     the reason, and the uptime it had before is the last one we saw.
//   - Over HTTP (and for an MQTT device whose INFO3 we missed) a restart is an uptime that dropped
//     between two readings. Then one "Status 1" (never Status 0) gives the reason and BootCount.
//   - The first time a device's uptime is known and no restart is stored for it, the same "Status 1"
//     records its last restart, so "Last restart" is not empty until the next one.
//
// Everything is memory work on the bus goroutine; the Status 1 reads and the store writes happen in
// tick's goroutines, one device at a time (the poll flag), like the polls.

const (
	// uptimeDropSlack: a reading counts as a restart only when the uptime fell by more than this.
	// An HTTP poll and an MQTT STATE of the same moment can arrive out of order, a few seconds apart.
	uptimeDropSlack = 30
	// bootCheckRetry spaces the retries of a failed "Status 1"; after bootCheckTries failures it is dropped
	// (a device on minimal firmware, or one TasmoFleet cannot reach over HTTP, never answers).
	bootCheckRetry = 5 * time.Minute
	bootCheckTries = 3
	// bootSameWindow: two sightings of a restart (INFO3 and the uptime drop) closer than this are one.
	bootSameWindow = 45 * time.Second
)

const (
	checkBaseline = "baseline" // the first known uptime: record the last restart if none is stored
	checkDrop     = "drop"     // the uptime fell: a restart TasmoFleet may not have been told about
)

type info3Doc struct {
	Info3 struct {
		// RestartReason is a string, or after a crash the crash dump, an object whose "Reason" is the
		// reset reason (support_crash_recorder.ino): reasonText takes both.
		RestartReason json.RawMessage `json:"RestartReason"`
		BootCount     int             `json:"BootCount"`
	} `json:"Info3"`
}

// reasonText is the RestartReason as text: the string itself, or the Reason of a crash dump.
func reasonText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return cleanReason(s)
	}
	var dump struct {
		Reason string `json:"Reason"`
	}
	if json.Unmarshal(raw, &dump) == nil {
		if dump.Reason == "" {
			return "Exception"
		}
		return cleanReason(dump.Reason)
	}
	return ""
}

// noteUptime runs under s.mu with a new UptimeSec reading and flags the checks it implies.
func (s *Service) noteUptime(ds *devState, sec int64) {
	if s.d.Stability == nil {
		return
	}
	switch {
	case ds.uptimeKnown && sec+uptimeDropSlack < ds.lastUptime:
		ds.dropUptime = ds.lastUptime
		ds.check, ds.checkTries, ds.checkAfter = checkDrop, 0, time.Time{}
	case !ds.uptimeKnown && !ds.baselined:
		ds.baselined = true
		ds.check, ds.checkTries, ds.checkAfter = checkBaseline, 0, time.Time{}
	}
	ds.lastUptime, ds.uptimeKnown = sec, true
}

// noteInfo3 runs under s.mu for a tele/INFO3 and returns the restart to store, if it is one.
func (s *Service) noteInfo3(ds *devState, payload []byte, now time.Time) *model.DeviceBoot {
	if s.d.Stability == nil {
		return nil
	}
	var doc info3Doc
	if json.Unmarshal(payload, &doc) != nil || (len(doc.Info3.RestartReason) == 0 && doc.Info3.BootCount == 0) {
		return nil
	}
	// Every live INFO3 is one restart (the device sends it once per boot, when it connects; a retained
	// one never gets here). Its BootCount is no key: Tasmota raises it only after 10 s of uptime, so a
	// loop of faster boots repeats it.
	b := &model.DeviceBoot{DeviceID: ds.id, At: now, BootCount: doc.Info3.BootCount, Reason: reasonText(doc.Info3.RestartReason)}
	ds.lastBootAt = now
	if ds.uptimeKnown { // after an earlier restart without a reading since, the old uptime belongs to the run before that
		b.UptimeBefore = time.Duration(ds.lastUptime) * time.Second
	}
	ds.baselined, ds.uptimeKnown, ds.check = true, false, "" // this restart is recorded: the next reading starts a new run
	return b
}

// cleanReason bounds a device-controlled text before it is stored and shown.
func cleanReason(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		s = s[:80]
	}
	return strings.ToValidUTF8(s, "")
}

// bootChecksDue lists the devices whose flagged check may run now, at most budget of them, and marks
// them busy (caller holds s.mu).
func (s *Service) bootChecksDue(now time.Time, budget int) []model.DeviceID {
	var due []model.DeviceID
	for id, ds := range s.devs {
		if budget == 0 {
			break
		}
		if ds.check == "" || ds.polling || ds.authFailed || now.Before(ds.checkAfter) || s.d.Stability == nil || s.d.Client == nil {
			continue
		}
		ds.polling = true
		budget--
		due = append(due, id)
	}
	return due
}

// checkBoot reads "Status 1" of a device flagged by noteUptime and records its restart.
func (s *Service) checkBoot(ctx context.Context, id model.DeviceID) {
	defer func() {
		s.mu.Lock()
		if ds := s.devs[id]; ds != nil {
			ds.polling = false
		}
		s.mu.Unlock()
	}()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return
	}
	s.mu.Lock()
	ds := s.devs[id]
	if ds == nil {
		s.mu.Unlock()
		return
	}
	kind, seeded := ds.check, ds.bootsSeeded
	s.mu.Unlock()

	if !seeded { // after a start the stored restarts tell when the last one was: no duplicate row for a restart already known
		last, err := s.d.Stability.ListBoots(ctx, id, 1)
		if err != nil {
			slog.Warn("health: reading stored restarts failed", "device", id, "err", err)
			s.retryCheck(id, kind)
			return
		}
		s.mu.Lock()
		if ds := s.devs[id]; ds != nil {
			ds.bootsSeeded = true
			if len(last) > 0 && ds.lastBootAt.IsZero() {
				ds.lastBootAt = last[0].At
			}
			if kind == checkBaseline && len(last) > 0 {
				ds.check = "" // a restart is stored already
				s.mu.Unlock()
				return
			}
		}
		s.mu.Unlock()
	}

	dev, err := s.d.Store.GetDevice(ctx, id)
	if err != nil {
		s.mu.Lock()
		if ds := s.devs[id]; ds != nil {
			ds.check = ""
		}
		s.mu.Unlock()
		return
	}
	pctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	raw, err := s.d.Client.Command(pctx, tasmota.TargetFor(dev), "Status 1")
	var env struct {
		StatusPRM struct {
			RestartReason string `json:"RestartReason"`
			BootCount     int    `json:"BootCount"`
		} `json:"StatusPRM"`
	}
	if err == nil && json.Unmarshal(raw, &env) != nil {
		err = tasmota.ErrBadResponse
	}
	if err != nil {
		if ctx.Err() == nil {
			s.retryCheck(id, kind)
		}
		return
	}
	now := s.d.Clock.Now()
	s.mu.Lock()
	ds = s.devs[id]
	if ds == nil {
		s.mu.Unlock()
		return
	}
	ds.check = ""
	prm := env.StatusPRM
	bootAt := now.Add(-time.Duration(ds.h.UptimeSec) * time.Second)
	if kind == checkDrop && !ds.lastBootAt.IsZero() && bootAt.Sub(ds.lastBootAt).Abs() <= bootSameWindow {
		s.mu.Unlock() // the device's own INFO3 told us about this restart already
		return
	}
	ds.lastBootAt = bootAt
	b := model.DeviceBoot{DeviceID: id, At: bootAt, BootCount: prm.BootCount, Reason: cleanReason(prm.RestartReason)}
	if kind == checkDrop {
		b.UptimeBefore = time.Duration(ds.dropUptime) * time.Second
	}
	s.publish(ds)
	s.mu.Unlock()
	b.Expected = s.expected(id)
	if err := s.d.Stability.AppendBoot(ctx, b); err != nil {
		slog.Warn("health: storing a restart failed", "device", id, "err", err)
	}
}

func (s *Service) retryCheck(id model.DeviceID, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ds := s.devs[id]
	if ds == nil {
		return
	}
	if ds.checkTries++; ds.checkTries >= bootCheckTries {
		ds.check = ""
		return
	}
	ds.check, ds.checkAfter = kind, s.d.Clock.Now().Add(bootCheckRetry)
}

// wifiSamplesDue returns the hourly samples to store now (caller holds s.mu): one per device that
// reports a signal and is online, at most one per clock hour.
func (s *Service) wifiSamplesDue(now time.Time) []model.WifiSample {
	if s.d.Stability == nil {
		return nil
	}
	hour := now.Truncate(time.Hour)
	var out []model.WifiSample
	for id, ds := range s.devs {
		if !ds.hasSig || ds.h.State != model.HealthOnline || !hour.After(ds.lastSample) {
			continue
		}
		ds.lastSample = hour
		out = append(out, model.WifiSample{DeviceID: id, At: hour, DBm: ds.sigDBm})
	}
	return out
}

func (s *Service) storeWifiSamples(ctx context.Context, samples []model.WifiSample) {
	for _, w := range samples {
		if err := s.d.Stability.AppendWifiSample(ctx, w); err != nil {
			slog.Warn("health: storing a Wi-Fi sample failed", "device", w.DeviceID, "err", err)
		}
	}
}

// parseDowntime reads Tasmota's "0T00:03:41" (days T hh:mm:ss); ok is false for anything else.
func parseDowntime(s string) (time.Duration, bool) {
	day, clock, found := strings.Cut(s, "T")
	if !found {
		return 0, false
	}
	d, err := strconv.Atoi(day)
	parts := strings.Split(clock, ":")
	if err != nil || len(parts) != 3 {
		return 0, false
	}
	var n [3]int
	for i, p := range parts {
		if n[i], err = strconv.Atoi(p); err != nil || n[i] < 0 {
			return 0, false
		}
	}
	return time.Duration(d)*24*time.Hour + time.Duration(n[0])*time.Hour + time.Duration(n[1])*time.Minute + time.Duration(n[2])*time.Second, d >= 0
}

func (s *Service) storeBoot(ctx context.Context, b model.DeviceBoot) {
	b.Expected = s.expected(b.DeviceID)
	if err := s.d.Stability.AppendBoot(ctx, b); err != nil {
		slog.Warn("health: storing a restart failed", "device", b.DeviceID, "err", err)
	}
}

// expected asks whether TasmoFleet itself was working on the device (an update or restore job).
func (s *Service) expected(id model.DeviceID) bool {
	return s.d.Expected != nil && s.d.Expected(id)
}
