#!/bin/sh
# Files that differ from the merge base with CHANGED_BASE (default origin/main), one path per line: committed,
# staged and unstaged changes of tracked files (a deleted or renamed file is listed with its old
# path too) and untracked files that are not ignored. Exits 1 without output when there is no merge
# base, so callers fall back to "everything changed".
#
#   tools/changed-files.sh
#   CHANGED_BASE=origin/release tools/changed-files.sh
set -eu
mb=$(git merge-base "${CHANGED_BASE:-origin/main}" HEAD 2>/dev/null) || exit 1
{
	git diff --name-only --no-renames "$mb"
	git ls-files --others --exclude-standard
} | sort -u
