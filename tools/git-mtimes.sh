#!/bin/sh
# Sets the modification time of every tracked file to the time of the last
# commit that changed it, and of every directory to the newest of its
# entries. A fresh checkout stamps everything with "now", and go test keys
# its result cache partly on the mtimes of the files and directories a test
# reads, so without this CI could never reuse a cached test result. Needs the
# full history (actions/checkout fetch-depth: 0).
set -eu
git -c core.quotePath=false log --format='T %ct' --name-only --no-renames HEAD | awk '
  /^T [0-9]+$/ { t = $2; next }
  NF && !($0 in seen) {
    seen[$0] = 1; print t "\t" $0
    d = $0
    while (sub(/\/[^\/]*$/, "", d)) { if (!(d in dir) || dir[d] < t) dir[d] = t }
    if (!("." in dir) || dir["."] < t) dir["."] = t
  }
  END { for (d in dir) print dir[d] "\t" d }
' | while IFS="$(printf '\t')" read -r t f; do
  [ -e "$f" ] && touch -m -d "@$t" "$f"
done
