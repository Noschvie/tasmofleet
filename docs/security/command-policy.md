# Command policy: what the console may send, and what may be logged

Scope: `internal/cmdpolicy` (pure classifier, reply scrubber), `internal/devicecmd`
(the only sender of free-form commands), `internal/web/console.go` and `power.go`.
Review findings are judged against this document. Every claim has a test.

## Model

**Decision 2026-10-21 (maintainer): "admins may do everything".** TasmoAdmin had only the
admin, and tasmofleet's console and bulk command are admin tools. The earlier allowlist
and the "forbidden for everyone" class are **superseded** by this model; the superseded
claims stay below, struck through in words, so a reviewer sees what changed and why.

- A command line is **one** command: `Name<index> <args>`. `cmdpolicy.Canonical` is
  the exact text that is judged and sent. Callers never send their original string.
  The one exception to "one command" is `Backlog`: it may hold ASCII `;`, and every
  part is judged on its own like a command sent alone.
- Classes: **read** and **operator** (operator role), **admin** (admin role only). The
  zero class and `forbidden` are never sendable; `forbidden` exists only for malformed
  lines. There is **no allowlist** and no command an admin cannot send.
- **Confirmation instead of refusal.** A destructive or lock-out command is still sent
  for an admin, but `Verdict.Confirm` names the consequence and the UI asks exactly
  once before it sends. Routine commands never get a second screen. The commands are
  `Reset`, `WifiConfig`, `Upgrade`, `Upload` and `OtaUrl` with arguments, `Wifi` with
  off or unclear arguments, `WebServer` with anything but the bare query and `2`,
  `IPAddress<n>`, `Ethernet` and `Ap` with arguments, `Module` and `Template` with
  arguments, `Restart` other than `Restart 1`, every credential command with
  arguments, and `Status 0` in every spelling (a warning). A `Backlog` asks when any
  of its parts would.
- `devicecmd.Run` is the single entry point (`cmdpolicy.Prepare`: authorise, then
  canonicalise). The web layer adds the confirmation screen but no authority.
- Power switching builds its command from validated integers (relay 1 to 8, ON, OFF,
  TOGGLE) and classifies it again before sending.
- Automated code is not affected: it never sends `Status 0` (safety rule 2), and it
  never sends firmware commands outside the OTA engine behind `fwselect.Guard`.

## Claims

| ID  | Claim | Tests |
| --- | ----- | ----- |
| P1  | A line that could make the device run a second command or hides what is sent (`;` outside a `Backlog`, look-alike semicolons, `%3B`, control, invisible, bidirectional and non-UTF-8 characters, over-long lines, bad names) is malformed: `ErrInvalid`, never sent. The same checks apply to every part of a `Backlog`. | `TestClassifyRejectsInjection`, `FuzzPolicy` |
| P2  | **Superseded 2026-10-21 (admins may do everything).** Was: command executors (`Backlog`, `Json`, `Rule`, `RuleTimer`, `If`, `Event`, `Publish`, `Publish2`, `WebSend`, `WebQuery`, `WebGetConfig`, `UfsRun`, `Script`, `Br`, `SendMail`, `Var`, `Mem`, `DevGroupSend`, `Telnet`, `LoRaCommand`, `SetOption85` with an argument, `Timer`, `Timers`, `Subscribe`, `Unsubscribe`, `PublishRetain`) forbidden for every role. Now they are plain admin commands. Only a `Backlog` is looked into, so a `Reset` inside it asks like a `Reset` alone; the other executors are not inspected. | `TestClassify`, `TestBacklogInheritsConfirmations` |
| P3  | **Superseded 2026-10-21.** Was: `Reset`, `WifiConfig`, `Upgrade`, `Upload`, `OtaUrl`, `Restart` other than `Restart 1`, `Wifi` off, `WebServer` other than the bare query and `2`, `IPAddress<n>`, `Ethernet`, `Ap` and `Status 0` forbidden. Now each of them is an admin command with one confirmation naming the consequence (`Confirm`); `Status 0` is allowed with a warning on that step. | `TestClassify`, `TestBoundaries`, `FuzzPolicy`, `TestEveryFixtureCommandIsClassified` |
| P4  | **Superseded 2026-10-21.** Was: credential commands forbidden in the console. Now `Password<n>`, `WebPassword`, `MqttPassword`, `SSId<n>`, `TmToken`, `IfxPassword`, `IfxToken`, `IfxUser`, `TLSKey<n>` and `LoRaWanAppKey<n>` are admin commands with a confirmation; the verdict is marked `Redact`, and their arguments never reach any log (L1). | `TestClassify`, `TestConsoleCredentialCommandsAreConfirmedAndRedacted` |
| P8  | **Superseded 2026-10-21.** Was: an allowlist; unknown names (`Sensor13`, `SetSensor`, driver and Zigbee commands) were refused with `ErrUnknown`. Now a name nobody listed is an admin command; an operator is refused it with `ErrRole`. | `TestAnyNameIsAdminOnly` |
| P5  | An operator can send only read and operator class commands: `Status 1..11`, queries without arguments, `Power<1..8> ON/OFF/TOGGLE`, `Restart 1`. `Power0`, relays above 8, bare `SaveData` (writes flash), `MqttUser`, every unlisted name and every confirmed command need admin. | `TestAuthorize`, `TestClassify`, `FuzzPolicy`, `TestRunPowerVariantsPerRole` |
| P6  | The role check happens in `devicecmd.Run`, before any network I/O, for every caller; roles below operator, above admin or unknown get nothing. Refusals are audited (`command.rejected`). | `TestRunRefusesBeforeNetworkIO`, `FuzzPolicy` |
| P7  | What is classified is what is sent: `Classify(Canonical(x))` equals `Classify(x)` and the device receives the canonical text. | `TestCanonicalIsWhatIsJudged`, `TestRunSendsTheCanonicalCommand`, `FuzzPolicy` |
| L1  | The audit log, the console history and error texts hold only `cmdpolicy.Redacted(cmd)`: an **allowlist of arguments**. Arguments appear only for operator-level commands and a short list of harmless settings (credential patterns scrubbed). Every other command, refused ones included, shows as `Name ***`, a malformed line as `[rejected: malformed command]`. A `Backlog` is shown part by part, each part redacted on its own. A secret wrapped in `Json`, `Rule`, `Publish` never reaches a log. | `TestRedactedForLog`, `TestRedactedScrubsArguments`, `TestSecretsNeverReachTheAuditLog`, `TestErrorsNeverEchoTheCommand`, `FuzzPolicy` |
| L2  | Device replies are shown only after `cmdpolicy.ScrubReply`: values of secret-looking keys (`password`, `token`, `secret`, `apikey`, `MqttUser`) and credential patterns in strings (URL userinfo, `[user:pass@host]`, `password=`) are masked; key order and numbers are kept. Replies are never written to the audit log. | `TestScrubReply`, `TestRunScrubsCredentialsFromReplies` |
| L3  | The console page never repeats text that the policy would not log: the input box and the confirmation step carry only loggable text (the command shown there is `Redacted`). **The "type the command again" step is superseded (2026-10-21):** the confirmation step names the consequence and posts the canonical command from the server-side session of the form, so nothing has to be retyped and no secret is echoed. Replies are HTML-escaped. | `TestConsoleConfirmDoesNotEchoUnknownArguments`, `TestConsoleReplyIsEscaped`, `TestConsoleCredentialCommandsAreConfirmedAndRedacted` |
| L4  | Power fragments pushed over SSE carry relay states only, escaped, no command text. | `TestPowerStateIsEscaped`, `TestSSEPublishesPowerFragment` |
| D1  | After the legacy "web log level too low" warning the HTTP client raises the level and repeats **only read-class commands**. A `Power TOGGLE` or `Restart 1` that the device already applied is never sent twice; the caller gets an error saying it was applied. | `TestClient_WebLogWarningNeverRepeatsStateChangingCommands`, `TestClient_WebLogQuirk` |

**Device settings editor, flash confirmation: superseded (2026-10-20, proportionate security).** The earlier rule asked for a required "flash wears out" confirmation and an optional "write to flash now" box. The page is admin-only, CSRF-protected and audited; the confirmation protected nothing the admin role does not already allow. Applying changes now always ends with exactly one `SaveData` write (`TestSettingsAlwaysSavesOnce`), so the result also holds with SaveData 0.

## Bulk send (2026-10-21)

`internal/web/bulkcommand.go` and `ota.Engine.SubmitCommand` send one command line to
several devices, admin only. The same `cmdpolicy` verdict, confirmation texts and
redaction as the console apply.

| ID  | Claim | Tests |
| --- | ----- | ----- |
| B1  | Admin only. The preview (targets, offline devices skipped, consequence) is the confirmation; the run takes command and devices from a single-use server-held token bound to the session, never from the form. Credential commands (`Redact`) are refused for bulk because the stored job would hold the value. | `TestBulkCommandPreviewThenRun`, `TestBulkCommandPreviewNamesTheConsequence`, `TestBulkCommandRefusals` |
| B2  | The job is not an OTA job: queued, applying, succeeded or failed with the compact reply as note. Before each step the creator is resolved again (role, disabled, gone) by the pair (sign-in method, subject), which the job stores (`created_kind`): subjects are not unique across methods. A local account only under the reserved `local:` prefix, which header and SSO users never resolve under; an SSO or header user only through a principal record written by the same method; unknown method, other method's record, no record or no role fails closed. anything but an active admin fails the step and is audited (`command.bulk.refused`). The command policy checks the role once more inside `devicecmd.Run`. A job found mid-send after a restart fails instead of being sent again. | `TestCommandStepFailsWhenTheCreatorIsDemotedDisabledOrGone`, `TestResolverSeparatesSignInMethodsThatShareASubject`, `TestResolverFailsClosed`, `TestCommandStepWithoutASignInMethodFails`, `TestCommandJobFoundSendingIsNotSentAgain`, `TestCommandJobSendsOnceAndKeepsTheReply` |
| B3  | A retry is admin-only (web and service), runs as the retrying admin, passes `Authorize` for every stored command, and asks for a new confirmation when a stored command has a consequence. | `TestCommandRetryRunsAsTheRetryingAdminOnly`, `TestCommandRetryChecksEveryStoredCommandAgainstThePolicy`, `TestCommandTransactionRetryNeedsAnAdminAndANewConfirmation` |
| B4  | Transaction pages show only `Redacted(command)`. | `TestCommandTransactionShowsOnlyTheRedactedCommand` |

Residual: the role of a single-sign-on or header user is the one in their principal
record (written at sign-in; for header users at most every few minutes), so a user
demoted at the identity provider is caught at their next sign-in. The principals
table has one row per subject: two methods using one subject overwrite each other's
record, and a job of the older one then fails closed.

## Maintenance

- **Rule (superseded 2026-10-21).** There is no `forbiddenNames` table and no
  allowlist any more. A command that destroys, strands or locks out a device gets a
  consequence text in `confirmFor`; everything else is a plain admin command.
- **Test-backed.** `TestDocListsEveryConfirmedName` fails when a name in `confirmFor` or
  `secretNames` is missing from the claims table above, so this document cannot fall
  behind the code. Each such name also needs a row in `TestClassify`.
- `FuzzPolicy` runs as a seed-corpus test in `make test`. Run it for a minute after
  any change to `cmdpolicy.go`: `go test ./internal/cmdpolicy -run xxx -fuzz FuzzPolicy -fuzztime 60s`.
- Mutation gate: `tools/mutation-gate.sh ./internal/cmdpolicy 90` (also in `make mutation`).
