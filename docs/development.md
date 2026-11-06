# Development

```sh
make check   # templ, fmt, vet, golangci-lint (incl. staticcheck), unit and integration tests, govulncheck, build
make quick   # between two edits: the packages changed since origin/main and their importers only
make ci      # everything CI runs: check plus race, mutation and e2e tests
make demo    # the static demo (dist/demo); make demo-media renews docs/media
```

Building the image yourself: a plain `docker build` has no git history, so it reports
`<last release>+dev`. Pass the version to get the release and the commit:
`docker build --build-arg VERSION=$(git describe --tags) -t tasmofleet .` (for example
`v0.15.2-3-gabc1234`). `make image` does this.

Requires Go 1.27. The e2e tests need Node.js (`make e2e-deps`, then
`npx playwright install chromium` once). See [CONTRIBUTING.md](../CONTRIBUTING.md) and
[CLAUDE.md](../CLAUDE.md) (rules for AI agents and humans).
