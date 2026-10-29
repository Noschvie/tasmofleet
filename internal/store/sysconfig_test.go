package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// Why: a secret setting (the MQTT password) must not sit in the database file
// in clear text, like device passwords.
func TestSystemSettingSecretsAreEncryptedAtRest(t *testing.T) {
	s, path, _ := newStore(t)
	ctx := context.Background()
	err := s.SaveSystemSettings(ctx, "alice", []SystemSettingChange{
		{Key: "MQTT_PASSWORD", Value: "fake-pw-9f3a", Secret: true, Target: "tcp://broker.lan:1883"}, {Key: "BACKUP_KEEP", Value: "4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got["MQTT_PASSWORD"].Value != "fake-pw-9f3a" || got["MQTT_PASSWORD"].Target != "tcp://broker.lan:1883" || !got["MQTT_PASSWORD"].Secret || got["BACKUP_KEEP"].Value != "4" || got["BACKUP_KEEP"].UpdatedBy != "alice" {
		t.Fatalf("loaded %+v", got)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT value FROM system_settings WHERE key='MQTT_PASSWORD'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "fake-pw-9f3a") {
		t.Fatal("secret stored in clear text")
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "fake-pw-9f3a") {
		t.Fatal("secret visible in the database file")
	}
}

// Why: one form save is one transaction, and deleting a stored value is how a
// field goes back to its default.
func TestSystemSettingsSaveAndDelete(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	if err := s.SaveSystemSettings(ctx, "a", []SystemSettingChange{{Key: "A", Value: "1"}, {Key: "B", Value: "2"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSystemSettings(ctx, "b", []SystemSettingChange{{Key: "A", Delete: true}, {Key: "B", Value: "3"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadSystemSettings(ctx)
	if _, ok := got["A"]; ok || got["B"].Value != "3" || got["B"].UpdatedBy != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestPolicyRuleVersionsKeepHistory(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.CurrentPolicyRules(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty: %v", err)
	}
	v1, _ := s.AddPolicyRuleVersion(ctx, PolicyRuleVersion{Rules: `[]`, Actor: "a", Note: "first"})
	v2, _ := s.AddPolicyRuleVersion(ctx, PolicyRuleVersion{Rules: `[{"command":"SaveData","want":"0"}]`, Actor: "b"})
	v3, err := s.AddPolicyRuleVersion(ctx, PolicyRuleVersion{Rules: `[]`, Actor: "c", RevertOf: v1})
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := s.CurrentPolicyRules(ctx)
	if cur.ID != v3 || cur.RevertOf != v1 {
		t.Fatalf("current %+v", cur)
	}
	list, _ := s.ListPolicyRuleVersions(ctx, 2)
	if len(list) != 2 || list[0].ID != v3 || list[1].ID != v2 {
		t.Fatalf("list %+v", list)
	}
	old, err := s.GetPolicyRuleVersion(ctx, v1)
	if err != nil || old.Note != "first" {
		t.Fatalf("old %+v %v", old, err)
	}
}

// Why: the History tab pages with a cursor; a page must start strictly below the
// cursor so no version appears twice or is skipped between two pages.
func TestListPolicyRuleVersionsBeforePagesWithoutOverlap(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 5; i++ {
		id, err := s.AddPolicyRuleVersion(ctx, PolicyRuleVersion{Rules: `[]`, Actor: "a"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	top, _ := s.ListPolicyRuleVersionsBefore(ctx, 0, 2)
	if len(top) != 2 || top[0].ID != ids[4] || top[1].ID != ids[3] {
		t.Fatalf("beforeID 0 must start at the newest: %+v", top)
	}
	next, _ := s.ListPolicyRuleVersionsBefore(ctx, top[1].ID, 2)
	if len(next) != 2 || next[0].ID != ids[2] || next[1].ID != ids[1] {
		t.Fatalf("second page %+v", next)
	}
	last, _ := s.ListPolicyRuleVersionsBefore(ctx, next[1].ID, 2)
	if len(last) != 1 || last[0].ID != ids[0] {
		t.Fatalf("last page %+v", last)
	}
	none, _ := s.ListPolicyRuleVersionsBefore(ctx, ids[0], 2)
	if len(none) != 0 {
		t.Fatalf("below the oldest: %+v", none)
	}
}

// Why (review F7): a stored secret that cannot be decrypted (key lost, database
// restored elsewhere) must not stop the start; the row is reported as unreadable
// and the admin enters the value again.
func TestUndecryptableSecretIsReportedNotFatal(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO system_settings (key, value, sealed, updated_by, updated_at) VALUES ('MQTT_PASSWORD','not-a-sealed-value',1,'x',1)`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSystemSettings(ctx, "a", []SystemSettingChange{{Key: "BACKUP_KEEP", Value: "4"}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatalf("load must not fail: %v", err)
	}
	if !got["MQTT_PASSWORD"].Unreadable || got["MQTT_PASSWORD"].Value != "" || got["BACKUP_KEEP"].Value != "4" {
		t.Fatalf("got %+v", got)
	}
}
