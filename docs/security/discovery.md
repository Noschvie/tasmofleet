# Discovery: untrusted candidates and the admin's confirmation

Scope: `internal/discovery`, `internal/store/discovery.go`, `internal/web/discovery.go`.
Review findings are judged against this document. Every claim has a test.

## Model

A **candidate** is a claim from the network: an MQTT message on
`tasmota/discovery/<MAC>/config` or the answer to a scan probe. Any host in the
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
| D4  | Fields are sanitised before storing (control characters, length, topic charset, MAC format, topic MAC equals payload MAC, payload at most 16 KiB). Addresses outside `DEVICE_NETWORKS` (per the dialer rule) are dropped.                                   |
| D5  | A scan checks every CIDR against `DEVICE_NETWORKS` and the 1024 address cap before any packet. Loopback and link-local are skipped unless a device network names them explicitly. Probes carry no credentials, 16 in flight, 2 s, one retry on timeout.  |
| D6  | MQTT work is bounded: payloads over 16 KiB are dropped before queueing, messages are coalesced per MAC, at most 4096 messages and 8 MiB queued, one evaluation per MAC per 15 s (the rate table never resets: entries expire, a full table refuses new MACs), one worker. Proposals are capped at 500 visible **per class** (unknown devices versus address-change and conflict, so fake MACs cannot hide the spoofing signal) and expire after 30 days without a sighting. |
| D7  | `Accept` of a proposal applies only the version the admin saw (`AcceptVersion`). A newer sighting with other content has another version and the old confirmation fails with `ErrStale`. Accept and the evaluation of sightings share one lock, so the proposal cannot change between the check and the write. The web form always sends the shown version and refuses a POST without it. |
| D7a | **Superseded 2026-10-20** (over-restriction audit #13): accepting an address change no longer asks for an "I checked that this is the device" checkbox. The server decides: Accept probes the new address itself and requires the device's MAC (D8), and D7 still ties the accept to the shown version. | `TestAddressChangeNeedsNoCheckbox` |
| D8  | Discovery never moves a device that has stored credentials, and never binds a MAC to a device that has none (`MoveRefusal`, `ErrNotAcceptable`, no confirm button, the page says why). Reason: a genuine password-protected Tasmota cannot answer a credential-less probe, so an answer carrying the claimed MAC proves an impostor, and moving the record would hand it the stored password. Such addresses are changed in the device edit form. For a bound device without credentials, Accept probes the new address without credentials right before the write and requires the candidate's and the device's MAC. No verifier means no discovery move. |
| D9  | The write is a compare-and-set (`inventory.UpdateIf`): if the device changed after Accept read it (rebound, edited, deleted), nothing is written (`ErrStale`). `inventory.UpsertExisting` never creates, so a device deleted between lookup and write is not resurrected (D1). |
| D10 | Admin only. Scan start/cancel/end, accept and dismiss are audited; refused ranges too.                                                                                                                                                                  |
| D11 | A dismissed proposal stays hidden while its content is unchanged (retained messages replay at every reconnect) and reappears when it changes.                                                                                                            |

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
