#!/bin/sh
# Diff coverage of the SAFETY packages (#555, make diff-cover): every line this branch added or changed in a
# non-test Go file of a SAFETY package must be executed by a test of that package. Runs the tests of the changed
# packages once with a coverprofile (tests tagged "integration" included: the ota engine is exercised by its
# in-package integration tests) and compares the profile with the diff (scripts/diff-cover.mjs).
#
#   SAFETY="./internal/ota ./internal/auth" BASE=<ref> tools/diff-cover.sh
set -eu
GO=${GO:-go}
NICE=${NICE-}
: "${SAFETY:?SAFETY (the list from the Makefile) is not set}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
dirs=$(echo "$SAFETY" | tr ' ' '\n' | sed 's|^\./||; /^$/d' | paste -sd, -)

tools/pr-diff.sh >"$tmp/diff"
pkgs=$(node scripts/diff-cover.mjs packages --diff "$tmp/diff" --dirs "$dirs")
if [ -z "$pkgs" ]; then
	echo "diff-cover: no line of a SAFETY package changed (production code), nothing to check"
	exit 0
fi
echo "diff-cover: changed SAFETY packages:" $pkgs
list=$(for p in $pkgs; do echo "./$p"; done)
# shellcheck disable=SC2086
$NICE $GO test -tags=integration -count=1 -covermode=set -coverprofile="$tmp/cover.out" $list
node scripts/diff-cover.mjs check --diff "$tmp/diff" --profile "$tmp/cover.out" --module "$($GO list -m)" --dirs "$dirs"
