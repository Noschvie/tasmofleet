#!/bin/sh
# The Go packages a change since the merge base with CHANGED_BASE (default origin/main) can affect, as ./paths,
# one per line. A changed file belongs to the nearest package directory upwards, so embedded assets and
# migrations count. Changed code affects its package, every package that imports it (transitively) and
# every package whose tests import any of those (with -tags=integration, so a package whose integration
# tests import changed code is affected too). A changed _test.go file or testdata file affects its own
# package only. Used by "make quick" (#491).
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
	go list -tags=integration -f '{{.ImportPath}}|{{.Dir}}|{{join .Imports " "}}|{{join .TestImports " "}} {{join .XTestImports " "}}' ./...
	echo '--FILES--'
	printf '%s\n' "$files"
} | awk -F'|' -v root="$root" -v mod="$mod" '
	function code(p) { if (!(p in affected)) { affected[p] = 1; queue[++qn] = p } }
	/^--FILES--$/ { listing = 1; next }
	!listing {
		dir[$2] = $1
		n = split($3, imp, " ")
		for (i = 1; i <= n; i++) if (index(imp[i], mod) == 1) importers[imp[i]] = importers[imp[i]] " " $1
		n = split($4, imp, " ")
		for (i = 1; i <= n; i++) if (index(imp[i], mod) == 1) testers[imp[i]] = testers[imp[i]] " " $1
		next
	}
	{
		f = $0
		if (f !~ /\//) { if (f ~ /\.go$/ || f == "CHANGELOG.md") code(mod); next } # root package: changelog.go embeds CHANGELOG.md
		d = f; found = 0
		while (sub(/\/?[^\/]*$/, "", d) && d != "") {
			if ((root "/" d) in dir) { found = 1; break }
		}
		if (!found) { if (f ~ /\.go$/) everything = 1; next }
		if (f ~ /_test\.go$/ || f ~ /\/testdata\//) testonly[dir[root "/" d]] = 1; else code(dir[root "/" d])
	}
	END {
		if (everything) { print "./..."; exit }
		for (i = 1; i <= qn; i++) { # code reaches its importers, transitively
			n = split(importers[queue[i]], who, " ")
			for (j = 1; j <= n; j++) code(who[j])
		}
		for (p in affected) { # a package whose tests import changed code reruns its tests, nothing else
			out[p] = 1
			n = split(testers[p], who, " ")
			for (j = 1; j <= n; j++) out[who[j]] = 1
		}
		for (p in testonly) out[p] = 1 # a changed test file affects its own package only
		for (p in out) print (p == mod) ? "." : "./" substr(p, length(mod) + 2)
	}' | sort
