package store

import "testing"

// Why (#962): MQTT_ACL_CONFIRMED became MQTT_ACL_WARNING_HIDDEN without an alias. The settings service loads only keys
// it knows, so a stored row under the old key would be dropped without a word and the red broker warning that an admin
// had hidden would return on every page after the upgrade. The row must move to the new key with its value (the broker
// it is bound to) and its author, and nothing else may change.
func TestMigration0051CarriesTheACLConfirmationOverAsHidden(t *testing.T) {
	s := migrateFrom(t, "0051", map[string]string{
		"MQTT_ACL_CONFIRMED": "broker.lan:1883|tf",
		"MQTT_URL":           "tcp://broker.lan:1883",
	})
	if v, ok := setting(t, s, "MQTT_ACL_WARNING_HIDDEN"); !ok || v != "broker.lan:1883|tf" {
		t.Errorf("MQTT_ACL_WARNING_HIDDEN = %q, %v; want the value of the old row", v, ok)
	}
	if v, ok := setting(t, s, "MQTT_ACL_CONFIRMED"); ok {
		t.Errorf("the old key is still stored (%q): the service would ignore it and the migration would run for nothing", v)
	}
	var by string
	if err := s.db.QueryRowContext(ctx, `SELECT updated_by FROM system_settings WHERE key = 'MQTT_ACL_WARNING_HIDDEN'`).Scan(&by); err != nil || by != "adam" {
		t.Errorf("updated_by = %q, %v; the admin who made the decision must stay on it", by, err)
	}
	if v, _ := setting(t, s, "MQTT_URL"); v != "tcp://broker.lan:1883" {
		t.Errorf("an unrelated setting changed: MQTT_URL = %q", v)
	}
}

// Why (#962): an install that never confirmed anything must not get a hidden warning from the upgrade.
func TestMigration0051InventsNoHiddenWarning(t *testing.T) {
	s := migrateFrom(t, "0051", map[string]string{"MQTT_URL": "tcp://broker.lan:1883"})
	if v, ok := setting(t, s, "MQTT_ACL_WARNING_HIDDEN"); ok {
		t.Errorf("MQTT_ACL_WARNING_HIDDEN = %q was created for an install that had not confirmed", v)
	}
}
