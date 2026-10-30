# Tasmota command catalogue

Over MQTT the reply to a command arrives on `stat/<topic>/RESULT`, mixed with replies to other
pollers and button presses. To tell the reply from the noise, TasmoFleet needs to know which
top-level key of the JSON answers which command. `internal/tasmotacmd` holds that knowledge as
data generated from the Tasmota source, not as a hand-written table (#268, part of #79).

## What is in the package

- `catalogue-16.0.0.json`: one entry per command-table entry of Tasmota v16.0.0, embedded.
- `tasmotacmd.ReplyKeys(cmd) ([]string, ok)`: the keys that answer `cmd`. `cmd` is the command as sent
  (`OtaUrl`, `SetOption19`, `Power1 on`); arguments are ignored, matching is case-insensitive, trailing
  digits are the index. `<n>` in a stored key becomes the typed index (1 when none is typed, as in
  Tasmota). `ok` is false when the command is unknown or loose.
- `tasmotacmd.Lookup(cmd)` returns the same plus the `Loose` reason, so a caller can show why.
- `internal/tasmotacmd/extract` and `cmd/tasmofleet-catalogue`: the generator.

A command is **loose** when its reply keys cannot be derived statically. `ReplyKeys` then refuses
(`ok == false`); the `keys` stored in the JSON for a loose command are only what the reader found
and must not be used. Unknown commands (newer firmware, typos) are "not known", which is not the
same as loose.

## Entry fields

| Field        | Meaning                                                                                                                    |
| ------------ | -------------------------------------------------------------------------------------------------------------------------- |
| `command`    | The name as typed: table prefix + entry (`MqttHost`, not `Host`). An empty entry is the prefix alone (`InfluxDb`).        |
| `table`, `file`, `handler` | Where it was found; several handlers (comma separated) when #if/#else give a name different handlers.      |
| `synonym_of` | SetOption synonyms (`WifiNoSleep` is `SetOption127`).                                                                      |
| `requires`   | The `#if` conditions around the entry, as text, not evaluated (`!FIRMWARE_MINIMAL`, `USE_I2C`, ...).                       |
| `index`      | `min`/`max` the handler checks `XdrvMailbox.index` against, as source text (a number, a macro or a run-time expression). |
| `keys`       | Top-level reply keys. `<n>` stands for the command's index.                                                                |
| `kind`       | `number`, `text`, `state`, `array`, `object` or `mixed`; empty when the value is not a literal.                            |
| `indexed`    | Reply when an index is typed (`SetOption19`), only when the handler answers it differently from the bare command.          |
| `via`        | Functions the handler calls that build the reply.                                                                          |
| `loose`      | The reason the keys are not derivable.                                                                                     |

## How it is extracted

The extractor is a static reader of C/C++ text, not a compiler. It does not evaluate the
preprocessor, types or control flow.

1. Every `.ino`/`.h` file of Tasmota's `tasmota/` directory is read (Berry, LVGL, HTML assets and all
   languages except `en_GB.h` are skipped). Comments are removed, `#define NAME "text"` and
   `const char X[] PROGMEM = "..."` are collected, and every line remembers the `#if` conditions around it.
   Macros are resolved from the file itself first, then from the shared files (`include/`,
   `tasmota_support/`, `language/en_GB.h`). A macro with two different definitions is ambiguous, never guessed.
2. Command tables are found through the `DecodeCommand(kXxxCommands, XxxCommand[, kXxxSynonyms])` calls: the name
   table (a concatenated string split at `|`, first segment is the prefix) and the handler table
   (`void (* const XxxCommand[])(void)`) are paired by position. Under `#if/#else` both lists contain the
   alternatives of every branch, so entries are paired per configuration (every combination of the
   conditions that differ); a name keeps all handlers it is paired with. If the two lists never have the same
   length (upstream's Stcc and ZCDimmer tables do not), the entries are loose and no pairing is guessed.
3. A handler is read together with the functions it calls that build replies (to depth 6). Recognised:
   `ResponseCmndNumber/Float/Char/Char_P/StateText/Done/Error/Failed`, the `Idx` variants and `ResponseCmndAll`
   (key = command name, with `<n>` for the Idx forms), `ResponseCmnd()`, and `Response_P` / `ResponseAppend_P`
   with a literal format (also through macros such as `S_JSON_COMMAND_NVALUE`). The format is reconstructed
   with `%s` filled from `XdrvMailbox.command` or a string literal, and its top-level JSON keys are read.
4. Idioms that change the key are followed: `snprintf_P(XdrvMailbox.command, ..., "SetOption")` (renames, so `SO19` is
   answered as `SetOption19`), `strlcat(XdrvMailbox.command, "2")` (an extra key `Module2`), `if (!XdrvMailbox.usridx)` (the
   reply differs with and without a typed index; stored as `keys` and `indexed`), and `if (indexed)` on a literal
   argument (`CmndSetoptionBase(0)` for synonyms against `(1)` for `SetOption`).

## Why a command is loose

The reason is stored in each entry. The classes, with the counts of v16.0.0 (1077 entries, 954 distinct names,
176 entries loose):

| Reason                                                                                              | Entries |
| --------------------------------------------------------------------------------------------------- | ------: |
| keys added by helper functions through `ResponseAppend` are not read (Status, Energy margins, ...)  |      46 |
| a key ends in a number chosen at run time (`GPIO0`, `Timer1`, ...)                                  |      32 |
| the reply is cleared and published elsewhere (`Power`, `Backlog`, `Time`)                     |      19 |
| no reply call reachable (the handler dispatches through `XdrvCall` or a function pointer)           |      19 |
| the reply format is not a literal (variable or ambiguous macro)                                     |      17 |
| the reply text is not a literal JSON object                                                         |      15 |
| a key is built at run time                                                                          |       9 |
| name table and handler table differ in length (upstream inconsistency)                              |       7 |
| other (a reply with literal keys goes out with `MqttPublish*`, append-only replies, ...)            |      12 |

Typical loose commands: `Power<n>` (the state publisher answers, not the handler), `Backlog` (the queued commands
answer), `Status N` (answers on `stat/<topic>/STATUS<N>`, not RESULT), `Gpio` (`GPIO0`, `GPIO1`, ... depend on
the template), `Timer<n>`.

## What the reader cannot see (limits)

- Control flow. Keys from every branch are joined, so a derived key list is "any of these"; conditions
  inside a handler are ignored except the three idioms above. A foreign key in a RESULT is still ignored,
  but a reply from a branch that sets no key at all (a silent error path) is not detected.
- Build configuration. `requires` is text; whether a device has the driver is decided by its firmware.
- Replies built by helpers that only `ResponseAppend` (no `Response_P` of their own) are not read; the
  command is loose when a chain depends on one.
- Commands outside the `DecodeCommand` tables (Berry, `Script_SubCmd`, rules, scripts) are not covered.
- A handler that Tasmota overrides at run time (`Shortcut`, `SetOption` renames not of the three kinds above) is
  read as written.
- `Command`: Unknown and `Command`: Error replies are not listed; every command can produce them. The
  consumer must treat the `Command` key as an answer to whatever it just sent.
- Source files that appear twice (shutter, webcam, ws2812 variants) give one entry per file; `Lookup` joins
  their keys, and one loose variant makes the name loose.

## Things found on the way

- `WebPassword` and `MqttPassword` echo the new secret in their reply when it is set, unless the index is 2
  (`WebPassword2 x` answers `****`); a read without a value always answers `****`. Never log or store the RESULT
  of a set of either command.
- `Template` does not answer `{"Template":...}` but `{"NAME":..,"ARCH":..,"GPIO":[..],"FLAG":..,"BASE":..}`;
  `Template` is also a key, but only for the invalid-JSON error.
- `Module2` is answered as `Module2`.

## Regenerating

```sh
make catalogue          # fetches the pinned tarball once, verifies its sha256, regenerates
make catalogue-check    # FULL tier: fails when the checked-in file differs from a regeneration
```

The tarball (`TASMOTA_VERSION` / `TASMOTA_SHA256` in the Makefile) is unpacked under
`~/.cache/tasmofleet/` (`TASMOTA_SRC` overrides it), never into the repository; only `make catalogue-src`
touches the network. To pin another Tasmota version, change the three Makefile variables, run
`make catalogue`, rename the embed in `internal/tasmotacmd/lookup.go`, and read the diff: a changed
key or a newly loose command shows what upstream changed.
