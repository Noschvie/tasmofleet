package app

import (
	"context"
	"time"

	"github.com/hellwand/tasmofleet/internal/config"
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
		ConnChanged:       a.svc.Router.MQTTConnChanged(),
		TopicTaken:        a.svc.Router.TopicTaken(),
		BusConnected:      a.bus.Connected,
		BusConnectedSince: a.sw.ConnectedSince, // the switch follows a broker change on the settings page
		BusWills:          a.sw.WillsHeard,
		BrokerSame:        a.svc.Broker.Same,
		FullTopic:         func() string { return a.svc.Sys.Snapshot().MQTTFullTopic },
		FirmwareBaseURL:   func() string { return a.svc.Sys.Snapshot().FWBaseURL },
		BrokerPassword:    func() string { return a.svc.Sys.Snapshot().MQTTPassword }, // live: a change on the settings page counts at once
		CommandPrefix:     func() string { return mqttPrefixes[0] },
		Now:               time.Now,
	}
	a.facts = facts
	a.svc.Router.SetFacts(facts)
	a.svc.Prober.ReadsOverMQTT = a.readsOverMQTT // the identity probe's facts follow the router's read rule (#851)
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

// onlyOverMQTT reports whether any of the devices is reached over MQTT while HTTP is not known to work: a
// change of its MQTT identity can then cut it off for good (cmdpolicy.WithMQTTOnly). The console's question.
func (a *runtime) onlyOverMQTT(ctx context.Context, ids ...model.DeviceID) bool {
	return a.anyDevice(ctx, reachedOnlyOverMQTT, ids)
}

// knownOnlyOverMQTT reports whether any of the devices is known to be reached over MQTT only: HTTP fails and
// MQTT works. The bulk preview and Make compliant's plan refuse a change of the MQTT connection up front only
// there; elsewhere the router asks HTTP right before the change (#605).
func (a *runtime) knownOnlyOverMQTT(ctx context.Context, ids ...model.DeviceID) bool {
	return a.anyDevice(ctx, func(f transport.Facts) bool {
		return f.HTTP.State == transport.HTTPFailing && transport.MQTTBlocker(f, false) == ""
	}, ids)
}

// anyDevice reports whether is holds for the facts of any of the devices. The inventory is listed once for all.
func (a *runtime) anyDevice(ctx context.Context, is func(transport.Facts) bool, ids []model.DeviceID) bool {
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
		if err == nil && is(f) {
			return true
		}
	}
	return false
}

// reachedOnlyOverMQTT is true when HTTP is not known to work (failing, or no fresh answer: not HTTPOK) and
// MQTT works: then the router would refuse a change of the MQTT connection without the person's confirmation
// unless HTTP answers its probe, so the console asks up front. HTTP silent since its last answer is caught by the
// router's probe, and the console asks then (web.consoleSend on ErrCutOff). Not "Choose(f).Commands == MQTT": reads
// go MQTT first for every live device on our broker (#456), which says nothing about HTTP.
func reachedOnlyOverMQTT(f transport.Facts) bool {
	return f.HTTP.State != transport.HTTPOK && transport.MQTTBlocker(f, false) == ""
}

// typedRoutes is web.Deps.TypedRoutes (#826): the transport the router sends the typed command cmd over now to each of
// ids (transport.Router.TypedRoute, the send's own decision), and whether the live broker URL uses TLS.
func (a *runtime) typedRoutes(ctx context.Context, cmd string, ids ...model.DeviceID) (map[model.DeviceID]transport.Path, bool) {
	out := make(map[model.DeviceID]transport.Path, len(ids))
	for _, id := range ids {
		out[id] = a.svc.Router.TypedRoute(ctx, id, cmd)
	}
	return out, brokerUsesTLS(a.svc.Sys.Snapshot().MQTTURL)
}

// brokerUsesTLS reads the scheme of the broker URL (ssl://, tls:// or mqtts://): TasmoFleet's leg to the broker is
// encrypted then. A URL that cannot be read counts as plain, the stricter of the two sentences.
func brokerUsesTLS(raw string) bool {
	b, err := config.SplitMQTTURL(raw)
	return err == nil && b.TLS
}
