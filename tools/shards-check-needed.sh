#!/bin/sh
# Exit 0 when "make shards-check" can have a different result than at the merge base with CHANGED_BASE
# (default origin/main), 1 when it cannot. It reads the Makefile (MUTATION, shard count), tools/
# (race.sh, the mutation shard scripts and weights), the go.mod files and the list of packages, so
# it can only change after a change to one of those or after a package directory was added or
# removed. With no merge base it says "needed". Used by "make check" (#492); CI runs the check
# unconditionally.
set -eu
mb=$(git merge-base "${CHANGED_BASE:-origin/main}" HEAD 2>/dev/null) || exit 0
tools/changed-files.sh | grep -Eq '^(Makefile|tools/|go\.mod$)' && exit 0
# Directories holding Go files: now (tracked or untracked, still on disk) against the merge base.
dirs() { while read -r f; do dirname "$f"; done | sort -u; }
after=$(git ls-files --cached --others --exclude-standard -- '*.go' | while read -r f; do [ -e "$f" ] && echo "$f"; done | dirs)
before=$(git ls-tree -r --name-only "$mb" | grep '\.go$' | dirs || true)
[ "$after" != "$before" ]
