package simulator

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// Why: the v0.13.0 tag run hung for 10 minutes in a test cleanup inside
// Broker.Close. mochi v2.7.9 closes a listener's clients through
// Clients.GetByListener, which takes the clients' RLock and then RLocks again
// in Len. A client that disconnects at the same moment queues Delete (Lock)
// between the two, and the recursive RLock waits for it forever. Clients
// disconnecting while the broker closes is every test's cleanup, so Close
// must not take that path.
func TestBrokerCloseWhileClientsDisconnect(t *testing.T) {
	for round := 0; round < 30; round++ {
		b, err := StartBroker()
		if err != nil {
			t.Fatal(err)
		}
		var clients []paho.Client
		for i := 0; i < 20; i++ {
			c := paho.NewClient(paho.NewClientOptions().AddBroker(b.URL()).SetClientID(fmt.Sprintf("c%d-%d", round, i)).
				SetCleanSession(true).SetAutoReconnect(false))
			if tok := c.Connect(); !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
				t.Fatalf("connect: %v", tok.Error())
			}
			clients = append(clients, c)
		}
		var wg sync.WaitGroup
		for _, c := range clients {
			wg.Go(func() { c.Disconnect(0) })
		}
		// Raw connections that drop at once keep Delete calls coming while Close runs.
		for i := 0; i < 20; i++ {
			wg.Go(func() {
				if conn, err := net.Dial("tcp", b.addr); err == nil {
					_ = conn.Close() // the point is the abrupt close
				}
			})
		}
		// What a disconnecting client does to the clients map (attachClient ->
		// Clients.Delete), as often as the window needs to be hit.
		stop := make(chan struct{})
		for i := 0; i < 4; i++ {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
						b.srv.Clients.Delete("ghost")
					}
				}
			})
		}
		done := make(chan struct{})
		go func() { _ = b.Close(); close(done) }() // Close returns no error worth checking here
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("round %d: Broker.Close deadlocked", round)
		}
		close(stop)
		wg.Wait()
	}
}

// Why: the closer Broker.Close uses must survive a disconnect storm. With a
// recursive RLock (mochi's GetByListener) this loop deadlocks within seconds
// (checked against GetByListener when the fix was written); a closer that
// takes the lock once cannot.
func TestCloseClientsOfNeverLocksRecursively(t *testing.T) {
	b, err := StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					b.srv.Clients.Delete("ghost")
				}
			}
		})
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2_000_000; i++ {
			closeClientsOf(b.srv, "sim")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		close(stop)
		t.Fatal("closeClientsOf deadlocked against concurrent Delete") // the broker is left open: closing it would hang too
	}
	close(stop)
	wg.Wait()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}
