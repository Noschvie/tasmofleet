#!/bin/sh
# Dead-code guard (#1057): fails when deadcode finds a function that no main package reaches and
# tools/deadcode-allow.txt does not name it.
#
#   tools/deadcode-check.sh
#
# deadcode (golang.org/x/tools/cmd/deadcode) builds the call graph from the mains of ./... (the server,
# the catalogue generator, the dev environment and the revert helper) without their tests. golangci-lint's
# unused skips exported names, so an exported function that only its own test calls passes every other
# check; this one catches it. -generated includes generated code (the templ components in *_templ.go);
# without it deadcode skips every file with a "Code generated" header. A function that only a main used for development reaches counts as live
# (a known gap of ./...).
#
# The allow-list has one line per entry: "<regexp> # <reason>". The regexp is matched against the line
# "<import path> <function>" (a method is "Type.method"), must start with ^ and end with $ so that it
# cannot hide a neighbour, and the reason is mandatory. Fails when
#   - deadcode itself fails,
#   - a reported function matches no entry (delete it, or make production reach it, or add an entry),
#   - an entry has no reason or is not anchored,
#   - an entry matches nothing any more (the function is reachable or gone: delete the entry).
set -eu

allow=tools/deadcode-allow.txt
tool=${TOOL:-go tool -modfile=$PWD/tools/go.mod}
nice=${NICE-}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
out=$tmp/out
pats=$tmp/pats
: >"$pats"

# shellcheck disable=SC2086 # tool and nice are command prefixes
if ! $nice $tool deadcode -generated -f='{{range .Funcs}}{{printf "%s %s\n" $.Path .Name}}{{end}}' ./... >"$out"; then
	echo "deadcode-check: deadcode failed" >&2
	exit 1
fi

bad=0
while IFS= read -r line || [ -n "$line" ]; do
	case $line in '' | '#'*) continue ;; esac
	case $line in
	*' # '*) ;;
	*)
		echo "deadcode-check: $allow: entry without a reason (\"<regexp> # <reason>\"): $line" >&2
		bad=1
		continue
		;;
	esac
	pat=${line%% # *}
	reason=${line#* # }
	case $pat in
	'^'*'$') ;;
	*)
		echo "deadcode-check: $allow: regexp must start with ^ and end with \$: $pat" >&2
		bad=1
		continue
		;;
	esac
	if [ -z "$reason" ]; then
		echo "deadcode-check: $allow: empty reason: $line" >&2
		bad=1
		continue
	fi
	if ! grep -Eq -- "$pat" "$out"; then
		echo "deadcode-check: $allow: stale entry, nothing is unreachable there any more (delete it): $pat" >&2
		bad=1
	fi
	printf '%s\n' "$pat" >>"$pats"
done <"$allow"

unlisted=$(grep -Evf "$pats" "$out" || true)
if [ -n "$unlisted" ]; then
	echo "deadcode-check: unreachable functions (no main package calls them):" >&2
	echo "$unlisted" | sed 's/^/  /' >&2
	echo "Delete them. A function only a test needs belongs in a _test.go file. A test kit or seam is added" >&2
	echo "to $allow with its reason." >&2
	bad=1
fi
exit "$bad"
