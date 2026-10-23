# MQTT ingest: threat model and bounds

Scope: everything tasmofleet accepts from the MQTT broker (`internal/mqttbus`, the
known-device index in `internal/app/mqttknown.go`, consumers health and discovery).
Further findings are judged against this document. A finding is valid if it breaks a
claim below or shows a residual risk worse than stated.

## Attacker model

- **M1, any IoT host.** It can publish to the broker. Tasmota devices share broker
  credentials, so one compromised device, or anyone with the `tasmota` password, can
  publish any topic and set the retained flag. It cannot read tasmofleet's memory or the
  database.
- **M2, a spoofing device.** An M1 attacker publishes on the topics of a managed device
  (`tele/<topic>/LWT`, `stat/<topic>/STATUS*`, `tasmota/discovery/<MAC>/config`).
- MQTT carries no authenticity. tasmofleet never treats an MQTT message as proof of
  identity or as permission: discovery only creates proposals, and address moves of
  credentialed devices need an admin (`docs/security/` inventory notes).

## Mechanism

1. **Payload cap**: messages over 256 KiB are dropped before any parser
   (`payload_too_large`).
2. **Bounded topic grammar**: only the suffixes tasmofleet uses are decoded (LWT, STATE,
   SENSOR, INFO1-3, RESULT, UPGRADE, STATUS + 0-2 digits, POWER + 0-2 digits, discovery
   config). Everything else is ignored, so one prefix cannot mint unlimited distinct
   decodable topics.
3. **Retained replay cache** (the cache only serves consumers that subscribe after the
   retained burst; live dispatch to handlers never depends on it):
   - sizes count **topic + payload** bytes; every path (new, overwrite, clear,
     reclassify) goes through one remove+add routine;
   - **one canonical key**: classification and storage both use `mqttbus.DeviceKey`
     on the decoded message (exact bytes; inventory values are mapped with
     `TopicKey`/`MACKey`). Decoding is injective: only canonical topics decode;
   - **per-device reservations**: a message the known-device index attributes to an
     inventory device is accounted under that device's key (`topic:<topic>`, or
     `mac:<MAC>` for discovery). Each key holds at most 32 topics and 256 KiB. Known
     entries are never evicted. A known message beyond its key's reservation is treated
     as unknown (`device_reservation_full`). There are at most 4096 keys
     (`known_keys_full`, far above any homelab fleet);
   - **unknown pool**: 1024 topics / 4 MiB shared, oldest evicted first
     (`unknown_evicted`); an entry larger than the pool is refused (`unknown_too_large`);
   - drops and evictions are counted in `tasmofleet_mqtt_dropped_total{reason}`
     (Grafana panel).
4. **Known-device index** (app): an in-memory set of inventory topics and MACs. It is
   rebuilt at start, on every inventory change event and every minute, and then the bus
   re-classifies all cached entries (`Reclassify`), so removed devices lose and added
   devices gain their reservation. **Fail closed**: if the inventory cannot be read, the
   index becomes empty, and every topic competes in the unknown pool until the next
   successful refresh.

## Claims (each with a test)

| # | Claim | Test |
|---|---|---|
| Q1 | junk topics (1e5) never evict a managed device's cached LWT | `TestJunkTopicsCannotEvictInventoryDevices` |
| Q2 | spoofed topics on one device's prefix use only that device's reservation | `TestSpoofedKnownTopicsStayInOneReservation` |
| Q3 | accounting is exact after overwrite, clear and class-change churn; no pool exceeds its budget | `TestAccountingSurvivesOverwriteChurn` |
| Q4 | topic length counts against the budget | `TestTopicLengthCounts` |
| Q5 | reservations follow the inventory (removed loses, added gains) | `TestReclassifyFollowsInventory` |
| Q6 | handlers receive every message regardless of cache state | `TestDispatchUnaffectedByCacheLimits` |
| Q7 | oversized payloads reach no handler and are counted | `TestOversizedPayloadIsDropped` |
| Q8 | unbounded digit suffixes are not decoded | `TestSuffixDigitsAreBounded` |
| Q9 | an unreadable inventory yields an empty index (no stale reservations) | `TestKnownIndexEmptyKnowsNothing` |
| Q10 | discovery messages are attributed by MAC, others by topic | `TestKnownIndex` |
| Q11 | one key function: classification (Options.Known via `TopicKey`/`MACKey`) and storage (`DeviceKey`) use the same canonical key; every decoded topic re-encodes to exactly the raw topic under all FullTopic patterns | `FuzzDecodeIsCanonical` |
| Q12 | non-canonical spellings (lower-case MAC in discovery, empty/extra levels, case-changed suffix) do not decode, so one logical device cannot hold several reservations | `TestDiscoveryMACMustBeCanonical`, `FuzzDecodeIsCanonical` |

## Accepted residual risks

- **R1, spoofed content.** M2 can publish a false LWT/STATE for a managed device. Health
  may then show a wrong state until the next HTTP poll or genuine message. This cannot be
  fixed without broker-side authorization. OTA and identity decisions use HTTP only.
- **R2, reservation squatting on a device's own topics** (includes spoofing *within* one
  device's reservation: any publisher can write `tele/<victim>/...`; covered by R1 until
  broker ACLs exist). M2 can fill its victim device's
  32-topic / 256 KiB reservation with junk. The genuine messages then fall into the
  unknown pool and can be evicted from the replay cache. This affects only that device's
  replay after a consumer restart; live dispatch and HTTP polling are unaffected.
- **R3, unknown pool churn.** M1 can keep the unknown pool full, so proposals for genuinely
  new devices may arrive only live (no replay). Discovery still sees live messages.
- **R4, message rate.** There is no rate limit per publisher (the broker does not tell
  tasmofleet who published). CPU per message is bounded (payload cap, cheap index lookup
  without database access).

## Recommendation (operations)

Configure **broker ACLs** so each Tasmota device can only publish under its own topic,
and only tasmofleet/Home Assistant can read everything. That removes M1's ability to
spoof other devices (R1, R2) at the source. Mosquitto: one user per device, or pattern ACLs
with `%u`/`%c`. Until then, treat MQTT as untrusted input; tasmofleet does.
