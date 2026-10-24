#!/bin/sh
# Race-detector run of the given packages (default ./...), what "make race"
# does, but internal/auth (PBKDF2 in nearly every test, about half of all
# race time) is split by test name into AUTH_PARTS processes that run next to
# the other packages. AUTH_PARTS=1 (or fewer than 4 CPUs) runs one plain
# go test; hosted runners of private repositories have two cores, where
# parallel race runs oversubscribe the machine and make timing tests flaky.
#
#   tools/race.sh [package ...]
set -eu
export CGO_ENABLED=1
GO=${GO:-go}
parts=${AUTH_PARTS:-2}
[ "$(nproc)" -ge 4 ] || parts=1
[ $# -gt 0 ] || set -- ./...
pkgs=$($GO list "$@")
auth=github.com/nofuturekid/tasmofleet/internal/auth
if [ "$parts" -le 1 ] || ! echo "$pkgs" | grep -qx "$auth"; then
	exec $GO test -race -count=1 -timeout 40m $pkgs
fi
others=$(echo "$pkgs" | grep -vx "$auth" || true)
names=$($GO test -list '.*' "$auth" | grep '^Test')
pids=""
if [ -n "$others" ]; then
	$GO test -race -count=1 -timeout 30m $others &
	pids="$!"
fi
i=0
while [ $i -lt "$parts" ]; do
	re=$(echo "$names" | awk -v n="$parts" -v i=$i 'NR % n == i' | paste -sd'|')
	$GO test -race -count=1 -timeout 30m "$auth" -run "^($re)\$" &
	pids="$pids $!"
	i=$((i + 1))
done
rc=0
for p in $pids; do wait "$p" || rc=1; done
exit $rc
