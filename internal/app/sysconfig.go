package app

import (
	"context"
	"net/netip"
	"slices"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// watch returns a channel that receives (without blocking the sender) whenever
// one of keys changed on the system settings page, and a function that stops
// the subscription. Components that can follow a setting live read the value
// from a.svc.Sys.Snapshot() each round and wait on this channel to wake up
// early.
func (a *runtime) watch(keys ...string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	unsub := a.svc.Sys.Subscribe(func(e sysconfig.Event) {
		for _, k := range keys {
			if slices.Contains(e.Keys, k) {
				select {
				case ch <- struct{}{}:
				default:
				}
				return
			}
		}
	})
	return ch, unsub
}

// everyLive runs f every interval() until ctx ends, following changes of keys
// at once. interval() <= 0 means off. With runNow, f also runs right away at
// start and whenever a change turns the schedule on.
func (a *runtime) everyLive(ctx context.Context, interval func() time.Duration, keys []string, runAtStart bool, f func(context.Context)) {
	changed, unsub := a.watch(keys...)
	defer unsub()
	first := runAtStart
	for {
		every := interval()
		if every <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				first = true // switched on: run once now
				continue
			}
		}
		if first {
			first = false
			f(ctx)
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-changed:
			t.Stop()
			first = interval() > 0 // a (re)start of the schedule runs once now
		case <-t.C:
			f(ctx)
		}
	}
}

// setupSysConfigLive wires the settings that follow the system settings page
// without a restart: the backup schedule, the scheduled network scan.
func (a *runtime) setupSysConfigLive() {
	sys := a.svc.Sys
	sys.Subscribe(a.applyLiveSettings)
	a.goBG("backup-schedule", func() error {
		a.everyLive(a.bgContext, func() time.Duration { return sys.Snapshot().BackupSchedule }, []string{"BACKUP_INTERVAL"}, true, a.backupAll)
		return nil
	})
	a.goBG("snapshot-schedule", func() error {
		a.everyLive(a.bgContext, a.snapshotWait, []string{"SNAPSHOT_INTERVAL"}, true, a.scheduledSnapshot)
		return nil
	})
	a.goBG("discovery-scan-schedule", func() error {
		a.everyLive(a.bgContext, func() time.Duration {
			if c := sys.Snapshot(); c.DiscoveryScan {
				return c.DiscoveryScanInterval
			}
			return 0
		}, []string{"DISCOVERY_SCAN", "DISCOVERY_SCAN_INTERVAL", "DISCOVERY_SCAN_CIDRS"}, false, a.scheduledScan)
		return nil
	})
}

// scheduledScan starts the configured network scan as "system". A scan that is
// still running, or CIDRs outside the running device networks, are logged and
// the round is skipped; the next round tries again.
func (a *runtime) scheduledScan(ctx context.Context) {
	cidrs := a.svc.Sys.Snapshot().DiscoveryScanCIDRs
	if a.discovery == nil || len(cidrs) == 0 {
		return
	}
	if _, err := a.discovery.StartScan(ctx, "system", cidrs); err != nil {
		a.log.Warn("scheduled discovery scan not started", "err", err)
	}
}

// discoveryFollowsSetting attaches the MQTT discovery subscriber while
// DISCOVERY_MQTT is on and detaches it when it is turned off.
func (a *runtime) discoveryFollowsSetting() {
	if a.bus == nil {
		return
	}
	var unsub func()
	apply := func(on bool) {
		switch {
		case on && unsub == nil:
			unsub = a.discovery.Attach(a.bus)
		case !on && unsub != nil:
			unsub()
			unsub = nil
		}
	}
	// Subscribe callbacks run on the goroutine that saved the settings, one at
	// a time, so apply is never concurrent with itself.
	apply(a.svc.Sys.Snapshot().DiscoveryMQTT)
	a.svc.Sys.Subscribe(func(e sysconfig.Event) {
		if slices.Contains(e.Keys, "DISCOVERY_MQTT") {
			apply(e.New.DiscoveryMQTT)
		}
	})
}

var anywhere = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}

// brokerAddrAllowed is the address rule of the MQTT connection test: the rule
// of the device client with every network allowed, which still refuses
// loopback, link-local, cloud metadata, NAT64, IPv4-compatible, multicast and
// unspecified addresses. A broker may live outside DEVICE_NETWORKS.
func brokerAddrAllowed(ip netip.Addr) bool { return tasmota.IPAllowed(ip, anywhere) }

// applyLiveSettings hands changed settings to the components that follow them
// without a restart: backup retention, the update engine, the health tracker.
func (a *runtime) applyLiveSettings(e sysconfig.Event) {
	changed := func(keys ...string) bool {
		for _, k := range keys {
			if slices.Contains(e.Keys, k) {
				return true
			}
		}
		return false
	}
	c := e.New
	if changed("BACKUP_KEEP", "BACKUP_MAX_AGE") && a.backups != nil {
		a.backups.SetRetention(c.BackupKeep, c.BackupMaxAge)
	}
	if changed("OTA_PARALLEL", "OTA_RESTORE_OTAURL", "OTA_POWER_MAX_AGE") && a.engine != nil {
		a.engine.SetTuning(ota.Tuning{MaxParallel: c.OTAParallel, RestoreOtaURL: c.OTARestoreURL, PowerRestoreMaxAge: c.OTAPowerMaxAge})
	}
	if changed("POLL_INTERVAL", "TELEPERIOD") && a.tracker != nil {
		a.tracker.SetIntervals(staleAfter(c.TelePeriod), c.PollInterval)
	}
}

// staleAfter is how long a device may stay silent before it counts as stale.
func staleAfter(tele time.Duration) time.Duration { return 2*tele + time.Minute }

// runtimeBrokerAddrAllowed is the address rule of the live MQTT connection: the
// rule of the connection test, except that loopback is allowed (a broker on the
// same host, or the development environment). Link-local, cloud metadata,
// NAT64, IPv4-compatible, multicast and unspecified stay refused.
var runtimeBrokerNets = append([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}, anywhere...)

func runtimeBrokerAddrAllowed(ip netip.Addr) bool { return tasmota.IPAllowed(ip, runtimeBrokerNets) }
