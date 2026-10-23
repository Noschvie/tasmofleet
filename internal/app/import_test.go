package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

func importServices(t *testing.T) *Services {
	t.Helper()
	cfg, err := config.Load([]string{
		"TASMOFLEET_DATA_DIR=" + t.TempDir(), "TASMOFLEET_CONFIG_FILE=/dev/null", "TASMOFLEET_AUTH_MODE=local",
		"TASMOFLEET_DEVICE_NETWORKS=127.0.0.0/8", "TASMOFLEET_ALLOW_INSECURE_HTTP=true",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := openServices(cfg, slog.New(slog.DiscardHandler), "test", true) // the env networks stand in for a server start
	if err != nil {
		t.Fatal(err)
	}
	svc.Prober.Gap = 1
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// Why: the import is the migration path from TasmoAdmin. Rows must end up in
// the inventory with the MAC and identity the device reports, a second run
// must merge instead of duplicating, and an unreachable device must still be
// imported (without identity) rather than silently dropped.
func TestImportTasmoAdminProbesMergesAndKeepsUnreachable(t *testing.T) {
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	dev, err := fleet.Add(t.Context(), simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M})
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, dev.Addr())
	dir := t.TempDir()
	csv := fmt.Sprintf("1,\"plug\",%s,,,bulb_1,1,1,0,0,1,%d,\"plug\",0,plug,0\n2,\"ghost\",127.0.0.1,,,bulb_1,1,1,0,0,1,1,\"ghost\",0,ghost,0\n", host, port)
	if err := os.WriteFile(filepath.Join(dir, "devices.csv"), []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := importServices(t)
	ctx := context.Background()

	var out bytes.Buffer
	res, err := ImportTasmoAdmin(ctx, svc, ImportOptions{Dir: dir, DryRun: true}, &out)
	if err != nil || res.Created != 0 {
		t.Fatalf("dry run wrote something: %+v %v", res, err)
	}
	if devs, _ := svc.Inventory.List(ctx); len(devs) != 0 {
		t.Fatal("dry run must not write")
	}

	out.Reset()
	res, err = ImportTasmoAdmin(ctx, svc, ImportOptions{Dir: dir}, &out)
	if err != nil || res.Created != 2 || res.Unprobed != 1 {
		t.Fatalf("import: %+v %v\n%s", res, err, out.String())
	}
	devs, _ := svc.Inventory.List(ctx)
	byName := map[string]bool{}
	for _, d := range devs {
		byName[d.Name] = d.MAC != "" && d.Identity.Known()
	}
	if !byName["plug"] || byName["ghost"] {
		t.Fatalf("probe results wrong: %v", byName)
	}

	out.Reset()
	res, err = ImportTasmoAdmin(ctx, svc, ImportOptions{Dir: dir}, &out)
	if err != nil || res.Created != 0 || res.Updated != 2 {
		t.Fatalf("second run must merge: %+v %v\n%s", res, err, out.String())
	}
	if strings.Contains(out.String(), "password") {
		t.Fatal("output must not mention passwords")
	}
}

// Why (review F9): the importer prints names from the CSV and from probed
// devices; an escape sequence in a name must not reach the admin's terminal.
func TestImportOutputHasNoControlCharacters(t *testing.T) {
	dir := t.TempDir()
	csv := "1,\"evil\\x1b]0;pwned\\x07\",172.99.99.99,,,bulb_1,1,1,0,0,1,80,\"evil\\x1b[31m\",0,x,0\n"
	if err := os.WriteFile(filepath.Join(dir, "devices.csv"), []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := importServices(t)
	var out bytes.Buffer
	_, _ = ImportTasmoAdmin(context.Background(), svc, ImportOptions{Dir: dir, NoProbe: true}, &out)
	for _, r := range out.String() {
		if r != '\n' && r < 0x20 || r == 0x7f {
			t.Fatalf("control character %q in output %q", r, out.String())
		}
	}
	if out.Len() == 0 {
		t.Fatal("expected output")
	}
}

// Why: the CLI and the web upload must show the admin the same plan for the
// same files; both go through Services.Importer, and the CLI dry run prints it.
func TestCLIDryRunPrintsThePlanTheWebWouldShow(t *testing.T) {
	dir := t.TempDir()
	csv := "1,\"a\",127.0.0.1,,,bulb_1,1,1,0,0,1,8081,\"a\",0,a,0\n2,\"b\",127.0.0.1,,,bulb_1,1,1,0,0,1,8081,\"b\",0,b,0\n"
	if err := os.WriteFile(filepath.Join(dir, "devices.csv"), []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := importServices(t)
	f, _ := os.Open(filepath.Join(dir, "devices.csv"))
	defer func() { _ = f.Close() }() // read-only
	plan, err := svc.Importer().Plan(context.Background(), f, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := ImportTasmoAdmin(context.Background(), svc, ImportOptions{Dir: dir, DryRun: true}, &out); err != nil {
		t.Fatal(err)
	}
	for _, r := range plan.Rows {
		want := fmt.Sprintf("would %s: %s (%s)", r.Action, r.Device.Name, r.Device.Address)
		if !strings.Contains(out.String(), want) {
			t.Fatalf("CLI output lacks %q:\n%s", want, out.String())
		}
	}
	if len(plan.Rows) != 2 || plan.Rows[1].Action != "skip" {
		t.Fatalf("rows: %+v", plan.Rows)
	}
}
