-- 0051_mqtt_acl_warning_hidden: the checkbox that hides the red broker ACL warning is a statement about the warning,
-- not about the broker, and its setting is renamed to say so (#962): MQTT_ACL_CONFIRMED became
-- MQTT_ACL_WARNING_HIDDEN. Data only. The stored row moves to the new key with its value (the broker, as host:port|user,
-- it was given for) and updated_by unchanged, so a confirmation made before the upgrade stays a hidden warning for
-- that same broker and the red notice does not return on every page. Without this the settings service, which loads
-- only keys it knows, would drop the row without a word. Audit rows keep the old name: they are history. A downgrade
-- restores the .pre-51 copy.
UPDATE system_settings SET key = 'MQTT_ACL_WARNING_HIDDEN' WHERE key = 'MQTT_ACL_CONFIRMED';
