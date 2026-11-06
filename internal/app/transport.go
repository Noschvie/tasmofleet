package app

import (
	"context"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttcmd"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// mqttPrefixes are the topic prefixes of the bus (cmnd, stat, tele). The bus is built from them and
// the transport facts expand the devices' command topics with the first one, so both use this one value.
var mqttPrefixes = [3]string{"cmnd", "stat", "tele"}

// setupTransport gives the router its facts and the MQTT command channel. Until here the router
// is the HTTP client it wraps (also in the CLI, which never calls this).
func (a *runtime) setupTransport() {
	facts := &TransportFacts{
		Devices: a.svc.Store, Health: a.tracker, Memory: a.svc.Router.Memory(), Unanswered: a.svc.Router.UnansweredReads(),
		BusConnected:    a.bus.Connected,
		FullTopic:       func() string { return a.svc.Sys.Snapshot().MQTTFullTopic },
		FirmwareBaseURL: func() string { return a.svc.Sys.Snapshot().FWBaseURL },
		BrokerPassword:  func() string { return a.svc.Sys.Snapshot().MQTTPassword }, // live: a change on the settings page counts at once
		CommandPrefix:   func() string { return mqttPrefixes[0] },
		Now:             time.Now,
	}
	a.facts = facts
	a.svc.Router.SetFacts(facts)
	a.svc.Router.SetMQTT(a.mqttCmd)
	gr := newGroupTopicReader(a.fw.list(a.svc.Inventory.List), a.tracker.Get, a.svc.Client, a.svc.Store, a.log)
	gr.skip = a.apiOffRead
	a.goBG("group-topic", func() error { gr.run(a.bgContext); return nil })
	// Status 2 facts of devices identified before they were stored (#417).
	fr := newFWRFactsReader(a.svc.Inventory.List, a.tracker.Get, a.svc.Client, a.svc.Store, a.svc.Prober.announce, a.log)
	a.goBG("fwr-facts", func() error { fr.run(a.bgContext); return nil })
}

var (
	_ transport.DeviceFacts    = (*TransportFacts)(nil)
	_ transport.MQTTDownloader = (*mqttcmd.Channel)(nil) // the router asserts this at run time: a changed signature must not silently turn backups off
)

// onlyOverMQTT reports whether commands to any of the devices travel over MQTT because HTTP fails: a
// change of its MQTT identity can then cut it off for good (cmdpolicy.WithMQTTOnly). The inventory is
// listed once for all of them.
func (a *runtime) onlyOverMQTT(ctx context.Context, ids ...model.DeviceID) bool {
	all, err := a.facts.Devices.ListDevices(ctx)
	if err != nil {
		return false
	}
	want := make(map[model.DeviceID]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for _, d := range all {
		if !want[d.ID] {
			continue
		}
		f, err := a.facts.factsFor(ctx, d, all)
		if err == nil && reachedOnlyOverMQTT(f) {
			return true
		}
	}
	return false
}

// reachedOnlyOverMQTT is true when HTTP fails and MQTT works. Not "Choose(f).Commands == MQTT": reads
// go MQTT first for every live device on our broker (#456), which says nothing about HTTP.
func reachedOnlyOverMQTT(f transport.Facts) bool {
	return f.HTTP.State == transport.HTTPFailing && transport.MQTTBlocker(f, false) == ""
}
