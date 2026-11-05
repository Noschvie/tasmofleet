#!/bin/sh
# Migrations are append-only (CLAUDE.md): fails when a file under internal/store/migrations/ that
# exists at the latest release tag vX.Y.Z is modified, renamed or deleted between that tag and REV.
# Only additions pass. A database records only its schema version, so an edited shipped file would
# leave existing installations silently on the old schema (#595). Needs no manual release step: the
# baseline is the newest tag, found at run time.
#
# usage: tools/migration-append-only.sh [TAG [REV]]
#   TAG  a tag to leave out when picking the baseline (the tag job passes the tag it is releasing,
#        otherwise HEAD would be compared with itself)
#   REV  the commit to check (default HEAD)
# CI checkouts need the tags: actions/checkout with fetch-depth: 0.
set -eu
exclude=${1:-}
rev=${2:-HEAD}
dir=internal/store/migrations/
base=$(git tag --list --sort=-v:refname 'v[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | grep -vxF -- "$exclude" | head -n 1 || true)
if [ -z "$base" ]; then
	echo "migration guard: no release tag vX.Y.Z to compare with (shallow checkout without tags?)" >&2
	exit 1
fi
changed=$(git diff -M --name-status --diff-filter=MDR "$base..$rev" -- "$dir")
if [ -n "$changed" ]; then
	echo "migration guard: files shipped in $base were modified, renamed or deleted (migrations are append-only; add a new NNNN_name.sql instead):" >&2
	echo "$changed" >&2
	exit 1
fi
echo "migration guard: $dir only gained files since $base"
