package model

import "time"

// HealthState is the summarised reachability of a device.
type HealthState string

// Health states, see spec section "Health model".
const (
	HealthUnknown HealthState = "unknown" // never heard of since start
	HealthOnline  HealthState = "online"  // LWT Online and telemetry fresh, or last HTTP poll ok
	HealthStale   HealthState = "stale"   // LWT Online but telemetry older than the stale threshold
	HealthOffline HealthState = "offline" // LWT Offline, or HTTP poll failed repeatedly
	// HealthAuthFailed: the device answers but rejects our credentials
	// (HTTP 401 or {"WARNING":"Need user=..."}), never counted as online.
	HealthAuthFailed HealthState = "auth_failed"
)

// HealthSource says which channel produced the latest observation.
type HealthSource string

// Health sources.
const (
	SourceMQTT HealthSource = "mqtt"
	SourceHTTP HealthSource = "http"
)

// Health is the current health of one device. It lives in memory (health
// package) and is persisted only as LastSeen/State on change.
type Health struct {
	DeviceID  DeviceID
	State     HealthState
	Source    HealthSource
	LastSeen  time.Time // last successful observation of any kind
	SeenMQTT  time.Time // last message on our broker (shown per channel in the UI)
	SeenHTTP  time.Time // last successful HTTP poll
	LWT       string    // "Online", "Offline" or "" when no MQTT
	UptimeSec int64
	RSSI      int // Wifi.RSSI percentage from STATE, 0 if unknown
	// SignalDBm is Wifi.Signal from STATE/Status 11 (0 = not reported). LinkCount is Wifi.LinkCount,
	// how often the device connected to its access point since it started (0 = not reported).
	SignalDBm int
	LinkCount int
	// WifiDowntime is Wifi.Downtime: how long the device was disconnected since it started.
	WifiDowntime time.Duration
	Power        map[string]string // "POWER1" -> "ON"; from STATE/RESULT
	Since        time.Time         // when State last changed
}
