package settings

import (
	"context"
	"maps"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// StaleAfter is how old a remembered reading may be before the pages mark it as stale instead of
// letting it look current (#599). A constant on purpose: it is not a setting.
const StaleAfter = time.Hour

// Snapshot is the last complete Read of a device: the values and when they were read. It is kept in
// memory only; after a restart the first open reads the device again.
type Snapshot struct {
	Values map[string]string
	At     time.Time
}

// Stale reports whether the reading is older than StaleAfter at now.
func (s Snapshot) Stale(now time.Time) bool { return now.Sub(s.At) > StaleAfter }

// flight is one device read that callers share.
type flight struct {
	done chan struct{}
	vals map[string]string
	err  error
}

// Last returns the values of the newest successful Read of the device, without contacting it.
func (m *Manager) Last(id model.DeviceID) (Snapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.last[id]
	if !ok {
		return Snapshot{}, false
	}
	return Snapshot{Values: maps.Clone(s.Values), At: s.At}, true
}

// forget drops what was remembered of a device: after a write the old values are wrong, and showing
// them as "last known" would be worse than showing nothing.
func (m *Manager) forget(id model.DeviceID) {
	m.mu.Lock()
	delete(m.last, id)
	m.mu.Unlock()
}

// readShared runs one Read per device at a time. A caller that arrives while a read is under way
// waits for it and gets its result, so a second open of the page sends nothing to the device (the
// client already serialises requests per device; this keeps a whole second round of ~30 queries from
// queueing behind the first). The read runs detached from the first caller's request: the caller
// closing the page must not fail the readers that joined, and the values are worth keeping.
func (m *Manager) readShared(ctx context.Context, id model.DeviceID, keys []string) (map[string]string, error) {
	m.mu.Lock()
	f, running := m.flights[id]
	if !running {
		f = &flight{done: make(chan struct{})}
		m.flights[id] = f
		go func() {
			vals, err := m.readKeys(context.WithoutCancel(ctx), id, keys)
			m.mu.Lock()
			f.vals, f.err = vals, err
			if err == nil {
				m.last[id] = Snapshot{Values: maps.Clone(vals), At: m.o.Now()}
			}
			delete(m.flights, id)
			m.mu.Unlock()
			close(f.done)
		}()
	}
	m.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	return maps.Clone(f.vals), nil
}
