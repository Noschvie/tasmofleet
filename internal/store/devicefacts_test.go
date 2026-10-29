package store

import (
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: the Overview shows subnet, DNS, OTA URL and relay names from the device record, so a
// probe result must persist them and a later read must give back exactly what was written.
func TestUpdateProbeStoresDeviceFactsAndKeepsThemWhenNotRead(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "plug", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	if err != nil {
		t.Fatal(err)
	}
	guard := ProbeGuard{Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"}
	net := model.NetFacts{IP: "192.0.2.10", Subnet: "255.255.255.0", DNS: "192.0.2.1, 9.9.9.9"}
	res := ProbeResult{SetNet: true, Net: net, SetOTAURL: true, OTAURL: "http://fw.example/tasmota.bin.gz", RelayNames: []string{"Pump", "Light"}}
	if err := s.UpdateProbe(ctx, id, guard, res); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if d.Net.Subnet != net.Subnet || d.Net.DNS != net.DNS || d.OTAURL != res.OTAURL || !reflect.DeepEqual(d.RelayNames, res.RelayNames) {
		t.Fatalf("round trip: net %+v ota %q relays %q", d.Net, d.OTAURL, d.RelayNames)
	}
	// A probe that read none of them (minimal firmware, failed Status 1) must not blank them.
	if err := s.UpdateProbe(ctx, id, guard, ProbeResult{}); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, id)
	if d.OTAURL != res.OTAURL || !reflect.DeepEqual(d.RelayNames, res.RelayNames) || d.Net.DNS != net.DNS {
		t.Fatalf("a probe without facts wiped them: ota %q relays %q net %+v", d.OTAURL, d.RelayNames, d.Net)
	}
	// An admin edit with a stale copy must not wipe them either: they belong to the probe.
	d.Notes, d.OTAURL, d.RelayNames = "edited", "", nil
	if err := s.UpdateDevice(ctx, d); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, id)
	if d.Notes != "edited" || d.OTAURL != res.OTAURL || len(d.RelayNames) != 2 {
		t.Fatalf("edit lost the facts: %+v", d)
	}
	// A device reporting an empty OtaUrl is recorded as empty, not kept stale.
	if err := s.UpdateProbe(ctx, id, guard, ProbeResult{SetOTAURL: true}); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.GetDevice(ctx, id); d.OTAURL != "" {
		t.Fatalf("OTAURL = %q, want empty after the device reported none", d.OTAURL)
	}
}

// Why: the probe guard protects admin edits (address, MAC): a probe that answers for a device
// that was moved meanwhile came from another device and must write nothing.
func TestUpdateProbeDeviceFactsRespectTheGuard(t *testing.T) {
	s, _, _ := newStore(t)
	id, _ := s.CreateDevice(ctx, model.Device{Name: "plug", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	moved := ProbeGuard{Address: "192.0.2.99", MAC: "aa:bb:cc:00:00:01"}
	err := s.UpdateProbe(ctx, id, moved, ProbeResult{SetOTAURL: true, OTAURL: "http://x/y", RelayNames: []string{"A"}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if d, _ := s.GetDevice(ctx, id); d.OTAURL != "" || d.RelayNames != nil {
		t.Fatalf("a guarded probe wrote: %q %q", d.OTAURL, d.RelayNames)
	}
}

// Why: the background network read writes subnet and DNS through UpdateNet, not UpdateProbe.
func TestUpdateNetStoresSubnetAndDNS(t *testing.T) {
	s, _, _ := newStore(t)
	id, _ := s.CreateDevice(ctx, model.Device{Name: "plug", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"})
	n := model.NetFacts{IP: "192.0.2.10", Subnet: "255.255.255.0", DNS: "192.0.2.1"}
	if err := s.UpdateNet(ctx, id, ProbeGuard{Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"}, n); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); d.Net.Subnet != n.Subnet || d.Net.DNS != n.DNS {
		t.Fatalf("Net = %+v", d.Net)
	}
}

// Why: live databases upgrade from 0.16.2 (migration 0030) with devices in them; the new
// columns must default to "not read yet" so those devices load without error and show nothing
// until the next refresh.
func TestMigration0031KeepsExistingDevicesAndAddsEmptyFacts(t *testing.T) {
	db, path := rawDB(t)
	upTo, all := fstest.MapFS{}, fstest.MapFS{}
	err := fs.WalkDir(Migrations, "migrations", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := fs.ReadFile(Migrations, p)
		if rerr != nil {
			return rerr
		}
		all[p] = &fstest.MapFile{Data: b}
		if p < "migrations/0031" {
			upTo[p] = &fstest.MapFile{Data: b}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, upTo); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO devices (name, address, mac, topic, created_at, updated_at) VALUES ('old','10.0.0.1','aa:bb:cc:00:00:09','t',0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, all); err != nil {
		t.Fatal(err)
	}
	var subnet, dns, ota, names string
	if err := db.QueryRowContext(ctx, `SELECT net_subnet, net_dns, ota_url, relay_names FROM devices WHERE name='old'`).Scan(&subnet, &dns, &ota, &names); err != nil {
		t.Fatal(err)
	}
	if subnet != "" || dns != "" || ota != "" || names != "" {
		t.Fatalf("defaults: %q %q %q %q", subnet, dns, ota, names)
	}
}

// Why: live databases upgrade with discovered devices waiting in the list (#130); their rows
// must load with an empty hostname (shown as a dash) and lose nothing.
func TestMigration0032KeepsProposalsAndAddsAnEmptyHostname(t *testing.T) {
	db, path := rawDB(t)
	upTo, all := fstest.MapFS{}, fstest.MapFS{}
	err := fs.WalkDir(Migrations, "migrations", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := fs.ReadFile(Migrations, p)
		if rerr != nil {
			return rerr
		}
		all[p] = &fstest.MapFile{Data: b}
		if p < "migrations/0032" {
			upTo[p] = &fstest.MapFile{Data: b}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, upTo); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO discovery_proposals (mac, kind, via, name, address, topic, first_seen, seen_at) VALUES ('aa:bb:cc:00:00:09','new','mqtt','old','10.0.0.1','t',1,2)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, all); err != nil {
		t.Fatal(err)
	}
	var name, host string
	if err := db.QueryRowContext(ctx, `SELECT name, hostname FROM discovery_proposals`).Scan(&name, &host); err != nil {
		t.Fatal(err)
	}
	if name != "old" || host != "" {
		t.Fatalf("row: %q %q", name, host)
	}
}
