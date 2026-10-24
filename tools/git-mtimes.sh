#!/bin/sh
# Sets the modification time of every tracked file to the time of the last
# commit that changed it. A fresh checkout stamps every file with "now", and
# go test keys its result cache partly on the mtimes of the files a test
# reads, so without this CI could never reuse a cached test result. Needs the
# full history (actions/checkout fetch-depth: 0). Files changed in the working
# tree but not committed keep their mtime.
set -eu
git log --format='T %ct' --name-only --no-renames HEAD | awk '
  /^T [0-9]+$/ { t = $2; next }
  NF && !($0 in seen) { seen[$0] = 1; print t "\t" $0 }
' | while IFS="$(printf '\t')" read -r t f; do
  [ -f "$f" ] && touch -m -d "@$t" "$f"
done
