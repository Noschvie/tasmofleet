package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/metrics"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

type memSettings struct {
	mu   sync.Mutex
	rows map[string]store.SystemSetting
}

func (m *memSettings) LoadSystemSettings(context.Context) (map[string]store.SystemSetting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]store.SystemSetting{}
	for k, v := range m.rows {
		out[k] = v
	}
	return out, nil
}

func (m *memSettings) SaveSystemSettings(_ context.Context, actor string, ch []store.SystemSettingChange) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range ch {
		if c.Delete {
			delete(m.rows, c.Key)
		} else {
			m.rows[c.Key] = store.SystemSetting{Key: c.Key, Value: c.Value, UpdatedBy: actor}
		}
	}
	return nil
}

type nopAudit struct{}

func (nopAudit) Record(context.Context, model.AuditEvent) {}

func liveRuntime(t *testing.T) *runtime {
	t.Helper()
	cfg, err := config.Load([]string{
		"TASMOFLEET_DATA_DIR=/nonexistent-tasmofleet-test", "TASMOFLEET_CONFIG_FILE=/dev/null", "TASMOFLEET_AUTH_MODE=header",
		"TASMOFLEET_TRUSTED_PROXIES=172.31.12.20/32", "TASMOFLEET_OPERATOR_GROUPS=ops",
	})
	if err != nil {
		t.Fatal(err)
	}
	sys, err := sysconfig.New(context.Background(), sysconfig.Options{Base: cfg, Store: &memSettings{rows: map[string]store.SystemSetting{}}, Audit: nopAudit{}})
	if err != nil {
		t.Fatal(err)
	}
	return &runtime{svc: &Services{Sys: sys}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// Why: a schedule that is switched on or changed on the settings page must
// start working at once, and switching it off must stop it, without a restart.
func TestScheduleFollowsTheSettingsPageLive(t *testing.T) {
	a := liveRuntime(t)
	var every atomic.Int64 // 0 = off
	var runs atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		a.everyLive(ctx, func() time.Duration { return time.Duration(every.Load()) }, []string{"BACKUP_INTERVAL"}, false, func(context.Context) { runs.Add(1) })
		close(done)
	}()
	apply := func(v string) {
		t.Helper()
		if _, err := a.svc.Sys.Apply(ctx, sysconfig.Request{Actor: "a", Values: map[string]string{"BACKUP_INTERVAL": v}}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(60 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatal("a schedule that is off must not run")
	}
	every.Store(int64(20 * time.Millisecond))
	apply("2m") // the page saved a new interval
	waitFor(t, func() bool { return runs.Load() >= 3 }, "the schedule must start and tick after the change")
	every.Store(0)
	apply("0s")
	time.Sleep(40 * time.Millisecond)
	n := runs.Load()
	time.Sleep(80 * time.Millisecond)
	if runs.Load() != n {
		t.Fatal("a schedule switched off must stop")
	}
	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// Why (review F4): the connection test must not reach the host itself, the
// cloud metadata service or NAT64 aliases of it, but a broker on the LAN or on
// the internet is fine.
func TestBrokerAddressRule(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "64:ff9b::a9fe:a9fe", "::10.0.0.1", "0.0.0.0", "224.0.0.1"} {
		if brokerAddrAllowed(netip.MustParseAddr(ip)) {
			t.Errorf("%s must be refused", ip)
		}
	}
	for _, ip := range []string{"172.31.13.5", "10.1.2.3", "203.0.113.9", "2001:db8::1"} {
		if !brokerAddrAllowed(netip.MustParseAddr(ip)) {
			t.Errorf("%s must be allowed", ip)
		}
	}
}

// Why: the MQTT broker can be changed on the settings page while the server runs:
// consumers (health, discovery, console) keep one subscription and must follow.
func TestMQTTFollowsTheSettingsPageLive(t *testing.T) {
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() })
	a := liveRuntime(t)
	a.bgContext = t.Context()
	a.svc.Std = metrics.NewStandard(metrics.NewRegistry(), "test", false)
	a.mqttKnown = newKnownIndex()
	a.sw = mqttbus.NewSwitch()
	a.bus = a.sw
	a.connectMQTT(t.Context(), a.svc.Sys.Snapshot())
	if a.sw.Enabled() {
		t.Fatal("no broker configured: MQTT is off")
	}
	a.svc.Sys.Subscribe(func(e sysconfig.Event) {
		if slices.Contains(e.Keys, "MQTT_URL") {
			a.connectMQTT(t.Context(), e.New)
		}
	})
	if _, err := a.svc.Sys.Apply(t.Context(), sysconfig.Request{Actor: "t", Values: map[string]string{"MQTT_URL": br.URL()}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return a.sw.Enabled() && a.sw.Connected() }, "the new broker must be connected without a restart")
	if _, err := a.svc.Sys.Apply(t.Context(), sysconfig.Request{Actor: "t", Values: map[string]string{"MQTT_URL": ""}}); err != nil {
		t.Fatal(err)
	}
	if a.sw.Enabled() || a.sw.Connected() {
		t.Fatal("an empty URL turns MQTT off")
	}
}

// Why: the always-blocked ranges stay blocked whatever the configuration says; the
// live broker connection may reach loopback (same host) but never the metadata
// service or link-local space.
func TestRuntimeBrokerAddressRule(t *testing.T) {
	for _, ip := range []string{"169.254.169.254", "fe80::1", "64:ff9b::a9fe:a9fe", "::10.0.0.1", "0.0.0.0", "224.0.0.1"} {
		if runtimeBrokerAddrAllowed(netip.MustParseAddr(ip)) {
			t.Errorf("%s must be refused", ip)
		}
	}
	for _, ip := range []string{"127.0.0.1", "::1", "172.31.12.11", "203.0.113.9"} {
		if !runtimeBrokerAddrAllowed(netip.MustParseAddr(ip)) {
			t.Errorf("%s must be allowed", ip)
		}
	}
}

// Why: the setup suggests the networks the container itself sits in. Only private IPv4
// subnets qualify, and never wider than a /16.
func TestPrivateSubnetsSuggestion(t *testing.T) {
	mk := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	got := privateSubnets([]net.Addr{mk("172.31.12.61/24"), mk("127.0.0.1/8"), mk("169.254.3.4/16"), mk("203.0.113.9/24"), mk("fd00::5/64"),
		mk("10.1.2.3/8"), mk("172.31.12.99/24"), &net.UnixAddr{Name: "x", Net: "unix"}})
	want := []netip.Prefix{netip.MustParsePrefix("172.31.12.0/24"), netip.MustParsePrefix("10.1.0.0/16")}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
