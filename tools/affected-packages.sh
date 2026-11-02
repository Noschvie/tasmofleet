#!/bin/sh
# The Go packages a change since the merge base with CHANGED_BASE (default origin/main) can affect, as ./paths,
# one per line: the packages holding a changed file (the nearest package directory upwards, so
# testdata, static assets and migrations count) plus every package that imports one of them,
# directly or through others, in its code or its tests (with -tags=integration, so a package whose
# integration tests import a changed package is affected too). Used by "make quick" (#491).
#
# Prints ./... when it cannot tell: no merge base, a change to go.mod, go.sum, .golangci.yml or the root
# testdata/ (shared by many packages), or a deleted Go file in a directory that is no package any more.
# Prints nothing when no package is affected (a docs-only change).
set -eu
files=$(tools/changed-files.sh) || { echo './...'; exit 0; }
[ -n "$files" ] || exit 0
if printf '%s\n' "$files" | grep -Eq '^(go\.mod|go\.sum|\.golangci\.yml|testdata/)'; then
	echo './...'
	exit 0
fi
root=$(go list -m -f '{{.Dir}}')
mod=$(go list -m)
{
	go list -tags=integration -f '{{.ImportPath}}|{{.Dir}}|{{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./...
	echo '--FILES--'
	printf '%s\n' "$files"
} | awk -F'|' -v root="$root" -v mod="$mod" '
	function hit(p) { if (!(p in affected)) { affected[p] = 1; queue[++qn] = p } }
	/^--FILES--$/ { listing = 1; next }
	!listing {
		dir[$2] = $1
		n = split($3, imp, " ")
		for (i = 1; i <= n; i++) if (index(imp[i], mod) == 1) importers[imp[i]] = importers[imp[i]] " " $1
		next
	}
	{
		f = $0
		if (f !~ /\//) { if (f ~ /\.go$/ || f == "CHANGELOG.md") hit(mod); next } # root package: changelog.go embeds CHANGELOG.md
		d = f; found = 0
		while (sub(/\/?[^\/]*$/, "", d) && d != "") {
			if ((root "/" d) in dir) { hit(dir[root "/" d]); found = 1; break }
		}
		if (!found && f ~ /\.go$/) everything = 1
	}
	END {
		if (everything) { print "./..."; exit }
		for (i = 1; i <= qn; i++) {
			n = split(importers[queue[i]], who, " ")
			for (j = 1; j <= n; j++) hit(who[j])
		}
		for (p in affected) print (p == mod) ? "." : "./" substr(p, length(mod) + 2)
	}' | sort
