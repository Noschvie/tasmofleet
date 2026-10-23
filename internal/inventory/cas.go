package inventory

import (
	"context"
	"fmt"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// UpsertExisting is Upsert for a device the caller already matched: it never
// creates. If the device with this id is gone, or the claim now matches
// another record, nothing is written and ErrConflict is returned. Discovery
// uses it so a device an admin deleted meanwhile is not resurrected.
func (m *Manager) UpsertExisting(ctx context.Context, actor string, id model.DeviceID, d model.Device) (model.DeviceID, bool, error) {
	return m.upsert(ctx, actor, d, id)
}

// UpdateIf is Update as a compare-and-set: the write happens only if the
// stored record still equals expected (same comparison as Upsert's re-check:
// all inventory-owned fields, ignoring timestamps and identity); otherwise
// nothing is written and ErrConflict is returned.
func (m *Manager) UpdateIf(ctx context.Context, actor string, expected, next model.Device) error {
	// Same rule as Update: a credentialed device moves only on the admin's
	// explicit, target-bound confirmation (probe and check before the lock).
	unverified, cerr := m.checkCredentialedMove(ctx, expected, next)
	if cerr != nil {
		m.record(ctx, actor, "device.update", next.ID, map[string]any{}, cerr)
		return cerr
	}
	m.mu.Lock()
	old, err := m.st.GetDevice(ctx, next.ID)
	if err == nil && !sameRecord(old, expected) {
		err = fmt.Errorf("%w: device %d", ErrConflict, next.ID)
	}
	if err == nil {
		err = m.validate(&next)
	}
	if err == nil {
		err = m.st.UpdateDevice(ctx, next)
	}
	m.mu.Unlock()
	detail := map[string]any{}
	if err == nil {
		detail = updateDetail(old, next)
		if unverified {
			detail["mac_unverified"] = true
		}
	}
	m.record(ctx, actor, "device.update", next.ID, detail, err)
	if err != nil {
		return err
	}
	if got, gerr := m.st.GetDevice(ctx, next.ID); gerr == nil {
		next = got
	}
	m.publish(next)
	return nil
}
