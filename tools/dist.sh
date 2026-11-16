#!/usr/bin/env bash
# Native release archives (#472): static linux/amd64 and linux/arm64 binaries next to the image.
# Writes to DIST (default dist/release):
#   tasmofleet_<VERSION>_linux_<arch>.tar.gz   one directory of that name: tasmofleet (0755), LICENSE,
#                                              README.md and tasmofleet.service (deploy/systemd)
#   SHA256SUMS                                 the checksums of exactly those archives, written last
# A failed build writes nothing (no archive, no SHA256SUMS) and removes the files of an earlier run.
# The tar files are reproducible: sorted, the commit's time as mtime, owner 0, gzip without a name.
#
# usage: make dist VERSION=X.Y.Z
#   make passes GO, LDFLAGS and BUILDFLAGS (the Makefile is the one place for the build flags) and VERSION,
#   which is X.Y.Z or X.Y.Z-pre without a "v", as the image tag; LDFLAGS must stamp that version.
# release.yml runs it in the job "binaries" and attaches the files to the GitHub release.
set -euo pipefail

fail() {
	echo "dist: $*" >&2
	exit 1
}

version=${VERSION:-}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] ||
	fail "VERSION must be X.Y.Z or X.Y.Z-pre without a v (the image tag), got '$version'"
ldflags=${LDFLAGS:-}
[[ " $ldflags " == *" -X main.version=$version "* ]] ||
	fail "LDFLAGS do not stamp main.version=$version: '$ldflags' (run make dist VERSION=$version)"
go=${GO:-go}
out=$(realpath -m "${DIST:-dist/release}")

cd "$(dirname "$0")/.."
epoch=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct)}

mkdir -p "$out"
rm -f "$out"/tasmofleet_*_linux_*.tar.gz "$out"/SHA256SUMS
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

for arch in amd64 arm64; do
	name=tasmofleet_${version}_linux_${arch}
	mkdir -m 0755 "$stage/$name"
	# BUILDFLAGS is a list of flags from the Makefile, split on purpose.
	# shellcheck disable=SC2086
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch "$go" build ${BUILDFLAGS:-} -ldflags "$ldflags" -o "$stage/$name/tasmofleet" ./cmd/tasmofleet
	chmod 0755 "$stage/$name/tasmofleet"
	install -m 0644 LICENSE README.md deploy/systemd/tasmofleet.service "$stage/$name/"
	tar --sort=name --mtime="@$epoch" --owner=0 --group=0 --numeric-owner -C "$stage" -cf - "$name" |
		gzip -n -9 >"$stage/$name.tar.gz"
done

(cd "$stage" && sha256sum -- *.tar.gz >SHA256SUMS)
mv "$stage"/*.tar.gz "$out"/
mv "$stage/SHA256SUMS" "$out/SHA256SUMS"
ls -l "$out"
