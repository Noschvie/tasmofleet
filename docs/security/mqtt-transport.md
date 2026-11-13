# Commands over MQTT: threat model and claims

Scope: how tasmofleet sends commands, settings writes and policy fixes to a device over its MQTT
broker instead of its HTTP API (`internal/transport`, `internal/mqttcmd`, spec 6.5 and 6.6).
Backup, restore and update over MQTT extend this document when they land. Findings are judged
against it: valid if they break a claim below or show a residual risk worse than stated.

## What changes compared with HTTP

- On the MQTT path only the broker protects the device (user, ACL, TLS). The device's web password
  plays no part; anyone who can publish to `cmnd/<topic>/#` can do what the console can do.
- Commands, settings writes and their replies travel through the broker. Since #605 every write of a device
  usable on TasmoFleet's broker goes there first, whatever it carries: a password (`WebPassword`, `MqttPassword`,
  `Password<n>`, `SSId<n>`, ...), a login inside a command (`OtaUrl http://user:pass@host/...`, `Sendmail`'s SMTP
  login), a change of the MQTT connection, a GPIO template. A secret is no reason for HTTP (the product owner's
  rule of 2026-11-14): on a usual Tasmota both ways are plain text. Tasmota serves no HTTPS, and TasmoFleet's HTTP
  call carries the command and the device's web password (HTTP Basic authentication) in clear; the device's leg to
  the broker is plain `tcp://` unless the firmware was built with TLS, so an `ssl://` URL on TasmoFleet's side does
  not protect the command on its way to the device. The difference: over HTTP a command goes from TasmoFleet to the
  device, over MQTT the broker delivers it, and the reply, to **every client subscribed to the device's topics**
  (`cmnd/<topic>/#`, `stat/<topic>/#`, `#`). Give the broker an ACL that lets only TasmoFleet and the device itself
  read and write a device's topics; nothing in TasmoFleet warns about a broker without one.
- What TasmoFleet shows and stores is masked as before, whatever the transport: the audit log and the console
  history hold only `cmdpolicy.Redacted` text, and replies pass `cmdpolicy.ScrubReply` (T7; command-policy L1, L2).

## Claims

| #   | Claim                                                                                                                                                                                                                                                                       | Test                                                                                                     |
| --- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| T1  | `Status 0` is never published, in any spelling, alone or in a `Backlog`, and not by a command that runs other commands: `Json`, `Publish`, `Publish2`, `Rule` with a rule text, `RuleTimer`, `If`, `WebSend`, `WebQuery`, `DevGroupSend`, `LoRaCommand`, `Br`, `Script`, `UfsRun` and a `Template` with a `CMND` key (in any case) are refused by the channel. Such a command goes over HTTP once (T2), also while MQTT is the first transport for writes (T11); the section "What still uses HTTP" says why. | `TestStatus0IsNeverPublished`, `TestCommandsThatRunOtherCommandsAreRefused`, `TestSimStatus0CannotBePublishedByAnExecutor`, `TestSimOnlyWhatMQTTCannotCarryGoesOverHTTPWhileHTTPWorks` |
| T2  | A command is sent on the other transport inside one call only when the first provably never left: HTTP to MQTT after a refused connection, an address outside DEVICE_NETWORKS, a 401, SetOption128; MQTT to HTTP after the channel refused the command or could not publish it. A timeout, an unparsable answer or a dropped connection is never followed by a second send. | `TestFailsOverToMQTTOnlyWhenNothingWasSent`, `TestRefusedConnectionIsOneMQTTSend`, `TestNotSent`, `TestAWriteMQTTDidNotPublishIsSentOnceOverHTTP`, `TestPowerMQTTDidNotPublishIsSentOverHTTP`, `TestPowerFallbackThroughTheRouterStaysOnHTTP` |
| T3  | A published command is never sent again over HTTP, also while HTTP works (a write over MQTT first that nobody answered is an unknown outcome). A channel closed while a command waits reports an unknown outcome, not "not sent"; a closed channel publishes nothing. | `TestAFailedMQTTSendIsNeverRepeatedOverHTTP`, `TestATimedOutMQTTWriteIsNeverResent`, `TestPowerOverMQTTUnconfirmedDoesNotRetryOverHTTP`, `TestCloseWakesACommandInFlight`, `TestDoAfterCloseNeverPublishes` |
| T4  | Reads follow the read path, every other command the stricter write path (an unread GroupTopic of this or another device blocks writes). Calls without a device ID never go to MQTT.                                                                                       | `TestReadsAndWritesFollowTheirOwnPath`, `TestTargetWithoutDeviceNeverUsesMQTT`                            |
| T5  | An upgrade or restore command is sent over MQTT only by a job that pinned the path.                                                                                                                                                                                         | `TestUpgradeAndRestoreCommandsOnlyGoOverMQTTPinned`                                                      |
| T6  | The topic is checked again right before the publish; a changed topic, group topic, FullTopic or MQTT capability sends nothing.                                                                                                                                              | `TestTopicChangeBetweenSnapshotAndSendSendsNothing`                                                      |
| T7  | The reply of a credential-setting command is masked before anything shows or stores it. `FileDownload`, `FileUpload` and `Upload` (their payload carries the device's MQTT password) are refused for every role.                                                              | `TestSecretReplyIsScrubbedOverMQTT`, `TestFileTransferCommandsAreNeverSent`                              |
| T8  | A change that can cut off a device reached over MQTT only (Topic, FullTopic, GroupTopic, Mqtt*, `SetOption3 0`, ...) asks in the console for a confirmation that says so whenever HTTP is not known to work: failing, or unknown (no answer within 15 minutes; a device live on MQTT is not polled over HTTP and a timeout is never recorded as a failure). While a fresh answer is on record the console sends without asking, and the router still asks HTTP right before (T10): when HTTP has gone silent since (the person's own `WebServer 0` a minute ago), the router's refusal turns into the same confirmation, so a resend is never a dead end. Opening the console asks for an HTTP probe, as the device page does, so a working HTTP is usually fresh. The bulk preview refuses such a change up front only for a device known to be reached over MQTT only (HTTP failing); for the others the router decides per device (T10). | `TestMQTTOnlyNamesTheCutOffConsequence`, `TestConsoleMQTTOnlyDeviceAsksBeforeACutOffChange`, `TestConsoleAsksWhenTheRouterRefusesACutOffChange`, `TestReachedOnlyOverMQTTMeansHTTPIsNotKnownToWork`, `TestOnlyOverMQTTFollowsWhetherHTTPIsKnownToWork`, `TestKnownOnlyOverMQTTMeansHTTPIsFailing`, `TestBulkCommandRefusesACutOffChangeForMQTTOnlyDevices`, `TestOpeningTheConsoleAsksForOneHTTPProbe`, `TestOverHTTPACutOffWriteIsSentAsAlways` |
| T10 | A write that can cut off a device reached over MQTT only is published without a person's confirmation (the console's) only while HTTP is proven to work right then: an answer to one `Status 11` the router asks over HTTP right before (it carries nothing of the write; the audit's `via` stays `mqtt`). A fresh answer on record is no proof (HTTP may have stopped since) and saves no question. HTTP failing, or silent to that question: without the confirmation the write is refused (`ErrCutOff`), nothing is published or sent over HTTP, and the silence of a refused connection is remembered as a failure. Make compliant and bulk never carry the confirmation; the router decides for them per device the same way. The failure of a write after a refused-API answer is not repeated over MQTT unless the memory already knew the device refuses the API. Once such a change was published after that answer (answered or not), or HTTP carried it to a device on TasmoFleet's broker (the channel refused it, the device was marked already, Make compliant; an answer or a timeout), HTTP carries the device's next commands, writes and reads, while HTTP is not failing: Tasmota restarts after it through a clean MQTT DISCONNECT, so the broker drops the will and the retained last will stays `Online` even when the device cannot rejoin the broker (a typo in `MqttPassword`; Tasmota 15.6.0 `support_wifi.ino` 1646-1648 and 1739-1741, `xdrv_02_9_mqtt.ino` 484-487 and 1512-1519, `restart_flag = 2`). Without it every following command, the person's corrective one included, would be published into the void and end as an unknown outcome. The mark lasts 10 minutes after the last such change (a second one extends it) and ends early only at an answer over MQTT on a path that still uses MQTT during it (a pinned job, HTTP failing): reads and the data refresh go over HTTP while it is set, so on a live device it nearly always lasts the full 10 minutes. A change the person confirmed on a device HTTP was not proven to reach sets none (MQTT may be the only way left). Which change counts is read as the device reads the name (`cmdpolicy.ResolveName`, from the pinned Tasmota catalogues): any letter case, the index read as a number (`SetOption0103`), and every SetOption synonym as its SetOption (`MqttTLS` is `SetOption103`, `MqttTLSFingerprint` is `SetOption132`). | `TestACutOffWriteNeedsAConfirmationOverMQTT`, `TestAfterAChangeOfTheMQTTConnectionHTTPCarriesTheNextCommands`, `TestTheMarkAfterAChangeOfTheMQTTConnectionNeedsHTTPAndEndsWithAnMQTTAnswer`, `TestAChangeOfTheMQTTConnectionThatHTTPCarriedMarksTheDevice`, `TestACutOffWriteUnderASetOptionSynonymIsJudgedLikeItsSetOption`, `TestCutsMQTTOffUnderEverySpellingTheDeviceRuns`, `TestEveryCatalogueSynonymIsJudgedLikeItsSetOption`, `TestAfterAChangeOfTheMQTTConnectionTheFactsSendSwitchesOverHTTP`, `TestACutOffWriteGoesWithoutAQuestionOnlyWhileHTTPIsProvenUsable`, `TestACutOffWriteAfterHTTPClosedIsRefusedDespiteAnAnswerOnRecord`, `TestACutOffWriteWithoutConsentDependsOnHTTPAnsweringNow`, `TestWritesGoOverMQTTFirstWhateverTheyCarry`, `TestAPIRefusedFailsOverOnlyForReadsOrAKnownRefusal`, `TestApplyLeavesOutWhatCouldCutAnMQTTOnlyDeviceOff` |
| T9  | The audit says which transport carried a write (`via`).                                                                                                                                                                                                                      | `TestRunOverMQTTIsAuditedAndScrubbed`, `TestRunOfAStrictlyUsableDeviceGoesOverMQTTAndIsAudited`, `TestPowerFollowsTheStrictWriteRule`, `TestPowerAuditNamesTheTransportTheRouterUsed` |
| T11 | A write goes over MQTT first when the device is usable there under the strict rule (own GroupTopic read, no other device's unread, topic exclusive, FullTopic ours, last will Online), its MQTT commands are not going unanswered (a read or a write over MQTT that got no answer marks the device for 10 minutes; the write itself is never resent) and its MQTT connection was not just changed (T10); the power toggle asks the same rule. What the write carries is no reason for HTTP: a password, a Wi-Fi name, a key, a login in the text, a change of the MQTT connection (topic, broker, login: while HTTP answers right before, T10) and a `Template` without a `CMND` key go MQTT first like `Power1 ON`, from TasmoFleet's own code and from the console or a bulk command alike, and the reply of a command cmdpolicy keeps secret is masked (T7). HTTP carries a write only when MQTT is not usable for the device, when the channel refuses it (T1, `Backlog2`/`Backlog3`: see "What still uses HTTP") or when MQTT published nothing (T2); Make compliant keeps HTTP first until #606. | `TestStrictlyUsableWritesGoOverMQTTFirst`, `TestWritesGoOverMQTTFirstWhateverTheyCarry`, `TestSimOnlyWhatMQTTCannotCarryGoesOverHTTPWhileHTTPWorks`, `TestRunSendsWhatTheAuditHidesOverMQTTFirstAndStillHidesIt`, `TestWritesThatMQTTMustNotCarryFirstGoOverHTTP`, `TestRouterWritesFollowChoose`, `TestWritesReasonSaysWhatTheRouterDoes`, `TestHTTPFirstWritesMarkerKeepsTodaysOrder`, `TestPowerFollowsTheStrictWriteRule`, `TestPowerAsksTheRoutersWriteRule`, `TestWritesGoOverMQTTFirstUnderTheStrictRule`, `TestAWriteMQTTLeftUnansweredSendsTheNextCommandsOverHTTP` |

## What still uses HTTP

HTTP carries an operation only when MQTT is not set up or not usable for the device (Choose's blockers: the bus is
down, the device is on another broker or has no topic, its topic is shared or its GroupTopic unread, its FullTopic
differs, its last will says Offline, its MQTT commands go unanswered or its MQTT connection was just changed while
HTTP works), or when the operation cannot be
done over MQTT. Each case of the second kind, with its evidence (Tasmota source lines are of v15.1.0):

| Case | Why not over MQTT | Evidence |
| ---- | ----------------- | -------- |
| `Backlog2`, `Backlog3` | Tasmota runs them "with no response": nothing is published that could confirm the command. | Tasmota `support_command.ino` 527-532 (`backlog_no_mqtt_response`); `internal/mqttcmd/correlate.go` (`parseRequest`); `TestSimOnlyWhatMQTTCannotCarryGoesOverHTTPWhileHTTPWorks` |
| Reads of `State`, a bare `Gpio`, `Gpios`, `Modules` | The reply cannot be matched: `State` answers without a `State` key, a bare `Gpio` answers under its own key only when GPIO1 is set, `Gpios` and `Modules` answer in several messages. Over MQTT each would time out and wrongly mark the device. | `transport.MQTTCannotMatch`, `internal/mqttcmd/correlate.go` (`otherKeys`); `TestRepliesMQTTCannotMatchAreReadOverHTTPFirst` |
| Commands that run other commands (`Json`, `Publish`, `Publish2`, `Rule` with a rule text, `RuleTimer`, `If`, `WebSend`, `WebQuery`, `DevGroupSend`, `LoRaCommand`, `Br`, `Script`, `UfsRun`, a `Template` with a `CMND` key, which Tasmota runs as a `Backlog` when it applies the template) and `Status 0` | Refused by the channel so that none can run a `Status 0` on TasmoFleet's behalf (T1, safety rule 2). `Publish` also answers nothing over MQTT (Tasmota `xdrv_02_9_mqtt.ino` 1591-1614, `CmndPublish` ends with `ResponseClear`), so its outcome could not be confirmed. For the others this is a safety refusal, not a limit of MQTT. | T1 tests; `TestSimOnlyWhatMQTTCannotCarryGoesOverHTTPWhileHTTPWorks` |

Not yet moved, so still HTTP first while HTTP works although MQTT could carry them: Make compliant (until #606), an
`Upgrade` or `WebGetConfig` with an argument typed in the console or sent as a bulk command (T5: over MQTT only by a
job that pinned the path), and the transfers (backup, restore, firmware update: Choose's other paths, their own
changes). `FileDownload`, `FileUpload` and `Upload` are sent by no role on either transport (T7); TasmoFleet's own
transfer code sends them.

## Residual risk

- **Low.** A broker client that spoofs a device topic can answer a query (a reply is matched by topic and by
  the command's key, not by a signature). Identity checks (MAC via Status 5, family) still run before restore
  and update.
- **Low.** A write over MQTT first that the device did not confirm in time is an unknown outcome; the person sees it
  and decides (nothing resends it). Such a write marks the device like an unanswered read, so a device whose
  replies do not reach TasmoFleet leaves at most one write unknown per 10 minutes while HTTP works; its next
  commands go over HTTP for a while.
- **Low.** `GroupTopic2` to `GroupTopic4` are not stored; a device listening on one of them is not seen as
  sharing the topic. A device with a topic that neither HTTP nor our broker reaches keeps an unread
  GroupTopic and holds back MQTT writes for the others.
- **Low.** The reply scrub masks values by their JSON key name (password, token, SSId, ...) and, since #605, the
  exact keys `KeyL`, `KeyH` (`ZbConfig`) and `MIKeys` (`MI32Key`/`MI32Keys`); a key under any other name a device
  answers with reaches the admin who sent the command, over HTTP as over MQTT, as before #605. cmdpolicy, its
  policy rules, the reply scrub and the backup diff each decide by their own rule what is secret; #809 makes it
  one rule.
- **Medium, by design.** A write over MQTT is delivered by the broker to every client subscribed to the device's
  topics, and on a usual Tasmota it is plain text on the device's leg (see "What changes compared with HTTP"):
  a password, a key or a login set over MQTT can be read there. Over HTTP the same command crosses the network in
  plain text too, with the device's web password beside it. Restrict the device topics with a broker ACL.
- **Low.** "HTTP works" means HTTP answered the router's `Status 11` right before a change of the MQTT
  connection; an answer on record does not count, unknown is not usable. Without that answer such a change needs
  the console's confirmation. HTTP that answers can still lose the device after the change for reasons TasmoFleet
  does not see (the address changes with the reconnect), and the answer does not prove by MAC that HTTP reaches the
  same device (the health poll's does not either). A person who confirms the change on a device HTTP does not reach
  takes the risk the confirmation names. Each such change costs one `Status 11` (up to the HTTP timeout, 10 s, on a
  silent device).
- **Low.** After such a change the device's next commands go over HTTP for 10 minutes (T10), also when it rejoined
  the broker at once: HTTP then carries them as before #605. Only an answer on a path that still uses MQTT (a pinned
  job, HTTP failing) ends the mark earlier. A device that cannot rejoin is marked only by the router of this
  TasmoFleet and only for those 10 minutes; after that, with its last will still `Online`, the next read or write
  over MQTT goes unanswered and marks it again (T11): one unknown outcome per 10 minutes at most. The marks live in
  memory: a restart of TasmoFleet forgets them.
- **Low.** A device with `SetOption10 1` publishes its last will `Offline` on the old topic before it answers a
  changed `Topic` or `FullTopic` (Tasmota `xdrv_02_9_mqtt.ino` 1566-1567 and 1675-1676; with the default
  `SetOption10 0` the will is cleared instead). The channel takes `Offline` as the end of the wait, so such a change
  over MQTT is reported as an unknown outcome although it ran; nothing resends it (T3).
