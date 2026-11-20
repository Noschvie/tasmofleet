package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/policy"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
	"github.com/hellwand/tasmofleet/internal/transport"
)

// Reading every device's GroupTopic (#79, review #279). A message to a device's topic reaches every
// device that listens on it, so tasmofleet writes or transfers over MQTT only when it knows that no
// other device has the topic as its GroupTopic. One device with an unread GroupTopic that may be on our
// broker therefore blocks MQTT writes for every MQTT-routed device (transport.MQTTFacts.OthersGroupUnread
// names it; only a device whose will is Offline or that is provably not on our broker is left out, #812),
// and the reader closes that gap without a person doing anything: shortly after start and after a device is adopted,
// one bare GroupTopic per tick, through the router. That read answers all four group topics at once (#1005; Status 1
// carries GroupTopic1 only), and it is stored only when all four came back. The router reads it over MQTT first
// (#456, #839; cmdpolicy routes the bare GroupTopic as a read, which is allowed there with an unknown GroupTopic) and
// over HTTP when MQTT cannot serve the device. Right after the upgrade to #1005 every device has GroupTopic2 to
// GroupTopic4 unread, so MQTT writes wait until the reader has asked each device once.
// The bare GroupTopic changes nothing and is never Status 0; one read per tick on the whole process. A
// device that gave no usable answer is asked again after groupTopicBackoff[n]: soon at first (right
// after start it may not be known as ours on the broker yet), then rarely.
const (
	groupTopicTick    = 2 * time.Second
	groupTopicTimeout = 20 * time.Second
)

var groupTopicBackoff = [...]time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 15 * time.Minute}

// groupTopicIdle is how long the reader does not even list the inventory after a tick that found no
// device in need of a read.
const groupTopicIdle = 30 * time.Second

// groupTry is the record of the reads that gave nothing.
type groupTry struct {
	at time.Time
	n  int  // failed reads so far
	up bool // the device was online when it was tried
}

func (g groupTry) wait() time.Duration {
	return groupTopicBackoff[max(0, min(g.n-1, len(groupTopicBackoff)-1))]
}

type groupTopicReader struct {
	list   func(context.Context) ([]model.Device, error)
	client tasmota.Client // the router
	store  interface {
		UpdateGroupTopics(ctx context.Context, id model.DeviceID, g store.ProbeGuard, topic, first string, more []string) error
	}
	health    func(model.DeviceID) model.Health
	skip      func(model.DeviceID) bool // the HTTP API is off and MQTT cannot read the device (#659): left alone like an offline one; nil reads all
	now       func() time.Time
	log       *slog.Logger
	tried     map[model.DeviceID]groupTry
	idleUntil time.Time
}

func newGroupTopicReader(list func(context.Context) ([]model.Device, error), health func(model.DeviceID) model.Health, client tasmota.Client, st interface {
	UpdateGroupTopics(ctx context.Context, id model.DeviceID, g store.ProbeGuard, topic, first string, more []string) error
}, log *slog.Logger) *groupTopicReader {
	return &groupTopicReader{list: list, health: health, client: client, store: st, now: time.Now, log: log, tried: map[model.DeviceID]groupTry{}}
}

func (r *groupTopicReader) run(ctx context.Context) {
	t := time.NewTicker(groupTopicTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// wantsGroupTopic: the device has an MQTT topic and its group topics are not known (groupTopicStale).
// A device on safeboot or minimal firmware does not know GroupTopic (#558): it is asked again once it
// runs regular firmware.
func wantsGroupTopic(d model.Device) bool {
	return d.Topic != "" && !policy.LimitedFirmware(d.Identity.Variant) && groupTopicStale(d)
}

// tick reads one device: a never tried one before a retry that is due, else the one that has waited
// longest. Offline devices are skipped (nothing could answer), and a device that was tried while it was
// not online is read again at once when it comes online.
func (r *groupTopicReader) tick(ctx context.Context) {
	now := r.now()
	if now.Before(r.idleUntil) {
		return
	}
	devs, err := r.list(ctx)
	if err != nil {
		r.log.Warn("group topic reader: list devices", "err", err)
		return
	}
	inList := make(map[model.DeviceID]bool, len(devs))
	wanting := false
	var pick *model.Device
	for i := range devs {
		d := &devs[i]
		inList[d.ID] = true
		if !wantsGroupTopic(*d) {
			delete(r.tried, d.ID)
			continue
		}
		wanting = true
		h := r.health(d.ID)
		if h.State == model.HealthOffline || r.skip != nil && r.skip(d.ID) {
			continue
		}
		up := h.State == model.HealthOnline
		last, was := r.tried[d.ID]
		if was && up && !last.up {
			delete(r.tried, d.ID) // it came online since the failed attempt: read it now
			was, last = false, groupTry{}
		}
		if was && now.Sub(last.at) < last.wait() {
			continue
		}
		if pick == nil || last.at.Before(r.tried[pick.ID].at) {
			pick = d
		}
	}
	for id := range r.tried {
		if !inList[id] {
			delete(r.tried, id) // deleted from the inventory
		}
	}
	if pick == nil {
		if !wanting {
			r.idleUntil = now.Add(groupTopicIdle)
		}
		return
	}
	t := r.tried[pick.ID]
	r.tried[pick.ID] = groupTry{at: now, n: t.n + 1, up: r.health(pick.ID).State == model.HealthOnline}
	if r.read(ctx, *pick) {
		delete(r.tried, pick.ID)
	}
}

// read puts the bare GroupTopic to d and stores its four group topics; it reports whether that worked.
func (r *groupTopicReader) read(ctx context.Context, d model.Device) bool {
	ctx, cancel := context.WithTimeout(ctx, groupTopicTimeout)
	defer cancel()
	if d.Address == "" {
		ctx = transport.WithPath(ctx, transport.MQTT) // added from the broker: there is no HTTP address to try
	}
	raw, err := r.client.Command(ctx, tasmota.TargetFor(d), groupTopicRead)
	if err != nil {
		r.log.Debug("group topic reader: GroupTopic not read", "device", d.ID, "err", err)
		return false
	}
	first, more, ok := parseGroupTopics(raw)
	if !ok {
		r.log.Debug("group topic reader: the GroupTopic answer does not hold all four usable group topics", "device", d.ID)
		return false
	}
	g := store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC, MoreGroupTopicsGen: d.MoreGroupTopicsGen}
	if err := r.store.UpdateGroupTopics(ctx, d.ID, g, d.Topic, first, more); errors.Is(err, store.ErrConflict) {
		// moved, rebound, re-topiced, or the list was marked unread since the read began: nothing stored, so the read
		// did not work, and the restore's read must say so (the list stays unread)
		r.log.Debug("group topic reader: the device changed during the read; nothing stored", "device", d.ID)
		return false
	} else if err != nil {
		r.log.Warn("group topic reader: could not store the group topics", "device", d.ID, "err", err)
		return false
	}
	return true
}

// forgetGroupTopics marks GroupTopic2 to GroupTopic4 of a device unread after TasmoFleet itself may have changed them
// (#1005): a GroupTopic<n> with a value from the console, a bulk command or an update job (holdingCmd), or a restore
// upload. The reply holding all four is not taken as a read; the reader asks the device again within a few ticks, and
// until then othersGroupUnread holds the other devices' MQTT writes back (fail closed). The store write is not
// cancelled with the background context: a write that may have reached the device must still be marked.
func (a *runtime) forgetGroupTopics(id model.DeviceID) {
	if err := a.svc.Store.ForgetGroupTopics(context.WithoutCancel(a.bgContext), id); err != nil {
		a.log.Warn("could not mark the group topics unread", "device", id, "err", err)
	}
}

// readGroupTopics is restore.Options.ReadGroupTopics: one read of the reader's (the bare GroupTopic through the
// router, stored only when all four keys came back), right after a restore has made the list unread, so the
// verifying backup over MQTT is not held back until the reader's next tick (#1005).
func (a *runtime) readGroupTopics(ctx context.Context, d model.Device) error {
	// d is the device as the restore began; the upload has marked its list unread since (ForgetGroupTopics), and the
	// store keeps a read only with the mark count it began with.
	cur, err := a.svc.Store.GetDevice(ctx, d.ID)
	if err != nil {
		return err
	}
	d.MoreGroupTopicsGen = cur.MoreGroupTopicsGen
	r := groupTopicReader{client: a.svc.Client, store: a.svc.Store, log: a.log}
	if !r.read(ctx, d) {
		return errors.New("the device's group topics could not be read")
	}
	return nil
}
