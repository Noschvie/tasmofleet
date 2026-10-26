// Package discovery finds devices that are not in the inventory yet (M2):
// Tasmota MQTT discovery messages (retained tasmota/discovery/<MAC>/config),
// the device topics themselves (LWT, STATE, INFO, an on-demand Status 5;
// spec 11.3a, mqttfirst.go)
// and an opt-in HTTP scan of configured device networks (bounded concurrency,
// only addresses inside DEVICE_NETWORKS, probe "Status 2" then "Status 5").
//
// Everything this package reads comes from the network and is untrusted: any
// host can publish a discovery message or answer a probe with any MAC, name
// or topic. Consequences, documented in docs/security/discovery.md:
//
//   - nothing is added or moved automatically unless inventory.Upsert agrees;
//     what Upsert refuses (ErrNeedsVerification, ErrIdentityMismatch) becomes
//     an "address-change" or "conflict" proposal;
//   - proposals carry no credentials and are created only by this package;
//   - accepting a proposal is an admin action that goes through inventory
//     validation and is audited.
package discovery

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Errors. Callers match with errors.Is.
var (
	ErrNotFound      = errors.New("discovery: not found")
	ErrNotAcceptable = errors.New("discovery: this proposal cannot be accepted, resolve it in the inventory")
	ErrStale         = errors.New("discovery: the proposal no longer matches the device or the network")
	ErrNotRunning    = errors.New("discovery: service is not running")
	ErrScanRunning   = errors.New("discovery: a scan is already running")
	ErrScanRange     = errors.New("discovery: scan range refused")
)

// Proposal kinds and sources (Proposal.Kind, Proposal.Via).
const (
	KindNew           = "new"            // unknown MAC
	KindAddressChange = "address-change" // known device claims a new address; needs admin confirmation
	KindConflict      = "conflict"       // address or topic belongs to a device with a different MAC
	ViaMQTT           = "mqtt"
	ViaScan           = "scan"
)

// Proposal is a device seen but not (or not as seen) in the inventory.
// Device holds what the network claimed; it never carries credentials.
type Proposal struct {
	ID     int64
	Device model.Device
	Via    string // ViaMQTT or ViaScan
	Kind   string // KindNew, KindAddressChange or KindConflict
	Reason string
	SeenAt time.Time
	// FirstSeen is the first sighting of this MAC (the Add devices page shows it). Additive.
	FirstSeen time.Time
	// ExistingID is the inventory device an address-change or conflict refers
	// to; zero for KindNew. Additive to the M2 contract.
	ExistingID model.DeviceID
	// Version identifies the content shown; AcceptVersion applies only that
	// content. Additive to the M2 contract.
	Version string
	// LWT is the last MQTT LWT of the proposal's topic ("Online", "Offline"
	// or "" when none was seen). Additive (spec 11.3a).
	LWT string
}

// ScanID names one scan run.
type ScanID string

// Service is what the web layer uses.
type Service interface {
	Proposals(ctx context.Context) ([]Proposal, error)
	// StartScan validates the ranges before any packet is sent: each CIDR
	// must lie inside DEVICE_NETWORKS and all together hold at most 1024
	// addresses. Only one scan runs at a time.
	StartScan(ctx context.Context, actor string, cidrs []netip.Prefix) (ScanID, error)
	CancelScan(ctx context.Context, actor string, id ScanID) error
	// Accept adds the device ("new") or confirms the new address
	// ("address-change") through the inventory service. A conflict cannot be
	// accepted (ErrNotAcceptable).
	Accept(ctx context.Context, actor string, id int64) (model.DeviceID, error)
	Dismiss(ctx context.Context, actor string, id int64) error
}

// ScanState is the life cycle of a scan.
type ScanState string

// Scan states.
const (
	ScanRunning   ScanState = "running"
	ScanDone      ScanState = "done"
	ScanCancelled ScanState = "cancelled"
)

// Hit is one Tasmota device a scan found.
type Hit struct {
	Address string
	Port    int
	MAC     string
	Name    string
	Version string
	// Outcome says what happened with it: "new proposal", "known device",
	// "address-change proposal", "conflict proposal", "needs password",
	// "ignored" (not usable) or "error".
	Outcome string
}

// ScanStatus is a snapshot of a scan. Hits is capped by the scan size.
type ScanStatus struct {
	ID         ScanID
	Actor      string
	CIDRs      []netip.Prefix
	State      ScanState
	Total      int // probes planned
	Done       int // probes finished
	Hits       []Hit
	StartedAt  time.Time
	FinishedAt time.Time
}
