#!/bin/sh
# Race-detector run of the given packages (default ./...), what "make race" does.
#
# CI (FULL tier) runs it in two shards: RACE_SHARD=1/2 tests only the packages listed in HEAVY, RACE_SHARD=2/2 all
# the others, so a new package lands in shard 2 on its own. HEAVY is dealt out by the package times of the nightly
# run 41493544951 (2026-11-02, seconds of one package on a two-core runner, two packages at a time): store 291,
# discovery 229, cmd/tasmofleet 68, auth 18 against app 217, restore 182, web 53, mqttbus 36, devenv 33 and
# the small rest, about 620 s each. internal/auth used to be run alone in two parts by test name (its PBKDF2 made
# it half of all race time); with the test-only iteration count (#520) it takes 18 s and is just another package.
# "tools/race.sh --check" fails unless every package of ./... is in exactly one shard and HEAVY names real packages.
#
#   tools/race.sh [package ...]
#   RACE_SHARD=i/2 tools/race.sh
#   tools/race.sh --check
set -eu
export CGO_ENABLED=1
GO=${GO:-go}
mod=github.com/nofuturekid/tasmofleet
HEAVY="$mod/internal/store $mod/internal/discovery $mod/cmd/tasmofleet $mod/internal/auth"

# shard_pkgs <i>: the packages of ./... in shard i of 2.
shard_pkgs() {
	$GO list ./... | awk -v want="$1" -v heavy="$HEAVY" '
		BEGIN { n = split(heavy, h, " "); for (i = 1; i <= n; i++) isheavy[h[i]] = 1 }
		{ if ((want == 1) == ($0 in isheavy)) print }'
}

if [ "${1:-}" = --check ]; then
	all=$($GO list ./... | sort)
	got=$( (shard_pkgs 1; shard_pkgs 2) | sort)
	if [ "$all" != "$got" ]; then
		echo "race shards: a package is in no shard or in both (< only in ./..., > only in the shards):" >&2
		tmp=$(mktemp -d)
		echo "$all" >"$tmp/all"
		echo "$got" >"$tmp/got"
		diff "$tmp/all" "$tmp/got" >&2 || true
		rm -rf "$tmp"
		exit 1
	fi
	for h in $HEAVY; do
		echo "$all" | grep -qxF "$h" || { echo "race shards: HEAVY names $h, which is no package of ./..." >&2; exit 1; }
	done
	[ -n "$(shard_pkgs 1)" ] && [ -n "$(shard_pkgs 2)" ] || { echo "race shards: a shard is empty" >&2; exit 1; }
	echo "race shards: all $(echo "$all" | wc -l) packages are in exactly one of 2 shards"
	exit 0
fi

if [ -n "${RACE_SHARD:-}" ] && [ $# -eq 0 ]; then
	pkgs=$(shard_pkgs "${RACE_SHARD%/*}")
	[ -n "$pkgs" ] || { echo "race: shard $RACE_SHARD has no packages" >&2; exit 1; }
else
	[ $# -gt 0 ] || set -- ./...
	pkgs=$($GO list "$@")
fi
exec $GO test -race -count=1 -timeout 40m $pkgs
