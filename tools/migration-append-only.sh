#!/bin/sh
# Migrations are append-only (docs/dev/commits.md): fails when a file under internal/store/migrations/ that
# exists at the baseline release tag is modified, renamed, deleted or replaced; only additions pass.
# A database records only its schema version, so an edited shipped file would leave existing
# installations silently on the old schema (#595). Needs no manual release step: the baseline is
# found at run time.
#
# The baseline is the newest tag vX.Y.Z in the history of REV (or HEAD), not the newest tag of the
# repository: a branch that is behind the latest release is compared with the release it is based
# on (it would otherwise "delete" every migration it has not got yet), and a stray tag off main is
# never the baseline.
#
# usage: tools/migration-append-only.sh [TAG [REV]]
#   TAG  a tag to leave out when picking the baseline (the tag job passes the tag it is releasing,
#        otherwise REV would be compared with itself)
#   REV  the commit to check; without it the working tree is checked, so make check sees an edit
#        before it is committed
# No baseline fails everywhere (fail loud): CI needs actions/checkout with fetch-depth: 0 for the
# history and the tags, a local clone needs `git fetch --tags`.
set -eu
exclude=${1:-}
rev=${2:-}
dir=internal/store/migrations/
base=$(git tag --list --merged "${rev:-HEAD}" --sort=-v:refname 'v[0-9]*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | grep -vxF -- "$exclude" | head -n 1 || true)
if [ -z "$base" ]; then
	echo "migration guard: no release tag vX.Y.Z in the history of ${rev:-HEAD} (shallow clone or no tags? git fetch --tags [--unshallow] https://github.com/hellwand/tasmofleet)" >&2
	exit 1
fi
# --diff-filter=a: every change but an addition fails (M, D, R, and T: a file replaced by a symlink).
changed=$(git diff -M --name-status --diff-filter=a "$base" ${rev:+"$rev"} -- "$dir")
if [ -n "$changed" ]; then
	echo "migration guard: files shipped in $base were modified, renamed or deleted (migrations are append-only; add a new NNNN_name.sql instead):" >&2
	echo "$changed" >&2
	exit 1
fi
echo "migration guard: $dir only gained files since $base"
