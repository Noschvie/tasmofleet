# CLAUDE.md: rules for AI agents (and humans) working on tasmofleet

This file is a router: the hard rules, what enforces each, and where the details live.
Ground rules in full: [ground-rules](docs/dev/ground-rules.md). Human contributors: see also [CONTRIBUTING.md](CONTRIBUTING.md). Spec: `docs/spec/2026-10-18-tasmofleet-design.md`.
`make agent-refs` (part of `make check` and the CI link job) fails when this file or `docs/dev/` names a file, directory,
make target or doc that does not exist. There is no second copy of these rules (no agents-file twin): fix them here.

## Hard rules

| Rule                                                                                                                                                                     | Enforced by                                                                  |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------- |
| **English only** in everything committed: code, comments, docs, commit messages, test names. | convention (review). Full ground rules: [ground-rules](docs/dev/ground-rules.md) |
| **Test first.** Failing test, then code. Tests say _why_ the behaviour matters (a comment or the test name); a test that cannot fail when the business rule changes is wrong. | partly: `make revert-check BASE=origin/main` (Go changes only, opt-out possible); otherwise convention |
| **No network in unit tests.** Only loopback listeners the test itself starts (`httptest`, the simulator, the embedded MQTT broker, the mock OIDC provider). Nothing reaches the internet, a real broker or a real device. Same for `integration` tagged tests. | convention (`noctx` is off for `_test.go`, `forbidigo` does not check loopback) |
| **Stay inside your change's scope.** You may read everything, but change only the **packages** your change is about. If a contract (interface or type in `internal/model` or another package's exported API) has to change, stop and report it instead of editing it; contract changes go through the spec. | convention (review) |
| **Minimal dependencies.** The allowed modules are listed in the spec ("Dependencies"); adding one needs a written justification in the spec in the same change. | convention (`make vuln` is govulncheck, not an allow-list) |
| **Fail loud.** Never report "done"/"tests pass" if anything was skipped. No `t.Skip` without an issue reference. No ignored errors (`_ =` only with a comment why).      | `make lint` (errcheck); otherwise convention                                 |
| **Match conventions.** gofmt, `log/slog`, `errors.Is/As`, context on every I/O call, table tests, `t.TempDir()`.                                                         | `make fmt-check`, `make vet`, `make lint`                                    |
| **No backward-compatibility shims** (no redirects, deprecated env aliases, old CLI names). Breaking changes are fine and go into `CHANGELOG.md`; user data is the exception: migrations stay append-only. | convention; `make migration-append-only` for migrations. [principles](docs/dev/principles.md) |
| **Product principles:** environment = bootstrap only, every other setting is live in the UI/DB; proportionate security for a home-lab tool.                               | convention. [principles](docs/dev/principles.md)                             |
| **Package boundaries** (model leaf, pure safety packages, only `store` imports SQL, no production import of simulator/testutil, templ only in `web`, consumers depend on interfaces, device access only via `tasmota.Client`). | partly `make lint` (depguard, forbidigo, source test in `internal/web/ui`), the rest convention. [architecture](docs/dev/architecture.md) |
| **Generated code:** never edit `*_templ.go`; run `make templ` after changing a `.templ` and commit both.                                                                 | `make templ-check`                                                           |
| **Commits are signed**, never disable signing; message via quoted heredoc; `Co-Authored-By` trailer naming the real model; no session links; no secrets in the repo; `make check` before each commit. | ruleset `main` (signed commits); `make check`. [commits](docs/dev/commits.md) |
| **Migrations are append-only** since v0.0.1: never edit a merged file in `internal/store/migrations/`, add `NNNN_name.sql`. Patch releases never touch a migration.     | `make migration-append-only`, `make migration-guard` (tag job `guards`)      |
| **Every change goes through a PR** against `main`, merged fast-forward only by the maintainer after `required` is green; never the GitHub merge button.                   | ruleset `main`, CI job `required`. [pull-requests](docs/dev/pull-requests.md) |
| **CHANGELOG line** under "Unreleased" for every change; outside contributors credited as `(#N, thanks @handle)`. | convention (`make changelog-check` only checks the version heading at release; #527 changes this). [releasing](docs/dev/releasing.md) |
| **UI changes:** screenshot pairs per screen kept outside the repo, never committed or embedded; "tests pass" is not acceptance for UI. Gate: `make e2e-core` plus the affected e2e specs. | convention (coordinator review). [ui-review](docs/dev/ui-review.md) |

## Safety rules that must never regress

Numbers are cited from code comments ("CLAUDE.md safety rule N"); full text, with the exact scope of each, in
[safety-rules](docs/dev/safety-rules.md). Enforced by the tests of the SAFETY packages and `make safety BASE=origin/main`.

1. `fwselect.Guard` runs immediately before every `OtaUrl` and `Upgrade` that TasmoFleet sends on its own; never bypass it, never "guess". An `Upgrade`/`OtaUrl` typed in the console or sent as a bulk command is not guarded on purpose (the person's responsibility after the confirmation); it is only refused while an update job holds the device.
2. `Status 0` is never sent by automated code; individual `Status N` queries, one at a time per device.
3. `Upgrade 1` is never retried blindly; after a lost response the engine probes first.
4. Device credentials never reach the browser, logs, errors or audit details.
5. Local accounts: CLI or admins in the UI with step-up; header principals only view; operators never create accounts or raise roles; listing needs no step-up, every change does.
6. Forwarded headers are trusted only from `TRUSTED_PROXIES` (checked on `RemoteAddr`).

## Working on X: read Y

| Working on                                                 | Read                                                         |
| ---------------------------------------------------------- | ------------------------------------------------------------ |
| Which package owns what, where code goes, templ            | [architecture](docs/dev/architecture.md)                     |
| Firmware, OTA, device commands, credentials, auth          | [safety-rules](docs/dev/safety-rules.md)                     |
| Tests, the gates before a push, revert check, diff coverage, mutation | [testing](docs/dev/testing.md)                    |
| A test that needs a device                                 | [simulator](docs/dev/simulator.md)                           |
| CI workflows, test tiers, `@core` e2e, quarantine, Actions pinning | [ci-tiers](docs/dev/ci-tiers.md)                     |
| Commits, signing, migrations                               | [commits](docs/dev/commits.md)                               |
| Opening or merging a PR, branches and worktrees            | [pull-requests](docs/dev/pull-requests.md)                   |
| Anything visible in the UI                                 | [ui-review](docs/dev/ui-review.md)                           |
| Releases, CHANGELOG, tags, announcements, the Unraid template | [releasing](docs/dev/releasing.md)                        |
| Config or product behaviour (compat shims, settings)       | [principles](docs/dev/principles.md)                         |

Where things are written down: spec `docs/spec/` (what and why), `ROADMAP.md`, research `docs/research/` (device behaviour,
TasmoAdmin lessons), fixtures `testdata/fixtures/` (real, sanitised device captures; see its README).

**Before you push:** `make check`; for a `SAFETY` package also `make safety BASE=origin/main`; for every fix
`make revert-check BASE=origin/main`. UI changes: also `make e2e-core`.
