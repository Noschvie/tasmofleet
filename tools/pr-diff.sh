#!/bin/sh
# The change a branch makes, as `git diff -U0` against the merge base with BASE (default CHANGED_BASE, then
# origin/main): committed, staged and unstaged changes of tracked files, and every untracked file that is not
# ignored as an added file. No renames (a move is a delete and an add), no context lines. diff-cover and
# revert-check read it (scripts/diff-parse.mjs). Exits 2 without a merge base: a gate must not guess.
#
#   BASE=<ref or sha> tools/pr-diff.sh
set -eu
base=${BASE:-${CHANGED_BASE:-origin/main}}
mb=$(git merge-base "$base" HEAD 2>/dev/null) || {
	echo "pr-diff: no merge base of '$base' and HEAD (fetch it, or pass BASE=<ref>)" >&2
	exit 2
}
git -c core.quotepath=off diff -U0 --no-renames --no-color "$mb"
git -c core.quotepath=off ls-files --others --exclude-standard | while IFS= read -r f; do
	# --no-index exits 1 when the files differ, which is the point
	git -c core.quotepath=off diff -U0 --no-color --no-index -- /dev/null "$f" || true
done
