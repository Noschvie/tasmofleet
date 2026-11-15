#!/bin/sh
# Checks the broker ACLs of docs/mqtt-broker-acl.md against a real Mosquitto (#840): the ACL blocks of the page are
# extracted as they stand, loaded into a throwaway broker (its own temp dir and a free loopback port, never a
# running broker), and every claim of the page's table is tried with mosquitto_pub and mosquitto_sub as the
# page's users. Needs mosquitto, mosquitto_passwd, mosquitto_pub and mosquitto_sub (not part of `make check`:
# `make mqtt-acl-check`). The broker started here is stopped by its own pid only.
#
#   tools/mqtt-acl-check.sh [docs/mqtt-broker-acl.md]
set -eu

doc=${1:-docs/mqtt-broker-acl.md}
for bin in mosquitto mosquitto_passwd mosquitto_pub mosquitto_sub python3; do
	command -v "$bin" >/dev/null || {
		echo "mqtt-acl-check: $bin is not installed" >&2
		exit 2
	}
done

tmp=$(mktemp -d)
pid=""
cleanup() {
	[ -z "$pid" ] || kill "$pid" 2>/dev/null || true
	rm -rf "$tmp"
}
trap cleanup EXIT INT TERM

# block <info string>: the content of the fenced block that opens with ```<info string> in the doc
block() {
	awk -v tag="$1" '
		$0 == "```" tag { on = 1; next }
		on && $0 == "```" { exit }
		on { print }
	' "$doc"
}

block acl >"$tmp/acl-default"
block acl-strict >"$tmp/acl-strict"
[ -s "$tmp/acl-default" ] || {
	echo "mqtt-acl-check: no \`\`\`acl block in $doc" >&2
	exit 1
}
[ -s "$tmp/acl-strict" ] || {
	echo "mqtt-acl-check: no \`\`\`acl-strict block in $doc" >&2
	exit 1
}

fails=0
checks=0
ok() {
	checks=$((checks + 1))
	echo "ok    $1"
}
bad() {
	checks=$((checks + 1))
	fails=$((fails + 1))
	echo "FAIL  $1"
}

PORT=""
start_broker() { # start_broker <acl file>: the doc's rules plus one test-only user that may see everything
	[ -z "$pid" ] || {
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
		pid=""
	}
	cp "$1" "$tmp/acl"
	printf '\nuser witness\ntopic readwrite #\n' >>"$tmp/acl"
	: >"$tmp/pw"
	chmod 0600 "$tmp/pw" "$tmp/acl"
	for u in tasmofleet tasmota homeassistant other dev-1 dev-2 witness; do
		mosquitto_passwd -b "$tmp/pw" "$u" "$u-pw" 2>/dev/null
	done
	PORT=$(python3 -I -c "import socket; s = socket.socket(); s.bind(('127.0.0.1', 0)); print(s.getsockname()[1])")
	printf 'listener %s 127.0.0.1\nallow_anonymous false\npassword_file %s/pw\nacl_file %s/acl\n' "$PORT" "$tmp" "$tmp" >"$tmp/mosquitto.conf"
	mosquitto -c "$tmp/mosquitto.conf" >"$tmp/broker.log" 2>&1 &
	pid=$!
	i=0
	until mosquitto_pub -h 127.0.0.1 -p "$PORT" -u witness -P witness-pw -t ready -m x 2>/dev/null; do
		i=$((i + 1))
		[ "$i" -lt 50 ] || {
			echo "mqtt-acl-check: the broker did not start" >&2
			cat "$tmp/broker.log" >&2
			exit 1
		}
		sleep 0.1
	done
}

# delivers <publisher> <subscriber> <filter> <topic>: "yes" when what <publisher> sends to <topic> reaches
# <subscriber> subscribed to <filter>. Mosquitto answers every SUBSCRIBE as granted and filters at delivery, and drops a
# publish the ACL refuses without an answer, so delivery is the only honest measure.
delivers() {
	rcv="$tmp/rcv"
	: >"$rcv"
	mosquitto_sub -h 127.0.0.1 -p "$PORT" -u "$2" -P "$2-pw" -t "$3" -W 2 -C 1 >"$rcv" 2>/dev/null &
	sp=$!
	sleep 0.4
	mosquitto_pub -h 127.0.0.1 -p "$PORT" -u "$1" -P "$1-pw" -t "$4" -m "m-$$" 2>/dev/null || true
	wait "$sp" 2>/dev/null || true
	if grep -q "m-$$" "$rcv"; then echo yes; else echo no; fi
}

expect_flow() { # <publisher> <topic> <subscriber> <filter> <yes|no>
	got=$(delivers "$1" "$3" "$4" "$2")
	if [ "$got" = "$5" ]; then ok "$1 -> $2 reaches $3 on $4: $5"; else bad "$1 -> $2 reaches $3 on $4: want $5, got $got"; fi
}

echo "== acl block (TasmoFleet, the devices' login, Home Assistant)"
start_broker "$tmp/acl-default"
# TasmoFleet subscribes to exactly these filters (internal/mqttbus.TopicCodec.Filters, default FullTopic)
expect_flow witness tele/dev1/STATE tasmofleet 'tele/+/#' yes
expect_flow witness stat/dev1/RESULT tasmofleet 'stat/+/#' yes
expect_flow witness tasmota/discovery/AABBCCDDEEFF/config tasmofleet 'tasmota/discovery/+/config' yes
expect_flow witness cmnd/dev1/WebPassword tasmofleet 'cmnd/#' no
expect_flow witness cmnd/dev1/WebPassword tasmofleet '#' no
expect_flow tasmofleet cmnd/dev1/Power witness '#' yes
expect_flow tasmofleet cmnd/dev1/FileUpload witness '#' yes
expect_flow tasmofleet tele/dev1/STATE witness '#' no
expect_flow tasmofleet stat/dev1/RESULT witness '#' no
expect_flow witness cmnd/dev1/WebPassword tasmota 'cmnd/dev1/#' yes
expect_flow witness cmnd/tasmotas/Power tasmota 'cmnd/tasmotas/#' yes
expect_flow witness stat/dev1/RESULT tasmota 'stat/#' no
expect_flow witness tele/dev1/STATE tasmota 'tele/#' no
expect_flow witness stat/dev1/RESULT tasmota '#' no
expect_flow tasmota tele/dev1/LWT witness '#' yes
expect_flow tasmota stat/dev1/RESULT witness '#' yes
expect_flow tasmota stat/dev1/FILEDOWNLOAD witness '#' yes
expect_flow tasmota tasmota/discovery/AABBCCDDEEFF/config witness '#' yes
expect_flow tasmota cmnd/dev1/Power witness '#' no
expect_flow tasmota homeassistant/sensor/x/config witness '#' no
expect_flow witness tele/dev1/STATE homeassistant 'tele/#' yes
expect_flow witness stat/dev1/RESULT homeassistant 'stat/#' yes
expect_flow witness tasmota/discovery/AABBCCDDEEFF/config homeassistant 'tasmota/discovery/#' yes
expect_flow witness homeassistant/status homeassistant 'homeassistant/#' yes
expect_flow witness cmnd/dev1/WebPassword homeassistant 'cmnd/#' no
expect_flow witness cmnd/dev1/WebPassword homeassistant 'cmnd/dev1/#' no
expect_flow witness cmnd/dev1/WebPassword homeassistant '#' no
expect_flow homeassistant cmnd/dev1/Power tasmota 'cmnd/dev1/#' yes
expect_flow homeassistant tasmota/discovery/AABBCCDDEEFF/config witness '#' yes
expect_flow homeassistant tele/dev1/STATE witness '#' no
# what TasmoFleet sends, and what a device answers, must not reach Home Assistant's subscriptions
expect_flow tasmofleet cmnd/dev1/WebPassword homeassistant 'cmnd/#' no
expect_flow tasmofleet cmnd/dev1/FileDownload homeassistant '#' no
expect_flow tasmota stat/dev1/FILEDOWNLOAD homeassistant 'stat/#' no
expect_flow tasmota stat/dev1/FILEUPLOAD homeassistant 'stat/#' no
expect_flow tasmota stat/dev1/RESULT homeassistant 'stat/#' yes
expect_flow tasmota tele/dev1/STATE homeassistant 'tele/#' yes
# a login the page does not name has no rights at all
expect_flow witness cmnd/dev1/WebPassword other '#' no
expect_flow witness tele/dev1/STATE other 'tele/#' no
expect_flow witness cmnd/dev1/WebPassword other 'cmnd/#' no
expect_flow other cmnd/dev1/WebPassword witness '#' no
expect_flow other tele/dev1/STATE witness '#' no
# the whole path of one command: TasmoFleet to the device, the device's answer to TasmoFleet
expect_flow tasmofleet cmnd/dev1/Backlog tasmota 'cmnd/dev1/#' yes
expect_flow tasmota stat/dev1/RESULT tasmofleet 'stat/+/#' yes
expect_flow tasmota tele/dev1/LWT tasmofleet 'tele/+/#' yes
expect_flow tasmota tasmota/discovery/AABBCCDDEEFF/config tasmofleet 'tasmota/discovery/+/config' yes
# nobody but TasmoFleet and the devices' login can read a command
expect_flow tasmofleet cmnd/dev1/WebPassword other 'cmnd/#' no
expect_flow tasmofleet cmnd/dev1/WebPassword tasmota 'cmnd/dev1/#' yes

echo "== acl-strict block (one login per device)"
start_broker "$tmp/acl-strict"
expect_flow witness cmnd/dev-1/Power dev-1 'cmnd/dev-1/#' yes
expect_flow witness cmnd/dev-2/Power dev-1 'cmnd/dev-2/#' no
expect_flow witness cmnd/tasmotas/Power dev-1 'cmnd/tasmotas/#' yes
expect_flow witness cmnd/dev1/WebPassword dev-1 'cmnd/#' no
expect_flow witness cmnd/dev1/WebPassword dev-1 '#' no
expect_flow dev-1 tele/dev-1/LWT witness '#' yes
expect_flow dev-1 stat/dev-1/RESULT witness '#' yes
expect_flow dev-1 tasmota/discovery/AABBCCDDEEFF/config witness '#' yes
expect_flow dev-1 tele/dev-2/LWT witness '#' no
expect_flow dev-1 stat/dev-2/RESULT witness '#' no
expect_flow dev-1 cmnd/dev-2/Power witness '#' no
# a command to dev-2 reaches dev-2 only, and dev-1 cannot read it
expect_flow tasmofleet cmnd/dev-2/WebPassword dev-2 'cmnd/dev-2/#' yes
expect_flow tasmofleet cmnd/dev-2/WebPassword dev-1 'cmnd/dev-2/#' no
expect_flow tasmofleet cmnd/tasmotas/Power dev-1 'cmnd/tasmotas/#' yes
expect_flow witness tele/dev1/STATE tasmofleet 'tele/+/#' yes
expect_flow witness stat/dev1/RESULT tasmofleet 'stat/+/#' yes
expect_flow witness cmnd/dev1/WebPassword tasmofleet 'cmnd/#' no
expect_flow witness cmnd/dev1/WebPassword homeassistant 'cmnd/#' no
expect_flow homeassistant cmnd/dev-1/Power dev-1 'cmnd/dev-1/#' yes
expect_flow witness cmnd/dev1/WebPassword other '#' no

echo
if [ "$fails" -ne 0 ]; then
	echo "mqtt-acl-check: $fails of $checks checks FAILED ($(mosquitto -h 2>&1 | sed -n 's/^mosquitto version //p' | head -1))"
	exit 1
fi
echo "mqtt-acl-check: all $checks checks passed (mosquitto $(mosquitto -h 2>&1 | sed -n 's/^mosquitto version //p' | head -1))"
