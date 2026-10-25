-- 0028_resolve_topic_placeholder: earlier versions stored the Topic setting as typed, e.g.
-- "tasmota_%06X". Tasmota expands "%06X" to the last 6 hex digits of the Wi-Fi MAC (upper case;
-- support.ino Format() with NetworkUniqueId()), and publishes under that. The stored placeholder
-- never matched a message, so the device showed "MQTT: Not connected" while online.
-- Only the exact form "<prefix>%06X" with a known MAC is resolved; anything else is left to the
-- next identity refresh. A topic already used by another device is not touched.
UPDATE devices SET topic = substr(topic, 1, length(topic) - 4) || upper(substr(replace(mac, ':', ''), 7))
WHERE mac <> '' AND length(replace(mac, ':', '')) = 12
  AND topic LIKE '%\%06X' ESCAPE '\'
  AND instr(topic, '%') = length(topic) - 3
  AND length(topic) <= 100
  AND NOT EXISTS (SELECT 1 FROM devices o WHERE o.id <> devices.id
                  AND o.topic = substr(devices.topic, 1, length(devices.topic) - 4) || upper(substr(replace(devices.mac, ':', ''), 7)));
