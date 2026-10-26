# Discovery: untrusted candidates and the admin's confirmation

Scope: `internal/discovery`, `internal/store/discovery.go`, `internal/web/discovery.go`.
Review findings are judged against this document. Every claim has a test.

## Model

A **candidate** is a claim from the network: an MQTT message on
`tasmota/discovery/<MAC>/config`, a `stat/<topic>/STATUS5` reply to discovery's own
query (spec 11.3a), or the answer to a scan probe. Any host in the
device networks (and any MQTT publisher) can claim any MAC, name, topic or IP.
A **proposal** is a stored candidate. It never carries credentials.
A **version** is a SHA-256 digest (full length) over everything an admin sees and `Accept` applies
(kind, MAC, address, port, existing device, name, topic, full topic).

## Invariants

| ID  | Invariant                                                                                                                                                                                                                                                 |
| --- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| D1  | Discovery never creates a device. Only `Accept` (admin) does, through `inventory.Create`.                                                                                                                                                                 |
| D2  | An existing device changes only through `inventory.Upsert` (its identity rules decide) or through `Accept`. Candidate topic, full topic and name are never merged into a record matched by MAC.                                                           |
| D3  | What `Upsert` refuses becomes a proposal: `ErrNeedsVerification` is `address-change`, `ErrIdentityMismatch` is `conflict`. A conflict cannot be accepted.                                                                                                |
| D4  | Fields are sanitised before storing (control characters, length, topic charset, MAC format, topic MAC equals payload MAC, payload at most 16 KiB). Addresses outside `DEVICE_NETWORKS` (per the dialer rule) never become proposals; such a claim is kept in the in-memory topic table (bounded, D12) and evaluated again when `DEVICE_NETWORKS` changes, only if it is at most 1 h old and no newer sighting of its MAC is stored (#45).                                   |
| D5  | A scan checks every CIDR against `DEVICE_NETWORKS` and the 1024 address cap before any packet. Loopback and link-local are skipped unless a device network names them explicitly. Probes carry no credentials, 16 in flight, 2 s, one retry on timeout.  |
| D6  | MQTT work is bounded: payloads over 16 KiB are dropped before queueing, messages are coalesced per MAC, at most 4096 messages and 8 MiB queued, one evaluation per MAC per 15 s (the rate table never resets: entries expire, a full table refuses new MACs), one worker. Proposals are capped at 500 visible **per class** (unknown devices versus address-change and conflict, so fake MACs cannot hide the spoofing signal) and expire after 30 days without a sighting. |
| D7  | `Accept` of a proposal applies only the version the admin saw (`AcceptVersion`). A newer sighting with other content has another version and the old confirmation fails with `ErrStale`. Accept and the evaluation of sightings share one lock, so the proposal cannot change between the check and the write. The web form always sends the shown version and refuses a POST without it. |
| D7a | **Superseded 2026-10-20** (over-restriction audit #13): accepting an address change no longer asks for an "I checked that this is the device" checkbox. The server decides: Accept probes the new address itself and requires the device's MAC (D8), and D7 still ties the accept to the shown version. | `TestAddressChangeNeedsNoCheckbox` |
| D8  | Discovery never moves a device that has stored credentials, and never binds a MAC to a device that has none (`MoveRefusal`, `ErrNotAcceptable`, no confirm button, the page says why). Reason: a genuine password-protected Tasmota cannot answer a credential-less probe, so an answer carrying the claimed MAC proves an impostor, and moving the record would hand it the stored password. Such addresses are changed in the device edit form. For a bound device without credentials, Accept probes the new address without credentials right before the write and requires the candidate's and the device's MAC. No verifier means no discovery move. |
| D9  | The write is a compare-and-set (`inventory.UpdateIf`): if the device changed after Accept read it (rebound, edited, deleted), nothing is written (`ErrStale`). `inventory.UpsertExisting` never creates, so a device deleted between lookup and write is not resurrected (D1). |
| D10 | Admin only. Scan start/cancel/end, accept and dismiss are audited; refused ranges too.                                                                                                                                                                  |
| D11 | A dismissed proposal stays hidden while its content is unchanged (retained messages replay at every reconnect) and reappears when it changes.                                                                                                            |
| D12 | MQTT-only sightings (LWT, STATE, INFO) never become a proposal: they carry no MAC. Discovery asks such a topic for its MAC with one `Status 5` over MQTT (never `Status 0`, safety rule 2) only while the topic is unresolved and online, at most once per topic per 10 min, one command at a time and at least 1 s apart for the bus, after 30 s online. Each query is audited (`discovery.identify`) and `DISCOVERY_MQTT` off stops them. At most 1024 unresolved topics are kept (oldest dropped). A fake LWT flood therefore costs at most one small command per second. |
| D13 | A `STATUS5` reply is used only for the exact topic discovery asked, within 30 s of the question, while it is still unresolved; it is a claim like a discovery config (D4 sanitising, D2/D3 identity rules). Replies to tasmofleet's own HTTP probes of inventory devices (Tasmota mirrors them on MQTT) are ignored. |
| D14 | A scan sighting never overwrites the topic or full topic a MQTT sighting stored for the same MAC (topic and online state of a device seen on MQTT come from MQTT); a MQTT sighting keeps the port a scan found. |
| D15 | Accepting a proposal whose topic's last LWT is `Offline` skips the MAC check (nothing is sent anywhere). The fleet default password still needs a proven MAC, so for such an accept it is withheld: the device is stored without it and the audit row carries `default_password_withheld`. Every accept without MAC proof carries `mac_unverified` in its audit row. Before the first probe of a device without identity that comes online (an MQTT trigger anyone can publish; the probe carries the stored credentials) its stored address is asked the credential-less Status 5: another MAC than the stored one refuses the probe (`discovery.identify_refused`); a device with a stored MAC **and a stored password** whose address cannot prove the MAC (refusal, 401, timeout) does not get the password on this trigger (`discovery.identify_unverified`, retried every 10 min; acceptMove rule, audit row 14); without a password the probe runs. A device without a stored MAC is probed at the address the admin bound with its password (add form, import, edit; the health poller sends them there as well), and the probe binds no MAC (an automatic `Prober.Refresh`, actor "", never writes one; only a person's refresh binds, #65; pinned by `TestRefreshBindsTheMACOnlyForAnAdmin`); `discovery.reprobe` records the MAC that answered. Re-probing a stored device that comes online without identity uses the stored record, at most once per device per 10 min, and never changes its MAC; its topic follows the device only under the refresh rules (spec 5, Refresh identity). |

## Residual risks (MQTT-first, review 2026-10-26)

- **Topic claims.** Any broker publisher can claim a topic for a MAC (as with
  `tasmota/discovery` before). Since D14 the MQTT topic of a proposal wins over a
  scan sighting, so a spoofed claim for a MAC that a scan found can put a wrong topic
  into the proposal, and an accept stores it; MQTT health would then follow the wrong
  topic until an admin fixes it. Accept shows the topic before it is applied, and a
  broker ACL (below) removes the risk. Low.
- **Unverified accept.** A device accepted while offline is bound to the claimed MAC
  without proof (`mac_unverified`); nothing secret is sent, and the first probe checks
  the MAC first. A host that answers no Status 5 without credentials (a
  password-protected impostor) is probed with the stored password only if the admin set
  one for it. Low. Since #65 every `Prober.Refresh` (manual, accept, reprobe) sends `Status 5`
  first and refuses a device with another MAC before any further command; the stored password
  goes out with that one command (as with the health poller, which also sends it to the stored
  address). A device stored without a MAC (import) has nothing to compare and is probed at the
  address the admin set; a person's refresh then binds the reported MAC, an automatic one
  (reprobe) does not (D8). Minimal firmware answers no Status 5, so a refresh of a device that
  runs it cannot check the MAC. Low.
- **Topic table eviction (L2, review of #66).** Past 1024 tracked topics the oldest is
  dropped, and a dropped topic that reappears can be asked again before its 10 minutes are
  up; the bus-wide limit of one query per second still holds. A spoofed discovery config
  that names a real topic marks it as resolved, so that topic is not asked (its own
  discovery config or a scan still finds it). Low.
- **Kept outside-networks claims (review of #82, #45).** A claim refused only for its
  address is re-evaluated when the device networks grow. Its address can be stale: the
  claim is used only up to 1 h after it was seen (older: the topic is asked again with
  Status 5), a newer stored sighting of the MAC wins, and the claim is dropped when the
  topic becomes an inventory device. Within that hour a device that moved without a new
  message can still be proposed at its old address; Accept checks the MAC there (D8,
  D15 for offline topics). The re-evaluation sends no MQTT command, but the inventory
  rules may probe the claimed address without credentials for a known MAC, as for any
  sighting. Low.
- **Synchronous MAC check (L4, review of #66).** The credential-less Status 5 before a
  reprobe runs on the discovery loop; at Attach, many devices without identity coming
  online at once delay the processing of queued messages by up to one HTTP timeout each
  (bounded by once per device per 10 minutes). Nothing is lost, the queue is bounded. Low.

## Deployment

- **Broker ACL.** Only Tasmota devices should be allowed to publish
  `tasmota/discovery/#`. Without an ACL, any host that can reach the broker can
  publish discovery messages: it can fill the proposal list, and it can trigger
  the documented automatic move of a credential-less, MAC-bound device
  (`inventory.Upsert` accepts an address whose probe answers with the device's
  MAC; any host can answer so). Set `message_size_limit` (for example 64 KiB) on
  the broker as well.
- Credentialed devices are never moved by discovery (D8), so the automatic move
  affects only devices without a WebPassword; consider setting one.
