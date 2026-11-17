# Broker ACL for TasmoFleet

> [!CAUTION]
> **Without an access control list (ACL) on your MQTT broker, every client of the broker can read the commands
> TasmoFleet sends, the passwords in them, and the complete configuration dump of every device it backs up or
> restores.** On a usual Tasmota both MQTT and HTTP are unencrypted, but over MQTT the _broker_ hands every command,
> and the device's reply, to every client that may subscribe to the device's topics: Home Assistant, Node-RED, MQTT
> Explorer, a logger, any other device with the same login. An ACL that lets only TasmoFleet and the devices read
> them is what keeps those clients out. Set it up before you use TasmoFleet's MQTT features. This page has a ready
> ACL and a way to check it.

Since 0.37.0 MQTT is the first way TasmoFleet talks to a device that is online on its broker: reads, changes made in
the console, in settings and in bulk, Make compliant, backups and restores. HTTP is used only when MQTT is not set up
or cannot carry the operation ([what uses HTTP](security/mqtt-transport.md)). Secrets are no reason to prefer HTTP:
HTTP to a Tasmota is plain too, and carries the web password in every request. What differs is who else can read it:
over HTTP whoever can watch the device network, over MQTT in addition every client the broker lets subscribe to the
device's topics.

## What crosses the broker

| What                                                                            | Topic (default FullTopic)                                                 | Carries                                                                                                                        |
| ------------------------------------------------------------------------------- | ------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| A command or setting TasmoFleet sends (console, settings, bulk, Make compliant) | `cmnd/<topic>/<Command>`                                                  | The command, so also `WebPassword`, `Password1`, `SSId1`, `MqttPassword`, `OtaUrl` logins                                      |
| The device's answer and its state                                               | `stat/<topic>/...`, `tele/<topic>/...`                                    | Replies, power state, sensor values, firmware and network facts                                                                |
| A backup (`FileDownload`)                                                       | start on `cmnd/<topic>/FileDownload`, dump on `stat/<topic>/FILEDOWNLOAD` | The device's MQTT password in the start command, then **the whole settings dump**: Wi-Fi, web and MQTT passwords, tokens, keys. On a firmware built with `USE_UFILESYS_MQTT` (#924) also **every file** on the same topics, each after a start with the MQTT password: Berry scripts with their tokens, the TLS private key (`/tlskey`), Zigbee data |
| A restore (`FileUpload`)                                                        | `cmnd/<topic>/FileUpload`                                                 | The device's MQTT password and the whole dump in chunks                                                                        |
| Discovery                                                                       | `tasmota/discovery/<MAC>/config`                                          | Names, topics, MAC, firmware (retained)                                                                                        |

Logs, the audit log and the UI of TasmoFleet mask these secrets; the broker does not.

## Who needs which topics

One login per kind of client. Everything a client does not need stays closed.

| Login               | Reads                                                        | Writes                                             | Why                                                                                                                                                                                                                                                                                                                       |
| ------------------- | ------------------------------------------------------------ | -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| TasmoFleet          | `stat/#`, `tele/#`, `tasmota/discovery/+/config`             | `cmnd/#`                                           | It subscribes to exactly `tele/+/#`, `stat/+/#` and `tasmota/discovery/+/config` and publishes commands to `cmnd/<topic>/<Command>` (a test in the repository checks this against the code). It never reads `cmnd/`, so it does not need to.                                                                              |
| The Tasmota devices | `cmnd/#`                                                     | `stat/#`, `tele/#`, `tasmota/discovery/#`          | Tasmota subscribes to `cmnd/<topic>/#`, to its group topics and to its fallback topic, and publishes `tele/`, `stat/` and its discovery messages (Tasmota 15.1.0, `xdrv_02_9_mqtt.ino`, `xdrv_12_discovery.ino`).                                                                                                         |
| Home Assistant      | `tele/#`, `stat/#`, `tasmota/discovery/#`, `homeassistant/#` | `cmnd/#`, `tasmota/discovery/#`, `homeassistant/#` | Its Tasmota integration (the `hatasmota` library 0.10.2) subscribes to `<discovery prefix>/#`, to `tele/<topic>/{LWT,STATE,SENSOR}` and `stat/<topic>/{RESULT,STATUS,STATUS<n>,BUTTON<n>,SWITCH<n>}`, and publishes commands to `cmnd/<topic>/...`; it also clears retained discovery messages. It does not read `cmnd/`. |
| Everyone else       | nothing                                                      | nothing                                            | A login the file does not name has no rights at all.                                                                                                                                                                                                                                                                      |

The ACL below also keeps Home Assistant away from the two topics that carry a transfer, `stat/+/FILEDOWNLOAD` and
`stat/+/FILEUPLOAD`, although it may read the rest of `stat/`. A device also sends one empty message to its own
`cmnd/<topic>/POWER` after it connects (an old courtesy to ioBroker, `xdrv_02_9_mqtt.ino`); the ACL drops it, which
does no harm. Add `topic write cmnd/+/POWER` to the devices' block if you run ioBroker.

Tasmota repeats some arguments in its answer on `stat/<topic>/RESULT`, and Home Assistant reads that topic (its Tasmota
integration needs it), so the ACL does not keep these answers from it: an OtaUrl with a login in it, SSId, rule text,
and a new password set with `WebPassword`, `Password1`/`Password2` or `MqttPassword`, which Tasmota answers in clear.
Tasmota masks the answer only for `WebPassword2`, `Password3`/`Password4` and `MqttPassword2` (same effect, answer
`****`), so type those spellings in the console or a bulk command (Tasmota 16.0.0 source: `CmndWebPassword`,
`CmndPassword`, `CmndMqttPassword`).

## The ACL

The file below is what `make mqtt-acl-check` loads into a throwaway Mosquitto and tries line by line
(`tools/mqtt-acl-check.sh`). It uses the default topic layout (`cmnd`, `stat`, `tele`, FullTopic `%prefix%/%topic%/`).
With another layout, change the topic filters the same way.

<!-- The block below is read by tools/mqtt-acl-check.sh (the info string "acl") and by a test in internal/mqttbus. -->

```acl
# TasmoFleet: sends commands, reads what the devices answer and report.
user tasmofleet
topic write cmnd/#
topic read stat/#
topic read tele/#
topic read tasmota/discovery/+/config

# The Tasmota devices (all of them with one login): read their commands, report.
user tasmota
topic read cmnd/#
topic write stat/#
topic write tele/#
topic write tasmota/discovery/#

# Home Assistant: sends commands, reads state and discovery, never a command.
user homeassistant
topic write cmnd/#
topic read stat/#
topic read tele/#
topic deny stat/+/FILEDOWNLOAD
topic deny stat/+/FILEUPLOAD
topic readwrite tasmota/discovery/#
topic readwrite homeassistant/#
```

What each line does, and what happens without it:

- `user tasmofleet`: the rules below it belong to this login. A login that no `user` line names gets no rights.
- `topic write cmnd/#`: TasmoFleet publishes every command to `cmnd/<topic>/<Command>`. Without it nothing can be changed over MQTT; TasmoFleet then falls back to HTTP where it can.
- `topic read stat/#` and `topic read tele/#`: the device's answers (`stat/<topic>/RESULT`, `FILEDOWNLOAD`, `STATUS<n>`) and its state, online marker (`tele/<topic>/LWT`) and telemetry. Without them reads, backups and the device list over MQTT stop working.
- `topic read tasmota/discovery/+/config`: the retained discovery message of each device, which is how TasmoFleet finds new devices.
- `user tasmota` with `topic read cmnd/#`: each device subscribes to its own `cmnd/<topic>/#`, its group topics and its fallback topic. All devices share this login here, so each can read the commands of the others (see the stricter variant below).
- `topic write stat/#`, `topic write tele/#`: answers and telemetry. `topic write tasmota/discovery/#`: the retained discovery messages (`config`, `sensors`).
- `user homeassistant`: the internal login of the Mosquitto app. `topic write cmnd/#` lets it switch things; there is deliberately no `topic read cmnd/#`.
- `topic read stat/#`, `topic read tele/#`, `topic readwrite tasmota/discovery/#`: what the Tasmota integration subscribes to, and the discovery messages it clears.
- `topic deny stat/+/FILEDOWNLOAD` and `FILEUPLOAD`: a `deny` beats every other rule of the login, so Home Assistant does not receive the dump of a backup although it reads the rest of `stat/`.
- `topic readwrite homeassistant/#`: Home Assistant's own MQTT discovery and its birth and will message (`homeassistant/status`). Add a line for every other thing that talks to the broker through this login (for example `topic readwrite zigbee2mqtt/#`); a topic the file does not allow is silently refused.

Passwords are not in this file. Give each login its own strong password.

### Check that it works

With `mosquitto_sub` and `mosquitto_pub` from any computer that reaches the broker. Replace `BROKER`. Mosquitto
answers a subscription to a topic the login may not read as _granted_ and simply never delivers anything, so the check
is whether a message arrives, not what the subscribe answer says.

1. In one terminal listen as a login that must not see commands (Home Assistant's login, then once more as a login
   the file does not name, or a Home Assistant person account):

   ```sh
   mosquitto_sub -h BROKER -u homeassistant -P '<its password>' -t '#' -v -W 20
   ```

2. In a second terminal send a command as TasmoFleet's login, and a device's answer as the devices' login:

   ```sh
   mosquitto_pub -h BROKER -u tasmofleet -P '<its password>' -t 'cmnd/test-device/WebPassword' -m 'secret-check'
   mosquitto_pub -h BROKER -u tasmota -P '<its password>' -t 'stat/test-device/FILEDOWNLOAD' -m 'dump-check'
   ```

3. The listener must print **nothing** for either message. If it prints `cmnd/test-device/WebPassword secret-check`
   or `stat/test-device/FILEDOWNLOAD dump-check`, that login can read your commands and backups: the ACL is not in force.
4. As a last step make sure the way TasmoFleet works is open: listen as `tasmofleet` on `stat/+/#` and publish
   `stat/test-device/RESULT` as `tasmota`; this one must arrive.

`make mqtt-acl-check` does all of this, and more, against a throwaway Mosquitto with the ACL blocks of this page.

## Mosquitto app in Home Assistant

> [!CAUTION]
> **The ACL does not work in the Mosquitto app 7.x at the moment.** Checked with app 7.1.3 (Mosquitto 2.1.2, with the
> documented `customize` recipe): the files load, the log says so, and a login with no rights still subscribes to `#`
> and reads every command. Even `topic deny #` for it changes nothing. The app's authentication plugin answers every
> permission check with "allowed" before Mosquitto's own `acl_file` is asked. This is a known problem of the app since
> 7.0.0 ([app-mosquitto #5](https://github.com/home-assistant/app-mosquitto/issues/5); a fix is proposed in
> [app-mosquitto #6](https://github.com/home-assistant/app-mosquitto/pull/6), open on 2026-11-15). Until a fixed
> version is installed, **a broker run by this app has no ACL**, whatever files you add. Run the check above after
> every app update; it is the only way to know.
> What you can do now: use another broker for the devices (the plain Mosquitto section below, or a second
> Mosquitto container beside Home Assistant), or an app version from before 7.0 (reported to enforce the ACL, not tried
> here).

The steps below are the ones the app documents ([DOCS.md of the app](https://github.com/home-assistant/app-mosquitto/blob/main/mosquitto/DOCS.md)).
They are correct for a version that enforces `acl_file`.

1. **Logins.** In the app's _Configuration_ add a login for the devices and one for TasmoFleet (the app names Home
   Assistant's own users `homeassistant` and `addons`; they are reserved and exist without configuration):

   ```yaml
   logins:
     - username: tasmofleet
       password: <a long password>
     - username: tasmota
       password: <another long password>
   ```

   A Home Assistant person account (Settings, People) can log in to the broker with its Home Assistant password too.
   The ACL gives such a login nothing unless you name it in the file.

2. **Enable `customize`** in the same configuration, then save and restart the app:

   ```yaml
   customize:
     active: true
     folder: mosquitto
   ```

3. **Two files in the `share` folder** (Samba share `share`, or `/usr/share/hassio/share` on the host), in a folder named like `customize.folder`:
   - `share/mosquitto/acl.conf`, one line:

     ```text
     acl_file /share/mosquitto/accesscontrollist
     ```

   - `share/mosquitto/accesscontrollist`: the ACL from above. The app's documentation says Home Assistant expects
     `homeassistant` and `addons` to have access to every topic (`topic readwrite #`) and that you run into problems
     otherwise. The ACL above restricts `homeassistant` to what the Tasmota integration needs; if something else breaks,
     add its topics, or give it `topic readwrite #` with the two `deny` lines kept (the deny lines still keep the
     transfers away from it, the commands not). Apps that take their MQTT login from the Supervisor (Zigbee2MQTT,
     Node-RED and others) connect as `addons`; add `user addons` and the topics those apps need, or `topic readwrite #`
     if you trust them with everything, commands included.

4. Restart the app, read its log (a line `Warning: File ... has world readable permissions` means set the file to
   mode 0600), then run the check above.

5. **Set the login on TasmoFleet and the devices.** System, MQTT in TasmoFleet: broker host, the `tasmofleet` user and
   its password. On every Tasmota: Configuration, Configure MQTT, user `tasmota` and its password (or in the console
   `Backlog MqttUser tasmota; MqttPassword <password>`). Change them device by device and keep a device's web page
   reachable until it is back on the broker: a device with a wrong login cannot be reached over MQTT.

### Stricter: one login per device

With one login for all devices every device can read the commands (and so the secrets) sent to the others, and a
stolen device login can read and write every `stat` and `tele` topic. Worth it when the devices are not all yours or
live on a network you do not control; it costs one login and three lines per device. The login name must be the
device's **Topic**; the `pattern` lines apply `%u` (the login's name) to every login:

<!-- Read by tools/mqtt-acl-check.sh (the info string "acl-strict"). -->

```acl-strict
# TasmoFleet and Home Assistant: as above.
user tasmofleet
topic write cmnd/#
topic read stat/#
topic read tele/#
topic read tasmota/discovery/+/config

user homeassistant
topic write cmnd/#
topic read stat/#
topic read tele/#
topic deny stat/+/FILEDOWNLOAD
topic deny stat/+/FILEUPLOAD
topic readwrite tasmota/discovery/#
topic readwrite homeassistant/#

# Every login, for its own name only: its commands in, its answers and telemetry out.
pattern read cmnd/%u/#
pattern write stat/%u/#
pattern write tele/%u/#

# One block per device (dev-1 is the device's Topic, MqttUser dev-1): discovery and the default group topic.
user dev-1
topic write tasmota/discovery/#
topic read cmnd/tasmotas/#

user dev-2
topic write tasmota/discovery/#
topic read cmnd/tasmotas/#
```

Limits: a device may still write any discovery message (Mosquitto cannot tie a MAC to a login); the fallback topic of a
device (`cmnd/DVES_xxxxxx_fb/`) is not covered; `%u` must not contain MQTT wildcards, so give logins plain names (the
app 7.x sets `auth_plugin_deny_special_chars false`, check the name you choose).

## Plain Mosquitto

Not the Home Assistant app: a package or container. `mosquitto.conf`:

```text
listener 1883
allow_anonymous false
password_file /etc/mosquitto/passwd
acl_file /etc/mosquitto/acl
```

```sh
mosquitto_passwd -c /etc/mosquitto/passwd tasmofleet   # asks for the password; without -c for the next logins
mosquitto_passwd /etc/mosquitto/passwd tasmota
mosquitto_passwd /etc/mosquitto/passwd homeassistant
```

Put the ACL from above in `/etc/mosquitto/acl`, restart, and run the check. The ACL file is read at start; send
`SIGHUP` to reload it. Home Assistant's MQTT integration then uses the `homeassistant` login you created. Verified with
Mosquitto 2.1.2: `make mqtt-acl-check` (below).

## EMQX

Not tried; the rules are the same, in EMQX's own syntax. In EMQX 5 the file-based authorization source
(`etc/acl.conf`) takes one rule per line, `{allow | deny, who, publish | subscribe | all, [topics]}.`; rules are
checked top to bottom and the first match decides, so end the file with `{deny, all}.`. In outline, for the
logins above (check the syntax against the documentation of your EMQX version before use):

```text
{allow, {username, "tasmofleet"}, publish, ["cmnd/#"]}.
{allow, {username, "tasmofleet"}, subscribe, ["stat/#", "tele/#", "tasmota/discovery/+/config"]}.
{allow, {username, "tasmota"}, subscribe, ["cmnd/#"]}.
{allow, {username, "tasmota"}, publish, ["stat/#", "tele/#", "tasmota/discovery/#"]}.
{deny, {username, "homeassistant"}, all, ["stat/+/FILEDOWNLOAD", "stat/+/FILEUPLOAD"]}.
{allow, {username, "homeassistant"}, publish, ["cmnd/#", "tasmota/discovery/#", "homeassistant/#"]}.
{allow, {username, "homeassistant"}, subscribe, ["stat/#", "tele/#", "tasmota/discovery/#", "homeassistant/#"]}.
{deny, all}.
```

## What was checked, and what was not

Checked on 2026-11-15:

- **The ACL blocks on a real Mosquitto 2.1.2** (the version the Mosquitto app ships): `tools/mqtt-acl-check.sh`
  (`make mqtt-acl-check`) extracts the `acl` and `acl-strict` blocks of this page, starts a throwaway broker on a free
  loopback port and tries every line of the tables above with `mosquitto_pub` and `mosquitto_sub`: what each login may
  subscribe to, which messages reach whom, and that a login without rights and Home Assistant never receive a command or
  a transfer. It needs `mosquitto` installed and is not part of `make check`.
- **TasmoFleet's own topics** are read from the code: a test (`internal/mqttbus`) fails when the `tasmofleet` rules of
  the first block no longer cover the filters TasmoFleet subscribes to or the topics it publishes to.
- **The Mosquitto app 7.1.3**: its published container image, started with its own start scripts and the configuration
  above, against a stand-in for the Home Assistant Supervisor (login check only; no Home Assistant). The customize
  recipe loads and is not enforced (see the caution above); logins and a Home Assistant person account could log in.
- **Tasmota 15.1.0** (source: what a device subscribes to and publishes) and **`hatasmota` 0.10.2** (what Home
  Assistant's Tasmota integration subscribes to and publishes).
- **Which password commands Tasmota answers in clear** (`WebPassword`, `Password1`/`Password2`, `MqttPassword`) and
  which it masks: read in the Tasmota 16.0.0 source (`CmndWebPassword`, `CmndPassword`, `CmndMqttPassword`), not tried
  on a device.

Not checked: a real Home Assistant installation with the app (no Supervisor here), the app before 7.0, a fixed app
(none is released), EMQX, a real Tasmota device or TasmoFleet connecting to a restricted broker, and the per-device
variant on a device. Statements about them above say so.
