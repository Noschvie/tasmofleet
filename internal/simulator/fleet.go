package simulator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type fleet struct {
	o      FleetOptions
	x      Extra
	scale  float64
	log    *slog.Logger
	client *http.Client

	mu      sync.Mutex
	devices []*simDevice
	names   map[string]bool
}

// NewFleetWith is NewFleet with the extra options.
func NewFleetWith(o FleetOptions, x Extra) (Fleet, error) {
	if st, err := os.Stat(filepath.Join(o.FixturesDir, "devices")); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("simulator: FixturesDir %q has no devices/ directory", o.FixturesDir)
	}
	f := &fleet{o: o, x: x, scale: x.TimeScale, names: map[string]bool{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if f.scale <= 0 {
		f.scale = 1
	}
	if f.x.FlashVersion == "" {
		f.x.FlashVersion = "16.0.0"
	}
	if f.x.BindAddr == "" {
		f.x.BindAddr = "127.0.0.1"
	} else if f.x.BindAddr != "127.0.0.1" {
		slog.Warn("simulator: device listeners bind to a non-loopback address", "addr", f.x.BindAddr)
	}
	f.client = &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext:       loopbackOnly(x.AllowDownloadHosts),
			DisableKeepAlives: true,
			Proxy:             nil,
		},
		// A device never follows redirects: a 3xx is "Wrong HTTP Code".
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return f, nil
}

func (f *fleet) Add(ctx context.Context, spec DeviceSpec) (Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Name == "" {
		return nil, errors.New("simulator: DeviceSpec.Name is required")
	}
	def, ok := profileDefs[spec.Profile]
	if !ok {
		return nil, fmt.Errorf("simulator: unknown profile %q", spec.Profile)
	}
	f.mu.Lock()
	if f.names[spec.Name] {
		f.mu.Unlock()
		return nil, fmt.Errorf("simulator: device %q already exists", spec.Name)
	}
	f.names[spec.Name] = true
	f.mu.Unlock()
	c, err := loadCapture(f.o.FixturesDir, def)
	if err != nil {
		return nil, err
	}
	d, err := newDevice(f, spec, def, c)
	if err != nil {
		return nil, err
	}
	d.httpAddr = net.JoinHostPort(f.x.BindAddr, "0")
	d.host = f.x.BindAddr
	d.startHTTP()
	if d.srv == nil {
		return nil, errors.New("simulator: cannot listen")
	}
	if !d.hasFaultLocked(FaultHTTPOnly) {
		d.mq = newMQTTLink(d, f.o.MQTTURL)
	}
	d.mq.connect()
	if f.o.TelePeriod > 0 && d.mq != nil {
		d.spawn(func() { d.teleLoop(f.o.TelePeriod) })
	}
	d.startFlapIfNeeded()
	f.mu.Lock()
	f.devices = append(f.devices, d)
	f.mu.Unlock()
	return d, nil
}

func (f *fleet) Devices() []Device {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Device, len(f.devices))
	for i, d := range f.devices {
		out[i] = d
	}
	return out
}

func (f *fleet) Close() error {
	f.mu.Lock()
	devs := f.devices
	f.devices = nil
	f.mu.Unlock()
	var errs []error
	for _, d := range devs {
		errs = append(errs, d.Close())
	}
	f.client.CloseIdleConnections()
	return errors.Join(errs...)
}

func (d *simDevice) startFlapIfNeeded() {
	d.mu.Lock()
	start := d.hasFaultLocked(FaultFlap) && !d.flapping && d.mq != nil
	if start {
		d.flapping = true
	}
	d.mu.Unlock()
	if start {
		d.spawn(d.flapLoop)
	}
}
