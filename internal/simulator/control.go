package simulator

import (
	"context"
	"slices"
	"strings"
	"time"
)

// Controller is the test-side view of a simulated device. Every Device
// returned by a Fleet implements it: use Control(dev).
type Controller interface {
	Device
	Phase() Phase
	History() []PhaseChange // phase timeline, oldest first
	Image() string          // image name: tasmota, minimal, tasmota32, safeboot ...
	Minimal() bool
	Hardware() string
	MAC() string             // as Status 5 reports it, lower case
	OtaURL() string          // RAM value
	PersistedOtaURL() string // flash value, what the next boot reads
	SetOtaURL(url string)    // as if another tool changed it (RAM only; flash too on a normal image)
	// MQTTReady is true once the device is connected to the broker and subscribed to its
	// cmnd topics, so a command published now is received. Tests wait on it instead of
	// sleeping before they send the first command.
	MQTTReady() bool
	Boots() int              // reboots since creation
	BootCount() int          // Tasmota's persisted BootCount
	DownloadAttempts() int   // attempts of the latest OTA
	Power() bool             // relay 1
	Relays() []bool          // every relay, relay 1 first
	SetRelay(n int, on bool) // as if someone pressed the button
	SetConfigDump(b []byte)
	AddFault(Fault)
	ClearFaults()
	PowerCycle() // reboot without writing flash (RAM-only changes are lost)
	// PowerOff is the plug being pulled: the MQTT connection drops uncleanly (the
	// broker publishes the will, LWT Offline) and the HTTP port stops. The device
	// does not come back.
	PowerOff()
	// WaitFor polls until cond is true or ctx ends.
	WaitFor(ctx context.Context, cond func(Controller) bool) error
}

// Control returns the Controller of a device created by this package.
func Control(d Device) (Controller, bool) {
	c, ok := d.(Controller)
	return c, ok
}

func (d *simDevice) Addr() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.httpAddr
}
func (d *simDevice) Topic() string { return d.topic }
func (d *simDevice) Version() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.version
}
func (d *simDevice) Variant() string { return d.Image() }
func (d *simDevice) Commands() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.cmds...)
}

func (d *simDevice) Close() error {
	d.once.Do(func() {
		d.wgMu.Lock()
		d.closing = true
		d.wgMu.Unlock()
		d.cancel()
		d.stopHTTP()
		d.mq.disconnectClean()
		done := make(chan struct{})
		go func() { d.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	return nil
}

func (d *simDevice) PowerOff() {
	d.mq.dropUnclean()
	_ = d.Close() // Close never fails
}

// spawn runs fn in a goroutine that Close waits for. It returns false and
// runs nothing once Close has begun: HTTP handlers and timers keep running
// while the device closes, and a wg.Add racing with Close's wg.Wait is a data
// race (and could leave a goroutine Close never waits for).
func (d *simDevice) spawn(fn func()) bool {
	d.wgMu.Lock()
	defer d.wgMu.Unlock()
	if d.closing {
		return false
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		fn()
	}()
	return true
}

func (d *simDevice) Phase() Phase {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.phase
}
func (d *simDevice) History() []PhaseChange {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]PhaseChange(nil), d.history...)
}
func (d *simDevice) Image() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.image
}
func (d *simDevice) Minimal() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.minimalLocked()
}
func (d *simDevice) Hardware() string { return d.hardware }
func (d *simDevice) MAC() string      { return strings.ToLower(d.mac) }
func (d *simDevice) OtaURL() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runtime.otaURL
}
func (d *simDevice) PersistedOtaURL() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.persisted.otaURL
}
func (d *simDevice) SetOtaURL(url string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runtime.otaURL = url
	d.commitLocked()
}
func (d *simDevice) MQTTReady() bool { return d.mq.ready() }
func (d *simDevice) Boots() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.boots
}
func (d *simDevice) BootCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.runtime.bootCount
}
func (d *simDevice) DownloadAttempts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts
}
func (d *simDevice) Power() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.relays[0]
}

// Relays returns the state of every relay, relay 1 first.
func (d *simDevice) Relays() []bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.relays)
}

// SetRelay switches relay n (1-based) as if someone pressed the button.
func (d *simDevice) SetRelay(n int, on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n >= 1 && n <= len(d.relays) {
		d.relays[n-1] = on
	}
}
func (d *simDevice) SetConfigDump(b []byte) {
	d.mu.Lock()
	d.configDump = append([]byte(nil), b...)
	d.mu.Unlock()
}
func (d *simDevice) AddFault(f Fault) {
	d.mu.Lock()
	d.faults = append(d.faults, f)
	d.mu.Unlock()
	d.startFlapIfNeeded()
}
func (d *simDevice) ClearFaults() {
	d.mu.Lock()
	d.faults = nil
	d.mu.Unlock()
}
func (d *simDevice) PowerCycle() {
	d.mu.Lock()
	if d.phase != PhaseRun {
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	d.spawn(func() {
		d.reboot(false)
	})
}

func (d *simDevice) WaitFor(ctx context.Context, cond func(Controller) bool) error {
	for {
		d.mu.Lock()
		ch := d.phaseCh
		d.mu.Unlock()
		if cond(d) {
			return nil
		}
		select {
		case <-ch:
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
