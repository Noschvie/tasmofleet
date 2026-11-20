package store

import (
	"errors"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/secretbox"
)

// Why (#1005, the maintainer's decision): live databases upgrade with devices whose GroupTopic2 to GroupTopic4 nobody
// has read. The migration must leave them unread (no stamp, no list), never "read and empty": that would let MQTT
// writes reach a device listening on another one's topic until somebody happened to read it.
func TestMigration0054LeavesGroupTopics2To4UnreadForExistingDevices(t *testing.T) {
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
		if p < "migrations/0054" {
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
	if _, err := db.ExecContext(ctx, `INSERT INTO devices (name, address, mac, topic, group_topic, group_topic_read_at, created_at, updated_at)
		VALUES ('old','10.0.0.1','aa:bb:cc:00:00:09','t','tasmotas',1700000000000,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, all); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &SQLite{db: db, box: box, now: time.Now}
	d, err := s.GetDevice(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.GroupTopic != "tasmotas" || d.GroupTopicReadAt.IsZero() {
		t.Fatalf("GroupTopic1 %q at %v: the migration must keep it", d.GroupTopic, d.GroupTopicReadAt)
	}
	if d.MoreGroupTopics != nil || !d.MoreGroupTopicsReadAt.IsZero() || !d.GroupTopicsReadAt().IsZero() {
		t.Fatalf("GroupTopic2 to 4 %v at %v after the upgrade, want unread", d.MoreGroupTopics, d.MoreGroupTopicsReadAt)
	}
}

// Why (#1005): one bare GroupTopic read stores all four group topics with one stamp, under the guard of every probe
// write (a device deleted, moved, rebound or given another topic since the read began is left alone), and an empty
// list that was read stays known after a reload. A device created from a first probe keeps what that probe read.
func TestUpdateGroupTopicsStoresAllFourWithOneStampUnderTheGuard(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "plug", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01", Topic: "plug"})
	if err != nil {
		t.Fatal(err)
	}
	guard := ProbeGuard{Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"}
	for _, g := range []ProbeGuard{{Address: "192.0.2.11", MAC: guard.MAC}, {Address: guard.Address, MAC: "aa:bb:cc:00:00:02"}} {
		if err := s.UpdateGroupTopics(ctx, id, g, "plug", "tasmotas", []string{"kitchen"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("guard %+v: %v, want ErrConflict", g, err)
		}
	}
	if err := s.UpdateGroupTopics(ctx, id, guard, "other", "tasmotas", []string{"kitchen"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("another topic: %v, want ErrConflict", err)
	}
	if d, _ := s.GetDevice(ctx, id); !d.GroupTopicsReadAt().IsZero() || len(d.GroupTopics()) != 0 {
		t.Fatalf("a refused write stored %v at %v", d.GroupTopics(), d.GroupTopicsReadAt())
	}
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return at }
	if err := s.UpdateGroupTopics(ctx, id, guard, "plug", "tasmotas", []string{"kitchen", "floor/1"}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if !slices.Equal(d.GroupTopics(), []string{"tasmotas", "kitchen", "floor/1"}) || !d.GroupTopicReadAt.Equal(at) || !d.MoreGroupTopicsReadAt.Equal(at) {
		t.Fatalf("stored %v at %v/%v, want all three with one stamp", d.GroupTopics(), d.GroupTopicReadAt, d.MoreGroupTopicsReadAt)
	}
	if err := s.UpdateGroupTopics(ctx, id, guard, "plug", "tasmotas", nil); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.GetDevice(ctx, id); d.MoreGroupTopics != nil || !d.GroupTopicsReadAt().Equal(at) {
		t.Fatalf("an empty list read: %v at %v, want none, and known", d.MoreGroupTopics, d.GroupTopicsReadAt())
	}
	created, err := s.CreateDevice(ctx, model.Device{Name: "new", Address: "192.0.2.12", MAC: "aa:bb:cc:00:00:03", Topic: "new",
		GroupTopic: "tasmotas", GroupTopicReadAt: at, MoreGroupTopics: []string{"hall"}, MoreGroupTopicsReadAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ = s.GetDevice(ctx, created); !slices.Equal(d.GroupTopics(), []string{"tasmotas", "hall"}) || !d.MoreGroupTopicsReadAt.Equal(at) {
		t.Fatalf("created device: %v at %v, want what its first probe read", d.GroupTopics(), d.MoreGroupTopicsReadAt)
	}
}

// Why (#1005, the maintainer's decision): INFO1 and Status 1 carry GroupTopic1 only. They keep updating GroupTopic1 and
// never clear or re-stamp GroupTopic2 to GroupTopic4: a cleared list would hide a device listening on another one's
// topic, and a new stamp would vouch for values they never carried.
func TestGroupTopics2To4SurviveAnINFO1AndAStatus1(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "plug", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01", Topic: "plug"})
	if err != nil {
		t.Fatal(err)
	}
	guard := ProbeGuard{Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"}
	read := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return read }
	if err := s.UpdateGroupTopics(ctx, id, guard, "plug", "tasmotas", []string{"kitchen", "hall"}); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return read.Add(time.Minute) }
	if err := s.UpdateGroupTopicFromMQTT(ctx, id, guard, "plug", "upstairs"); err != nil { // INFO1
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if d.GroupTopic != "upstairs" || !d.GroupTopicReadAt.Equal(read.Add(time.Minute)) {
		t.Fatalf("GroupTopic1 %q at %v: INFO1 must still update GroupTopic1", d.GroupTopic, d.GroupTopicReadAt)
	}
	if !slices.Equal(d.MoreGroupTopics, []string{"kitchen", "hall"}) || !d.MoreGroupTopicsReadAt.Equal(read) {
		t.Fatalf("after INFO1: %v at %v, want GroupTopic2 to 4 and their stamp unchanged", d.MoreGroupTopics, d.MoreGroupTopicsReadAt)
	}
	// a probe whose Status 1 had GroupTopic1 but whose bare GroupTopic read failed
	if err := s.UpdateProbe(ctx, id, guard, ProbeResult{SetGroupTopic: true, GroupTopic: "tasmotas", SetOTAURL: true}); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, id)
	if !slices.Equal(d.MoreGroupTopics, []string{"kitchen", "hall"}) || !d.MoreGroupTopicsReadAt.Equal(read) {
		t.Fatalf("after a probe without the GroupTopic read: %v at %v, want them kept", d.MoreGroupTopics, d.MoreGroupTopicsReadAt)
	}
	// a probe that read them stores them with its own stamp, the one of its OtaUrl
	if err := s.UpdateProbe(ctx, id, guard, ProbeResult{SetGroupTopic: true, GroupTopic: "tasmotas", SetOTAURL: true,
		SetMoreGroupTopics: true, MoreGroupTopics: []string{"cellar"}}); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, id)
	if !slices.Equal(d.MoreGroupTopics, []string{"cellar"}) || !d.MoreGroupTopicsReadAt.Equal(d.OTAURLReadAt) {
		t.Fatalf("a probe that read them: %v at %v (OtaUrl at %v)", d.MoreGroupTopics, d.MoreGroupTopicsReadAt, d.OTAURLReadAt)
	}
}

// Why (#1005, review): after TasmoFleet sent a GroupTopic<n> with a value or restored a configuration, GroupTopic2 to
// GroupTopic4 may have changed and nothing refreshes them before the next read. Forgetting must make the list unread
// (so the other devices' MQTT writes wait) but keep the values (the device most likely still listens on them, and INFO1
// or Status 1 never clear them either), and leave GroupTopic1 to INFO1.
func TestForgetGroupTopicsMakesGroupTopics2To4UnreadAndKeepsThem(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "plug", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01", Topic: "plug"})
	if err != nil {
		t.Fatal(err)
	}
	read := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return read }
	if err := s.UpdateGroupTopics(ctx, id, ProbeGuard{Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"}, "plug", "tasmotas", []string{"kitchen"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetGroupTopics(ctx, id); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if !d.MoreGroupTopicsReadAt.IsZero() || !d.GroupTopicsReadAt().IsZero() {
		t.Fatalf("GroupTopic2 to 4 still read at %v: the list must count as unread", d.MoreGroupTopicsReadAt)
	}
	if !slices.Equal(d.GroupTopics(), []string{"tasmotas", "kitchen"}) || !d.GroupTopicReadAt.Equal(read) {
		t.Fatalf("group topics %v, GroupTopic1 read at %v: the values and GroupTopic1's stamp must stay", d.GroupTopics(), d.GroupTopicReadAt)
	}
	if err := s.ForgetGroupTopics(ctx, id+100); err != nil {
		t.Fatalf("a device that is gone: %v, want no error", err)
	}
}

// Why (#1005, review): a probe whose read began before ForgetGroupTopics holds GroupTopic1 from before TasmoFleet's
// own change as well; it must not overwrite the stored GroupTopic1 (which INFO1 may have refreshed since) or stamp the
// list as read. The same probe's other facts are stored as usual.
func TestAProbeFromBeforeTheMarkStoresNoGroupTopic(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "plug", Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01", Topic: "plug"})
	if err != nil {
		t.Fatal(err)
	}
	g := ProbeGuard{Address: "192.0.2.10", MAC: "aa:bb:cc:00:00:01"}
	if err := s.UpdateGroupTopics(ctx, id, g, "plug", "kitchen", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetGroupTopics(ctx, id); err != nil { // the probe began before this (count 0)
		t.Fatal(err)
	}
	r := ProbeResult{Identity: model.Identity{Version: "15.0.1"}, SetGroupTopic: true, GroupTopic: "tasmotas", SetMoreGroupTopics: true, MoreGroupTopics: []string{"old"}}
	if err := s.UpdateProbe(ctx, id, g, r); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if d.GroupTopic != "kitchen" || len(d.MoreGroupTopics) != 0 || !d.MoreGroupTopicsReadAt.IsZero() {
		t.Fatalf("group topics %v read at %v: an answer from before the mark was stored", d.GroupTopics(), d.MoreGroupTopicsReadAt)
	}
	if d.Identity.Version != "15.0.1" {
		t.Fatalf("version %q: the rest of the probe must still be stored", d.Identity.Version)
	}
}
