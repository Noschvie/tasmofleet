#!/bin/sh
# The packages that carry integration-tagged tests, for "make integration" (#490).
#
#   tools/integration-packages.sh          the packages (./dir, one per line) with a _test.go file whose
#                                          //go:build line mentions integration, found in the whole
#                                          module (tools/ is its own module, testdata/ holds no packages)
#   tools/integration-packages.sh --guard  exit 1, listing them, when a file that is not a _test.go
#                                          carries the tag: that would also change the build of its
#                                          package's unit tests, and the search above would miss it
set -eu
tag='^//go:build .*integration'
go_files() { find . \( -name .git -o -name node_modules -o -name testdata -o -path ./tools \) -prune -o -name '*.go' -print; }
case "${1:-}" in
'')
	go_files | grep '_test\.go$' | xargs -r grep -l -E "$tag" | xargs -r -n1 dirname | sort -u
	;;
--guard)
	bad=$(go_files | grep -v '_test\.go$' | xargs -r grep -l -E "$tag" || true)
	[ -z "$bad" ] || {
		echo "integration-guard: non-test files carry the integration build tag:" >&2
		echo "$bad" >&2
		exit 1
	}
	;;
*)
	echo "usage: tools/integration-packages.sh [--guard]" >&2
	exit 2
	;;
esac
