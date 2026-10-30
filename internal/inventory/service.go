package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nofuturekid/tasmofleet/internal/netaddr"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// ErrIdentityMismatch is returned by Upsert when an incoming device matches an
// existing one by topic or address but carries a different MAC: the address
// now belongs to other hardware and must not be merged into the old record.
var ErrIdentityMismatch = errors.New("inventory: matching device has a different MAC")

// ErrNeedsVerification is returned by Upsert when a change would point a
// record at a different address, or bind it to a MAC, and that change cannot
// be accepted from an untrusted source. Nothing is changed and no credential
// is sent anywhere. An admin can still make the change through Update.
var ErrNeedsVerification = errors.New("inventory: change needs verification of the device MAC")

// ErrConflict is returned by Upsert when the record changed while the new
// address was being verified. Nothing is written; the caller may retry.
var ErrConflict = errors.New("inventory: device changed concurrently")

// ErrMoveUnverified is returned by Update (and by discovery's "Update address")
// when a device with a stored password would be moved to an address that did
// not prove the device's MAC. The admin can confirm it explicitly, naming that
// address (WithUnverifiedMoveConfirmed); then the password goes to a host that
// may be an impostor, and the audit entry says "mac_unverified".
var ErrMoveUnverified = errors.New("inventory: could not verify that the new address is the same device (MAC)")

// UnverifiedMoveError is the typed form of ErrMoveUnverified: Target is the
// address:port the confirmation has to name (MoveTarget).
type UnverifiedMoveError struct{ Target string }

func (e *UnverifiedMoveError) Error() string {
	return fmt.Sprintf("%s: the stored password would be sent to %s", ErrMoveUnverified, e.Target)
}
func (e *UnverifiedMoveError) Is(t error) bool { return t == ErrMoveUnverified }

type unverifiedKey struct{}

// MoveTarget is the address:port string an unverified move is confirmed for.
func MoveTarget(address string, port int) string {
	if port == 0 {
		port = 80
	}
	return fmt.Sprintf("%s:%d", address, port)
}

// WithUnverifiedMoveConfirmed marks ctx as coming from a request in which the
// admin confirmed, for exactly this target, that the stored password may go to
// an address that could not prove the device's MAC.
func WithUnverifiedMoveConfirmed(ctx context.Context, target string) context.Context {
	return context.WithValue(ctx, unverifiedKey{}, target)
}

// UnverifiedMoveConfirmed reports whether ctx carries that confirmation for target.
func UnverifiedMoveConfirmed(ctx context.Context, target string) bool {
	t, ok := ctx.Value(unverifiedKey{}).(string)
	return ok && t == target
}

// Verifier reports which MAC answers at an address. It must probe WITHOUT
// credentials (Status 5 without user/password), connect only to the given
// address (Upsert passes IP literals only when it verifies a move), not follow
// redirects and not use a proxy. A device that demands authentication yields
// an error, which Upsert treats as "not verified".
//
// The reported MAC is self-asserted: any host can claim any MAC. Upsert
// therefore relies on it only where a lie leaks nothing: it never moves a
// record that holds (or would get) credentials, so a rogue host that claims a
// known MAC gains at most a credential-less record.
type Verifier interface {
	VerifyMAC(ctx context.Context, address string, port int) (mac string, err error)
}

// Publisher receives inventory change events (satisfied by *events.Hub).
type Publisher interface {
	Publish(e events.Event)
}

// Options configure the service.
type Options struct {
	Store store.DeviceStore
	Audit audit.Recorder // required
	// Events is optional. Published payloads never carry the device password.
	Events Publisher
	// Networks are DEVICE_NETWORKS. IP addresses outside them are refused;
	// with no networks no IP address is accepted.
	Networks []netip.Prefix
	// NetworkSet, if set, replaces Networks and can change while the service runs.
	NetworkSet *tasmota.NetworkSet
	Clock      clock.Clock // default clock.Real
	// Verifier confirms the MAC at a new address before Upsert moves a
	// record. Without one, Upsert never changes the address of an existing device.
	Verifier Verifier
}

// Manager implements Service.
type Manager struct {
	st    store.DeviceStore
	audit audit.Recorder
	pub   Publisher
	nets  []netip.Prefix
	set   *tasmota.NetworkSet
	clk   clock.Clock
	ver   Verifier
	// mu serialises every write of this service, so that Upsert can verify
	// without holding it and then re-check the record before it writes.
	mu sync.Mutex
	// refused remembers on which day a refused upsert of a device was audited,
	// so one device that keeps announcing a new address costs one audit row
	// per day. It is in memory: a restart may allow one more row.
	rmu     sync.Mutex
	refused map[model.DeviceID]string
}

var _ Service = (*Manager)(nil)

// networks returns the device networks in force now.
func (m *Manager) networks() []netip.Prefix {
	if m.set != nil {
		return m.set.Get()
	}
	return m.nets
}

// New returns the inventory service.
func New(o Options) (*Manager, error) {
	if o.Store == nil || o.Audit == nil {
		return nil, errors.New("inventory: Store and Audit are required")
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	return &Manager{st: o.Store, audit: o.Audit, pub: o.Events, nets: o.Networks, set: o.NetworkSet, clk: o.Clock, ver: o.Verifier, refused: map[model.DeviceID]string{}}, nil
}

const (
	maxNameLen = 100
	maxNoteLen = 4000
	maxTagLen  = 50
	maxTags    = 32
)

var (
	hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	topicBad  = regexp.MustCompile(`[\s#+/]`)
	macHex    = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// ValidTopic reports whether t is a topic that may be taken from a device's own reply and
// stored: non-empty, at most 100 bytes, no whitespace, control or invisible characters, no MQTT
// wildcard or level separator, and no "%" (a placeholder Tasmota has not resolved).
func ValidTopic(t string) bool {
	return t != "" && len(t) <= 100 && !strings.ContainsAny(t, "#+/%") &&
		strings.IndexFunc(t, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) < 0
}

// CleanName makes a device-supplied name storable: control and invisible characters (bidi
// overrides, zero-width) are dropped, the rest trimmed and cut to the name limit. It may be empty.
func CleanName(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s))
	if r := []rune(s); len(r) > maxNameLen {
		s = strings.TrimSpace(string(r[:maxNameLen]))
	}
	return s
}

// NormaliseMAC returns the MAC in lower case, colon separated form. It accepts
// aa:bb:..., AA-BB-..., aabb.ccdd.eeff and bare hex. An empty string is valid
// (MAC unknown).
func NormaliseMAC(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	hex := strings.NewReplacer(":", "", "-", "", ".", "").Replace(s)
	if !macHex.MatchString(hex) {
		return "", fmt.Errorf("%w: MAC %q", ErrInvalid, s)
	}
	parts := make([]string, 6)
	for i := range parts {
		parts[i] = hex[2*i : 2*i+2]
	}
	return strings.Join(parts, ":"), nil
}

// validate normalises d in place and checks it.
func (m *Manager) validate(d *model.Device) error {
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" || len([]rune(d.Name)) > maxNameLen || strings.IndexFunc(d.Name, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: name must be 1-%d printable characters", ErrInvalid, maxNameLen)
	}
	addr, err := m.checkAddress(d.Address)
	if err != nil {
		return err
	}
	d.Address = addr
	switch {
	case d.Port == 0:
		d.Port = 80 // stored normalised so (address, port) is unique regardless of spelling
	case d.Port < 1 || d.Port > 65535:
		return fmt.Errorf("%w: port %d", ErrInvalid, d.Port)
	}
	if d.MAC, err = NormaliseMAC(d.MAC); err != nil {
		return err
	}
	if d.MACs, err = normaliseMACs(d.MAC, d.MACs); err != nil {
		return err
	}
	if d.HTTPPassword == "" {
		d.HTTPUser = "" // a web user without a password means no auth (spec 11.1)
	}
	d.Topic = strings.TrimSpace(d.Topic)
	if d.Topic != "" && (topicBad.MatchString(d.Topic) || strings.IndexFunc(d.Topic, unicode.IsControl) >= 0) {
		return fmt.Errorf("%w: topic must not contain whitespace, '/', '#' or '+'", ErrInvalid)
	}
	d.FullTopic = strings.TrimSpace(d.FullTopic)
	if len(d.Notes) > maxNoteLen {
		return fmt.Errorf("%w: notes longer than %d bytes", ErrInvalid, maxNoteLen)
	}
	if d.Tags, err = normaliseTags(d.Tags); err != nil {
		return err
	}
	return nil
}

// normaliseMACs returns the normalised MAC set: primary first, no empties or
// duplicates. A device without any MAC has a nil set.
func normaliseMACs(primary string, extra []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range append([]string{primary}, extra...) {
		m, err := NormaliseMAC(raw)
		if err != nil {
			return nil, err
		}
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out, nil
}

func normaliseTags(tags []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		if len([]rune(t)) > maxTagLen || strings.IndexFunc(t, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("%w: tag %q", ErrInvalid, t)
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > maxTags {
		return nil, fmt.Errorf("%w: more than %d tags", ErrInvalid, maxTags)
	}
	return out, nil
}

// CheckAddress reports whether address would be accepted by Create, Update and
// Upsert: nil, or an error wrapping ErrInvalid (ErrOutsideNetwork for an IP
// outside the device networks). It stores and contacts nothing.
func (m *Manager) CheckAddress(address string) error {
	_, err := m.checkAddress(address)
	return err
}

// checkAddress accepts an IP literal inside the device networks or a DNS name.
// A DNS name cannot be checked here; tasmota.Client verifies the resolved
// address at dial time, which also defeats DNS rebinding.
func (m *Manager) checkAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("%w: address is empty", ErrInvalid)
	}
	if ip, err := netip.ParseAddr(s); err == nil {
		ip = ip.Unmap()
		// Same rule as the dialer: loopback and link-local only through an
		// explicit narrow DEVICE_NETWORKS entry (e.g. 127.0.0.0/8 for the
		// simulator), metadata/multicast/unspecified/zoned never.
		if !tasmota.IPAllowed(ip, m.networks()) {
			return "", fmt.Errorf("%w: %w: %s", ErrInvalid, ErrOutsideNetwork, ip)
		}
		return ip.String(), nil
	}
	host := strings.ToLower(strings.TrimSuffix(s, "."))
	if len(host) > 253 {
		return "", fmt.Errorf("%w: address too long", ErrInvalid)
	}
	for _, l := range strings.Split(host, ".") {
		if !hostLabel.MatchString(l) {
			return "", fmt.Errorf("%w: address %q must be an IP or DNS name without scheme, port or path", ErrInvalid, s)
		}
	}
	return host, nil
}

// List returns all devices.
func (m *Manager) List(ctx context.Context) ([]model.Device, error) { return m.st.ListDevices(ctx) }

// Get returns one device.
func (m *Manager) Get(ctx context.Context, id model.DeviceID) (model.Device, error) {
	return m.st.GetDevice(ctx, id)
}

// Create validates and stores a new device.
func (m *Manager) Create(ctx context.Context, actor string, d model.Device) (model.DeviceID, error) {
	m.mu.Lock()
	id, err := m.create(ctx, d)
	m.mu.Unlock()
	m.record(ctx, actor, "device.create", id, createDetail(d), err)
	if err != nil {
		return 0, err
	}
	d.ID = id
	m.publish(d)
	return id, nil
}

func (m *Manager) create(ctx context.Context, d model.Device) (model.DeviceID, error) {
	if err := m.validate(&d); err != nil {
		return 0, err
	}
	now := m.clk.Now().UTC()
	d.CreatedAt, d.UpdatedAt = now, now
	return m.st.CreateDevice(ctx, d)
}

// Update replaces the editable fields of a device. The write is a full
// replacement: callers that do not hold a new password must pass the stored
// one (Get returns it in memory), because an empty HTTPPassword clears it.
// Identity and health are not touched.
func (m *Manager) Update(ctx context.Context, actor string, d model.Device) error {
	// An address or port change keeps the stored password and sends it to the
	// new target. That is the admin's call (audit row 15, 2026-10-21): the form
	// warns when the MAC is proven; when it is not, the admin must confirm that
	// address explicitly (ErrMoveUnverified). The probe runs before the lock.
	unverified := false
	if cur, gerr := m.st.GetDevice(ctx, d.ID); gerr == nil {
		var cerr error
		if unverified, cerr = m.checkCredentialedMove(ctx, cur, d); cerr != nil {
			m.record(ctx, actor, "device.update", d.ID, map[string]any{}, cerr)
			return cerr
		}
	}
	m.mu.Lock()
	old, err := m.st.GetDevice(ctx, d.ID)
	if err == nil {
		err = m.validate(&d)
	}
	if err == nil {
		err = m.st.UpdateDevice(ctx, d)
	}
	m.mu.Unlock()
	detail := map[string]any{}
	if err == nil {
		detail = updateDetail(old, d)
		if unverified {
			detail["mac_unverified"] = true
		}
	}
	m.record(ctx, actor, "device.update", d.ID, detail, err)
	if err != nil {
		return err
	}
	if got, gerr := m.st.GetDevice(ctx, d.ID); gerr == nil {
		d = got
	}
	m.publish(d)
	return nil
}

// Delete removes a device.
func (m *Manager) Delete(ctx context.Context, actor string, id model.DeviceID) error {
	return m.DeleteWith(ctx, actor, id, DeleteOptions{})
}

// DeleteWith removes a device and records what happened to its backups.
func (m *Manager) DeleteWith(ctx context.Context, actor string, id model.DeviceID, opt DeleteOptions) error {
	m.mu.Lock()
	old, err := m.st.GetDevice(ctx, id)
	if err == nil {
		err = m.st.DeleteDevice(ctx, id)
	}
	m.mu.Unlock()
	detail := map[string]any{"name": old.Name, "address": old.Address, "mac": old.MAC}
	if opt.Backups != "" {
		detail["backups"] = opt.Backups
	}
	m.record(ctx, actor, "device.delete", id, detail, err)
	if err != nil {
		return err
	}
	m.publish(model.Device{ID: id})
	return nil
}

// Upsert matches by MAC, then topic, then address (with port); used by
// importers and discovery. A match is merged, not replaced:
//
//   - address and port follow the incoming device (DHCP drift is audited as
//     device.address_changed), subject to the identity rules below;
//   - name and notes are kept when already set (a re-import must not undo a
//     rename); topic, full topic and MQTT capability follow the incoming
//     value when it is non-empty; credentials are replaced when the incoming
//     device carries a user or password;
//   - tags are united; OTAExcluded only turns on, never off;
//   - the identity is replaced when the incoming one is complete (Known).
//
// Upsert has no way to tell an admin import from MQTT discovery, so every
// incoming device is untrusted, including the MAC a device reports about
// itself (any host can claim any MAC). Identity rules:
//
//   - a match by topic or address whose MAC differs from the incoming MAC is
//     refused with ErrIdentityMismatch;
//   - an address or port change is applied only if the record is bound to a
//     MAC, neither the record nor the incoming device carries credentials,
//     the new address is an IP literal (a DNS name could be rebound after the
//     check) and the Verifier reports exactly that MAC there. A credentialed
//     device is never moved here: a genuine password-protected Tasmota cannot
//     answer the unauthenticated probe, so a successful probe would prove only
//     an impostor. Such moves are an admin action through Update;
//   - a record without MAC is bound to the incoming MAC only if the Verifier
//     reports that MAC at the address the record already points to;
//   - a different MAC gives ErrIdentityMismatch; everything unproven gives
//     ErrNeedsVerification; a record changed during verification gives
//     ErrConflict. In every refusal the stored record, and with it the stored
//     credentials, stay untouched, and no credential is sent anywhere.
func (m *Manager) Upsert(ctx context.Context, actor string, d model.Device) (model.DeviceID, bool, error) {
	return m.upsert(ctx, actor, d, 0)
}

// upsert is Upsert; with only != 0 it never creates and refuses a match that
// is not that device (ErrConflict).
func (m *Manager) upsert(ctx context.Context, actor string, d model.Device, only model.DeviceID) (model.DeviceID, bool, error) {
	in := d
	if err := m.validate(&in); err != nil {
		m.record(ctx, actor, "device.upsert", 0, createDetail(d), err)
		return 0, false, err
	}
	m.mu.Lock()
	existing, found, err := m.find(ctx, in)
	if err != nil {
		m.mu.Unlock()
		m.record(ctx, actor, "device.upsert", 0, createDetail(in), err)
		return 0, false, err
	}
	if only != 0 && (!found || existing.ID != only) {
		m.mu.Unlock()
		err := fmt.Errorf("%w: device %d is gone or no longer matches", ErrConflict, only)
		m.record(ctx, actor, "device.upsert", only, withKey(createDetail(in), "refused", "gone"), err)
		return only, false, err
	}
	if !found {
		id, err := m.create(ctx, in)
		m.mu.Unlock()
		if err == nil {
			// CreateDevice persists the identity itself (see store.SQLite), so no extra write.
			in.ID = id
			m.publish(in)
		}
		m.record(ctx, actor, "device.create", id, withKey(createDetail(in), "via", "upsert"), err)
		return id, true, err
	}
	m.mu.Unlock()

	// A stored hostname is never replaced by an IP address automatically
	// (spec 11.1): the address is the admin's choice, the MAC identifies the device.
	if !isIP(existing.Address) && isIP(in.Address) {
		in.Address, in.Port = existing.Address, existing.Port
	}

	// Probing can take seconds, so it runs without the lock; the record is
	// compared again below before anything is written.
	if err := m.checkIdentity(ctx, existing, in); err != nil {
		m.recordRefused(ctx, actor, existing.ID, withKey(createDetail(in), "refused", "identity"), err)
		return existing.ID, false, err
	}

	merged := merge(existing, in)
	if sameRecord(merged, existing) && (!in.Identity.Known() || sameIdentity(in.Identity, existing.Identity)) {
		return existing.ID, false, nil // nothing to change: no write, no audit row
	}
	m.mu.Lock()
	cur, err := m.st.GetDevice(ctx, existing.ID)
	if err == nil && !sameRecord(cur, existing) {
		err = fmt.Errorf("%w: device %d", ErrConflict, existing.ID)
	}
	if err == nil {
		err = m.st.UpdateDevice(ctx, merged)
	}
	if err == nil && in.Identity.Known() {
		err = m.st.UpdateIdentity(ctx, merged.ID, in.Identity)
	}
	m.mu.Unlock()
	detail := updateDetail(existing, merged)
	action := "device.update"
	if existing.Address != merged.Address || existing.Port != merged.Port {
		action = "device.address_changed"
	}
	m.record(ctx, actor, action, merged.ID, withKey(detail, "via", "upsert"), err)
	if err != nil {
		return merged.ID, false, err
	}
	m.publish(merged)
	return merged.ID, false, nil
}

// checkIdentity applies the identity rules of Upsert to a matched record.
func (m *Manager) checkIdentity(ctx context.Context, existing, in model.Device) error {
	if in.MAC != "" && existing.MAC != "" && !existing.HasMAC(in.MAC) {
		return fmt.Errorf("%w: device %d has %s, incoming has %s", ErrIdentityMismatch, existing.ID, existing.MAC, in.MAC)
	}
	if existing.Address != in.Address || portOf(existing) != portOf(in) {
		return m.verifyMove(ctx, existing, in)
	}
	if existing.MAC == "" && in.MAC != "" {
		// Binding the record to a claimed MAC would let the claimant pass
		// every later check, so the claim must come from the host the record
		// already talks to.
		return m.verifyAt(ctx, existing.Address, portOf(existing), in.MAC)
	}
	return nil
}

func (m *Manager) verifyMove(ctx context.Context, existing, in model.Device) error {
	switch {
	case existing.MAC == "":
		return fmt.Errorf("%w: device %d is not bound to a MAC", ErrNeedsVerification, existing.ID)
	case hasCredentials(existing) || hasCredentials(in):
		return fmt.Errorf("%w: device %d has credentials, its address can only be changed by an admin", ErrNeedsVerification, existing.ID)
	}
	if _, err := netip.ParseAddr(in.Address); err != nil {
		return fmt.Errorf("%w: %s is a DNS name, an automatic move needs an IP address", ErrNeedsVerification, in.Address)
	}
	return m.verifyAt(ctx, in.Address, portOf(in), existing.MAC, existing.MACs...)
}

// checkCredentialedMove covers every edit that moves a device with a stored
// password (kept, not retyped) to another address or port. The probe goes out
// WITHOUT credentials, so nothing is sent to the new host. Another MAC stops
// the move. A matching MAC does NOT count as proof: a device that has a web
// password refuses a credential-less request, so an answer comes from a host
// that does not enforce it (an impostor, or a device whose password is gone).
// Either way the admin must confirm explicitly, for exactly this target, that
// the stored password may be sent there; the audit entry says "mac_unverified".
func (m *Manager) checkCredentialedMove(ctx context.Context, old, next model.Device) (unverified bool, err error) {
	if old.HTTPPassword == "" || next.HTTPPassword != old.HTTPPassword || (next.Address == old.Address && portOf(next) == portOf(old)) {
		return false, nil
	}
	if old.MAC != "" {
		if verr := m.verifyAt(ctx, next.Address, portOf(next), old.MAC, old.MACs...); verr != nil && !errors.Is(verr, ErrNeedsVerification) {
			return false, verr
		}
	}
	if !UnverifiedMoveConfirmed(ctx, MoveTarget(next.Address, next.Port)) {
		return false, &UnverifiedMoveError{Target: MoveTarget(next.Address, next.Port)}
	}
	return true, nil
}

// verifyAt asks the Verifier which MAC answers at address:port and compares it with want.
// Any MAC of the set is accepted (an ESP32 may answer with its Ethernet MAC).
func (m *Manager) verifyAt(ctx context.Context, address string, port int, want string, alsoWant ...string) error {
	if m.ver == nil {
		return fmt.Errorf("%w: no verifier configured", ErrNeedsVerification)
	}
	got, err := m.ver.VerifyMAC(ctx, address, port)
	if err != nil {
		return fmt.Errorf("%w: probe of %s failed: %w", ErrNeedsVerification, address, err)
	}
	if got, err = NormaliseMAC(got); err != nil || got == "" {
		return fmt.Errorf("%w: probe of %s returned no usable MAC", ErrNeedsVerification, address)
	}
	if got != want && !slices.Contains(alsoWant, got) {
		return fmt.Errorf("%w: %s hosts %s, expected %s", ErrIdentityMismatch, address, got, want)
	}
	return nil
}

// hasCredentials: only a password counts. A web user without a password means no auth.
func hasCredentials(d model.Device) bool { return d.HasCredentials() }

// sameRecord reports whether the inventory-owned fields of a and b are equal.
// Timestamps, the identity and what the transport code learns (firmware reach, the MQTT password
// refusal mark) are written by other paths and are ignored: a learning write between the read and
// the write of an update must not turn it into a conflict.
func sameRecord(a, b model.Device) bool {
	a.CreatedAt, a.UpdatedAt, a.Identity = time.Time{}, time.Time{}, model.Identity{}
	b.CreatedAt, b.UpdatedAt, b.Identity = time.Time{}, time.Time{}, model.Identity{}
	a.FWReach, a.MQTTPasswordBadAt = model.FWReach{}, time.Time{}
	b.FWReach, b.MQTTPasswordBadAt = model.FWReach{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

func (m *Manager) find(ctx context.Context, in model.Device) (model.Device, bool, error) {
	if in.MAC != "" {
		d, err := m.st.GetDeviceByMAC(ctx, in.MAC)
		if err == nil {
			return d, true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return model.Device{}, false, err
		}
	}
	if in.Topic != "" {
		d, err := m.st.GetDeviceByTopic(ctx, in.Topic)
		if err == nil {
			return d, true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return model.Device{}, false, err
		}
	}
	all, err := m.st.ListDevices(ctx)
	if err != nil {
		return model.Device{}, false, err
	}
	for _, d := range all {
		if strings.EqualFold(d.Address, in.Address) && portOf(d) == portOf(in) {
			return d, true, nil
		}
	}
	return model.Device{}, false, nil
}

func portOf(d model.Device) int {
	if d.Port == 0 {
		return 80
	}
	return d.Port
}

func merge(old, in model.Device) model.Device {
	out := old
	out.Address, out.Port = in.Address, in.Port
	if out.MAC == "" {
		out.MAC = in.MAC
	}
	if out.Name == "" {
		out.Name = in.Name
	}
	if out.Notes == "" {
		out.Notes = in.Notes
	}
	if in.Topic != "" {
		out.Topic = in.Topic
	}
	if in.FullTopic != "" {
		out.FullTopic = in.FullTopic
	}
	if in.MQTT != model.MQTTUnknown {
		out.MQTT = in.MQTT
	}
	if in.HasCredentials() {
		out.HTTPUser, out.HTTPPassword = in.HTTPUser, in.HTTPPassword
	}
	out.OTAExcluded = old.OTAExcluded || in.OTAExcluded
	out.Tags, _ = normaliseTags(append(append([]string{}, old.Tags...), in.Tags...))
	if in.Identity.Known() {
		out.Identity = in.Identity
	}
	return out
}

// recordRefused audits a refused upsert, at most once per device and UTC day.
func (m *Manager) recordRefused(ctx context.Context, actor string, id model.DeviceID, detail map[string]any, err error) {
	day := m.clk.Now().UTC().Format(time.DateOnly)
	m.rmu.Lock()
	seen := m.refused[id] == day
	m.refused[id] = day
	m.rmu.Unlock()
	if !seen {
		m.record(ctx, actor, "device.upsert", id, detail, err)
	}
}

func isIP(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

// sameIdentity compares two identities ignoring the detection time.
func sameIdentity(a, b model.Identity) bool {
	a.DetectedAt, b.DetectedAt = time.Time{}, time.Time{}
	return a == b
}

func (m *Manager) record(ctx context.Context, actor, action string, id model.DeviceID, detail map[string]any, err error) {
	e := model.AuditEvent{Actor: actor, Action: action, Result: "ok"}
	if id != 0 {
		e.Target = fmt.Sprintf("device:%d", id)
	}
	if b, merr := json.Marshal(detail); merr == nil {
		e.Detail = string(b)
	}
	if err != nil {
		e.Result = err.Error()
	}
	m.audit.Record(ctx, e)
}

// publish sends the device to subscribers with the password removed.
func (m *Manager) publish(d model.Device) {
	if m.pub == nil {
		return
	}
	d.HTTPPassword, d.MQTTPassword = "", ""
	m.pub.Publish(events.Event{Topic: events.TopicDevice, At: m.clk.Now().UTC(), Payload: d})
}

func createDetail(d model.Device) map[string]any {
	return map[string]any{
		"name": d.Name, "address": d.Address, "port": d.Port, "mac": d.MAC,
		"topic": d.Topic, "ota_excluded": d.OTAExcluded,
	}
}

func withKey(m map[string]any, k string, v any) map[string]any {
	m[k] = v
	return m
}

// updateDetail lists which fields changed, never their secret values.
func updateDetail(old, nu model.Device) map[string]any {
	var changed []string
	add := func(name string, differs bool) {
		if differs {
			changed = append(changed, name)
		}
	}
	add("name", old.Name != nu.Name)
	add("address", old.Address != nu.Address)
	add("port", portOf(old) != portOf(nu))
	add("mac", old.MAC != nu.MAC)
	add("topic", old.Topic != nu.Topic)
	add("full_topic", old.FullTopic != nu.FullTopic)
	add("tags", strings.Join(old.Tags, "\x00") != strings.Join(nu.Tags, "\x00"))
	add("notes", old.Notes != nu.Notes)
	add("credentials", old.HTTPUser != nu.HTTPUser || old.HTTPPassword != nu.HTTPPassword)
	add("ota_excluded", old.OTAExcluded != nu.OTAExcluded)
	add("mqtt", old.MQTT != nu.MQTT)
	sort.Strings(changed)
	detail := map[string]any{"changed": changed}
	if old.Address != nu.Address || portOf(old) != portOf(nu) {
		detail["address_from"] = netaddr.Display(old.Address, portOf(old), "http")
		detail["address_to"] = netaddr.Display(nu.Address, portOf(nu), "http")
	}
	return detail
}
