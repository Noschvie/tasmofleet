package devlock

import (
	"sync"
	"sync/atomic"
	"testing"
)

// Why: two writers on one device must never both proceed, however many race.
func TestOnlyOneHolderPerDevice(t *testing.T) {
	l := New()
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, ok := l.TryAcquire(1, "x"); ok {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d holders", won.Load())
	}
}

// Why: other devices stay free, the refused caller learns who holds the lock,
// and a double release must not free a lock somebody else has taken since.
func TestIndependentDevicesHolderNameAndIdempotentRelease(t *testing.T) {
	l := New()
	rel, _, _ := l.TryAcquire(1, "restore")
	if _, _, ok := l.TryAcquire(2, "ota"); !ok {
		t.Error("device 2 must be free")
	}
	if _, holder, ok := l.TryAcquire(1, "ota"); ok || holder != "restore" {
		t.Errorf("ok=%v holder=%q", ok, holder)
	}
	rel()
	rel2, _, ok := l.TryAcquire(1, "settings")
	if !ok {
		t.Fatal("released lock not free")
	}
	rel() // a stale second release must not free the new holder
	if _, _, ok := l.TryAcquire(1, "ota"); ok {
		t.Error("stale release freed the lock")
	}
	rel2()
}
