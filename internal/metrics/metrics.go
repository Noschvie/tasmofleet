// Package metrics exposes Prometheus metrics in the text exposition format
// without depending on client_golang (counters and gauges with labels are
// all tasmofleet needs; this keeps ~15 transitive modules out). Served on
// /metrics, optionally protected by a bearer token (TASMOFLEET_METRICS_TOKEN).
//
// Metric names (contract with deploy/grafana):
//
//	tasmofleet_devices{health}                     gauge
//	tasmofleet_ota_jobs_active                     gauge
//	tasmofleet_ota_jobs_total{result}              counter
//	tasmofleet_firmware_cache_bytes                gauge
//	tasmofleet_firmware_index_age_seconds          gauge
//	tasmofleet_discovery_proposals_dropped_total{kind} counter
//	tasmofleet_mqtt_dropped_total{reason}          counter
//	tasmofleet_backup_last_success_timestamp_seconds{device} gauge
//	tasmofleet_mqtt_connected                      gauge
//	tasmofleet_http_requests_total{route,code}     counter
//	tasmofleet_audit_errors_total                  counter
//	tasmofleet_build_info{version,decoder}         gauge
//
// Per-device gauges, exported only while METRICS_DEVICE_LABELS is on (see
// docs/metrics.md). Labels: device (inventory name), id (inventory id), plus
// channel (energy) or relay (relay_on):
//
//	tasmofleet_device_up{device,id}                          gauge
//	tasmofleet_device_uptime_seconds{device,id}              gauge
//	tasmofleet_device_wifi_rssi_percent{device,id}           gauge
//	tasmofleet_device_wifi_signal_dbm{device,id}             gauge
//	tasmofleet_device_power_watts{device,id,channel}         gauge
//	tasmofleet_device_energy_total_kwh{device,id,channel}    gauge
//	tasmofleet_device_energy_today_kwh{device,id,channel}    gauge
//	tasmofleet_device_energy_yesterday_kwh{device,id,channel} gauge
//	tasmofleet_device_relay_on{device,id,relay}              gauge
//	tasmofleet_device_last_seen_timestamp_seconds{device,id} gauge
package metrics

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ContractMetrics maps every metric name of the contract above to its type.
// deploy/grafana/tasmofleet.json may reference only these names (tested).
var ContractMetrics = map[string]string{
	"tasmofleet_devices":                               "gauge",
	"tasmofleet_ota_jobs_active":                       "gauge",
	"tasmofleet_ota_jobs_total":                        "counter",
	"tasmofleet_firmware_cache_bytes":                  "gauge",
	"tasmofleet_firmware_index_age_seconds":            "gauge",
	"tasmofleet_discovery_proposals_dropped_total":     "counter",
	"tasmofleet_mqtt_dropped_total":                    "counter",
	"tasmofleet_backup_last_success_timestamp_seconds": "gauge",
	"tasmofleet_mqtt_connected":                        "gauge",
	"tasmofleet_http_requests_total":                   "counter",
	"tasmofleet_audit_errors_total":                    "counter",
	"tasmofleet_build_info":                            "gauge",

	"tasmofleet_device_up":                          "gauge",
	"tasmofleet_device_uptime_seconds":              "gauge",
	"tasmofleet_device_wifi_rssi_percent":           "gauge",
	"tasmofleet_device_wifi_signal_dbm":             "gauge",
	"tasmofleet_device_power_watts":                 "gauge",
	"tasmofleet_device_energy_total_kwh":            "gauge",
	"tasmofleet_device_energy_today_kwh":            "gauge",
	"tasmofleet_device_energy_yesterday_kwh":        "gauge",
	"tasmofleet_device_relay_on":                    "gauge",
	"tasmofleet_device_last_seen_timestamp_seconds": "gauge",
}

// Registry holds metric families.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
	token    [sha256.Size]byte
	hasToken bool
}

type family struct {
	name, help, typ string
	labels          []string
	mu              sync.Mutex
	series          map[string]*Value // key: label values joined by \xff
}

// Value is one time series. Safe for concurrent use.
type Value struct {
	reg  *family
	vals []string
	v    float64
}

// Vec is a family of series sharing a name and label names.
type Vec struct{ f *family }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{families: map[string]*family{}} }

// Counter registers a counter family. Panics on a duplicate name (startup bug).
func (r *Registry) Counter(name, help string, labels ...string) *Vec {
	return r.add(name, help, "counter", labels)
}

// Gauge registers a gauge family. Panics on a duplicate name (startup bug).
func (r *Registry) Gauge(name, help string, labels ...string) *Vec {
	return r.add(name, help, "gauge", labels)
}

func (r *Registry) add(name, help, typ string, labels []string) *Vec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.families[name]; ok {
		panic("metrics: duplicate metric " + name)
	}
	f := &family{name: name, help: help, typ: typ, labels: labels, series: map[string]*Value{}}
	r.families[name] = f
	return &Vec{f: f}
}

// With returns the series for the label values (created on first use).
// Panics if the number of values differs from the declared labels.
func (v *Vec) With(values ...string) *Value {
	f := v.f
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s expects %d label values, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.series[key]
	if !ok {
		s = &Value{reg: f, vals: append([]string(nil), values...)}
		f.series[key] = s
	}
	return s
}

// Reset removes all series, so vanished entities stop being exported.
func (v *Vec) Reset() {
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	v.f.series = map[string]*Value{}
}

// Set sets a gauge.
func (s *Value) Set(x float64) { s.reg.mu.Lock(); s.v = x; s.reg.mu.Unlock() }

// Inc adds 1.
func (s *Value) Inc() { s.Add(1) }

// Add adds x. Negative values are ignored for counters (they must never decrease).
func (s *Value) Add(x float64) {
	if s.reg.typ == "counter" && x < 0 {
		return
	}
	s.reg.mu.Lock()
	s.v += x
	s.reg.mu.Unlock()
}

// SetBearerToken requires "Authorization: Bearer <token>" on the handler.
// An empty token leaves /metrics open. Safe to call while serving: the next
// request sees the new token.
func (r *Registry) SetBearerToken(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hasToken = token != ""
	r.token = sha256.Sum256([]byte(token))
}

// Handler serves the exposition format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !r.authorised(req) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.write(w)
	})
}

func (r *Registry) authorised(req *http.Request) bool {
	r.mu.Lock()
	has, want := r.hasToken, r.token
	r.mu.Unlock()
	if !has {
		return true
	}
	scheme, tok, ok := strings.Cut(req.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func (r *Registry) write(w io.Writer) {
	r.mu.Lock()
	fams := make([]*family, 0, len(r.families))
	for _, f := range r.families {
		fams = append(fams, f)
	}
	r.mu.Unlock()
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })

	var b strings.Builder
	for _, f := range fams {
		f.mu.Lock()
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(f.help), f.name, f.typ)
		for _, k := range keys {
			s := f.series[k]
			b.WriteString(f.name)
			if len(f.labels) > 0 {
				// Labels in alphabetical order, like client_golang.
				idx := make([]int, len(f.labels))
				for i := range idx {
					idx[i] = i
				}
				sort.Slice(idx, func(a, c int) bool { return f.labels[idx[a]] < f.labels[idx[c]] })
				b.WriteByte('{')
				for n, i := range idx {
					if n > 0 {
						b.WriteByte(',')
					}
					b.WriteString(f.labels[i] + `="` + labelEscaper.Replace(s.vals[i]) + `"`)
				}
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(strconv.FormatFloat(s.v, 'g', -1, 64))
			b.WriteByte('\n')
		}
		f.mu.Unlock()
	}
	_, _ = io.WriteString(w, b.String()) // a failed write means the scraper went away
}

// Standard holds the contract metrics. Producers (inventory, ota, firmware,
// backup, mqtt, web, audit) update these fields.
type Standard struct {
	Devices       *Vec // {health}
	OTAJobsActive *Vec // no labels
	OTAJobsTotal  *Vec // {result}
	FirmwareCache *Vec // no labels
	// FirmwareIndexAge: seconds since the upstream index was last refreshed
	// (since process start if never); no labels.
	FirmwareIndexAge *Vec
	// DiscoveryDropped counts proposals not stored because the limit was
	// reached ({kind}: new, address-change, conflict).
	DiscoveryDropped *Vec
	// MQTTDropped counts MQTT messages or retained cache entries the bus
	// dropped ({reason}).
	MQTTDropped   *Vec
	BackupLastOK  *Vec // {device}
	MQTTConnected *Vec // no labels
	HTTPRequests  *Vec // {route,code}
	AuditErrors   *Vec // no labels

	// Per-device gauges, filled by SetDevices only with METRICS_DEVICE_LABELS.
	DeviceUp, DeviceUptime, DeviceRSSI, DeviceSignal, DeviceLastSeen *Vec // {device,id}
	DevicePower, DeviceEnergyTotal, DeviceEnergyToday                *Vec // {device,id,channel}
	DeviceEnergyYesterday                                            *Vec // {device,id,channel}
	DeviceRelayOn                                                    *Vec // {device,id,relay}
}

// NewStandard registers the contract metrics on r and sets build_info.
func NewStandard(r *Registry, version string, decoder bool) *Standard {
	s := &Standard{
		Devices:       r.Gauge("tasmofleet_devices", "Devices by health state.", "health"),
		OTAJobsActive: r.Gauge("tasmofleet_ota_jobs_active", "OTA jobs currently in a non-terminal state."),
		OTAJobsTotal:  r.Counter("tasmofleet_ota_jobs_total", "Finished OTA jobs by result.", "result"),
		FirmwareCache: r.Gauge("tasmofleet_firmware_cache_bytes", "Size of the firmware cache on disk."),
		FirmwareIndexAge: r.Gauge("tasmofleet_firmware_index_age_seconds",
			"Seconds since the upstream firmware index was last refreshed successfully (since start if never)."),
		MQTTDropped:      r.Counter("tasmofleet_mqtt_dropped_total", "MQTT messages or retained-cache entries dropped by the bus.", "reason"),
		DiscoveryDropped: r.Counter("tasmofleet_discovery_proposals_dropped_total", "Discovery proposals not stored because the limit was reached.", "kind"),
		BackupLastOK:     r.Gauge("tasmofleet_backup_last_success_timestamp_seconds", "Unix time of the last successful backup per device.", "device"),
		MQTTConnected:    r.Gauge("tasmofleet_mqtt_connected", "1 if the MQTT connection is up."),
		HTTPRequests:     r.Counter("tasmofleet_http_requests_total", "HTTP requests by route pattern and status code.", "route", "code"),
		AuditErrors:      r.Counter("tasmofleet_audit_errors_total", "Failed audit writes."),

		DeviceUp:              r.Gauge("tasmofleet_device_up", "1 if the device is online, 0 if stale, offline or rejecting credentials. Devices never observed since start have no series.", "device", "id"),
		DeviceUptime:          r.Gauge("tasmofleet_device_uptime_seconds", "Device uptime from the last STATE or Status 11.", "device", "id"),
		DeviceRSSI:            r.Gauge("tasmofleet_device_wifi_rssi_percent", "Wi-Fi signal quality in percent (Wifi.RSSI).", "device", "id"),
		DeviceSignal:          r.Gauge("tasmofleet_device_wifi_signal_dbm", "Wi-Fi signal strength in dBm (Wifi.Signal).", "device", "id"),
		DevicePower:           r.Gauge("tasmofleet_device_power_watts", "Active power from the last ENERGY reading of an online device. Empty channel label: single meter.", "device", "id", "channel"),
		DeviceEnergyTotal:     r.Gauge("tasmofleet_device_energy_total_kwh", "Total energy counter of the device meter in kWh.", "device", "id", "channel"),
		DeviceEnergyToday:     r.Gauge("tasmofleet_device_energy_today_kwh", "Energy since midnight in kWh.", "device", "id", "channel"),
		DeviceEnergyYesterday: r.Gauge("tasmofleet_device_energy_yesterday_kwh", "Energy of yesterday in kWh.", "device", "id", "channel"),
		DeviceRelayOn:         r.Gauge("tasmofleet_device_relay_on", "1 if the relay is on, 0 if off. Single-relay devices use relay \"1\".", "device", "id", "relay"),
		DeviceLastSeen:        r.Gauge("tasmofleet_device_last_seen_timestamp_seconds", "Unix time of the last successful observation of the device.", "device", "id"),
	}
	r.Gauge("tasmofleet_build_info", "Build information.", "version", "decoder").With(version, strconv.FormatBool(decoder)).Set(1)
	// Label-less series exist from the start so dashboards show 0, not "no data".
	s.OTAJobsActive.With().Set(0)
	s.FirmwareCache.With().Set(0)
	s.MQTTConnected.With().Set(0)
	s.AuditErrors.With()
	return s
}
