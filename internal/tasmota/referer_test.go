package tasmota

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// Why: Tasmota's default (SetOption128 0) refuses every request without a
// Referer naming the device (issues #45, #63). The client must send one, so
// devices work without anybody changing their settings.
func TestClientSendsARefererThatSatisfiesSetOption128Zero(t *testing.T) {
	c, tg := uploadSim(t, simulator.DeviceSpec{RefererCheck: true})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := newClient(Options{}).Command(ctx, tg, "Status 2"); err != nil {
		t.Fatalf("Command on a device with the referer check: %v", err)
	}
	if err := upload(uploader(t, Options{}), tg, simImage(t, "tasmota32.bin", "16.1.0")); err != nil {
		t.Fatalf("UploadFirmware on a device with the referer check: %v", err)
	}
	waitBoots(t, c, 1)
	if c.Version() != "16.1.0" {
		t.Errorf("version %s: the upload did not flash", c.Version())
	}
}

// refusedTarget reaches the simulated device by a name that is neither its
// hostname nor its IP ("localhost" vs. "plug" / 127.0.0.1), as through NAT or
// a DNS alias. The Referer then cannot match and the device stays silent.
func refusedTarget(t *testing.T, password string) Target {
	t.Helper()
	_, tg := uploadSim(t, simulator.DeviceSpec{RefererCheck: true, Password: password})
	return TargetFor(model.Device{ID: 7, Address: "localhost", Port: tg.port, HTTPPassword: password})
}

// Why: a refusal looks like a dead device (the connection closes without a
// reply), which sent users hunting for network problems. The error must name
// the cause and the fix, yet stay an ErrUnreachable for every caller that
// handles unreachable devices.
func TestClientNamesTheRefererRefusal(t *testing.T) {
	tg := refusedTarget(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := newClient(Options{}).Command(ctx, tg, "Status 2")
	if !errors.Is(err, ErrHTTPAPIRefused) || !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Command: want ErrHTTPAPIRefused wrapping ErrUnreachable, got %v", err)
	}
	if !strings.Contains(err.Error(), "SetOption128 1") {
		t.Errorf("the message must tell the fix: %v", err)
	}
}

// Why: an EOF alone is also what a dropped link or a crashing device looks
// like; blaming SetOption128 then would send people to the wrong place. The
// SO128 hint needs proof that the device is up (its main page answers).
func TestDroppedCommandWithDeadMainPageIsNotReportedAsRefererRefusal(t *testing.T) {
	// A listener that accepts and closes at once: /cm and / both end in EOF.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_ = conn.Close() // deliberate: the peer answers nothing
		}
	}()
	_, ps, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(ps)
	tg := TargetFor(model.Device{ID: 8, Address: "127.0.0.1", Port: port})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = newClient(Options{}).Command(ctx, tg, "Status 2")
	if err == nil || errors.Is(err, ErrHTTPAPIRefused) || !errors.Is(err, ErrUnreachable) {
		t.Errorf("want plain ErrUnreachable, got %v", err)
	}
}

// Why: OtaUrl/Upgrade lose replies legitimately and the OTA engine probes after
// them (safety rule 3); the client must not add its own follow-up request or
// reinterpret that error.
func TestUpgradeNoReplyKeepsThePlainError(t *testing.T) {
	tg := refusedTarget(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := newClient(Options{}).Command(ctx, tg, "Upgrade 1")
	if err == nil || errors.Is(err, ErrHTTPAPIRefused) || !errors.Is(err, ErrUnreachable) {
		t.Errorf("want plain ErrUnreachable, got %v", err)
	}
	err = upload(uploader(t, Options{}), tg, simImage(t, "tasmota32.bin", "16.1.0"))
	if err == nil || errors.Is(err, ErrHTTPAPIRefused) || !errors.Is(err, ErrUnreachable) {
		t.Errorf("upload: want plain ErrUnreachable, got %v", err)
	}
}

// Why: only a refused GET is certain to have executed nothing. A cut or lost
// reply of the upload POST may mean the device flashes; it must not be
// reported as "refused, set SO128", or the caller would treat it as safe.
func TestLostUploadReplyIsNotReportedAsRefererRefusal(t *testing.T) {
	_, tg := uploadSim(t, simulator.DeviceSpec{Faults: []simulator.Fault{{Kind: simulator.FaultUploadCut}}})
	err := upload(uploader(t, Options{}), tg, simImage(t, "tasmota32.bin", "16.1.0"))
	if err == nil || errors.Is(err, ErrHTTPAPIRefused) {
		t.Errorf("want a non-refusal error, got %v", err)
	}
}
