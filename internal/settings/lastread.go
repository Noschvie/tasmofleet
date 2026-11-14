package settings

import (
	"context"
	"maps"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/transport"
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

// flight is one device read that callers share. gen is the device's write count when it started.
type flight struct {
	done chan struct{}
	vals map[string]string
	err  error
	gen  uint64
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

// Forget drops what was remembered of a device: after a write the old values are wrong, and showing
// them as "last known" would be worse than showing nothing. A read under way started before the write:
// counting the write keeps its values out of last and its later readers off it (readShared). Apply
// calls it; so must every other writer of device settings (the console, Make compliant), else a read
// that started before their write is remembered as current.
func (m *Manager) Forget(id model.DeviceID) {
	m.mu.Lock()
	delete(m.last, id)
	m.gen[id]++
	m.mu.Unlock()
}

// readShared runs one Read per device at a time. A caller that arrives while a read is under way
// waits for it and gets its result, so a second open of the page sends nothing to the device (the
// client already serialises requests per device; this keeps a whole second round of ~30 queries from
// queueing behind the first). The read runs detached from the first caller's request: the caller
// closing the page must not fail the readers that joined, and the values are worth keeping. A read that
// started before a write (Apply) holds values from before it: nobody joins it after the write, and its
// values are not remembered. The next reader starts a new read, sent once the old one has ended.
func (m *Manager) readShared(ctx context.Context, id model.DeviceID, keys []string) (map[string]string, error) {
	m.mu.Lock()
	f, running := m.flights[id]
	if !running || f.gen != m.gen[id] {
		prev := f // nil, or a read from before a write: the device answers one read at a time
		f = &flight{done: make(chan struct{}), gen: m.gen[id]}
		m.flights[id] = f
		go func() {
			vals, err := m.detachedRead(ctx, id, keys, prev)
			m.mu.Lock()
			f.vals, f.err = vals, err
			if err == nil && f.gen == m.gen[id] {
				m.last[id] = Snapshot{Values: maps.Clone(vals), At: m.o.Now()}
			}
			if m.flights[id] == f {
				delete(m.flights, id)
			}
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

// detachedRead is the read of a shared flight: it outlives the request that started it (the callers
// that joined still want the values), but not TasmoFleet's stop signal. It waits for prev, the read
// of the device from before a write (the device answers one read at a time), then reads, and then asks
// whether the device that answered is the stored one: values of another device are never returned.
func (m *Manager) detachedRead(ctx context.Context, id model.DeviceID, keys []string, prev *flight) (map[string]string, error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	defer context.AfterFunc(m.o.Stop, cancel)()
	if prev != nil {
		select {
		case <-prev.done: // bounded: every read ends within ReadTimeout
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	rctx, read := transport.WithTrace(ctx) // the check vouches for the transports these reads used (#810)
	vals, err := m.readKeys(rctx, id, keys)
	if err != nil || m.o.Verify == nil {
		return vals, err
	}
	d, err := m.o.Devices.GetDevice(ctx, id)
	if err == nil {
		vctx, vcancel := context.WithTimeout(ctx, m.o.ReadTimeout)
		err = m.o.Verify(vctx, d, read)
		vcancel()
	}
	if err != nil {
		return nil, err
	}
	return vals, nil
}
