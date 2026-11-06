# Ground rules

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md). Read the parts of the spec you need
(`docs/spec/2026-10-18-tasmofleet-design.md`).

- **English only** in everything committed: code, comments, docs, commit messages, test names.
- **Test first.** Write the failing test, then the code. Tests say _why_ the behaviour
  matters (a comment or the test name), not just what it does. A test that cannot fail
  when the business rule changes is wrong.
- **No network in unit tests.** Only loopback listeners the test itself starts
  (`httptest`, the simulator, the embedded MQTT broker, the mock OIDC provider). Nothing
  reaches the internet, a real broker or a real device. Integration tests (build tag
  `integration`) follow the same rule.
- **Stay inside your change's scope.** You may read everything, but change only the
  packages your change is about. If a contract (interface or type
  in `internal/model` or another package's exported API) has to change, stop and
  report it instead of editing it. Contract changes go through the spec.
- **Minimal dependencies.** The allowed modules are listed in the spec ("Dependencies").
  Adding one needs a written justification in the spec in the same change.
- **Fail loud.** Never report "done" or "tests pass" if anything was skipped. No
  `t.Skip` without an issue reference. No ignored errors (`_ =` only with a comment
  saying why).
- **Match conventions.** gofmt, `log/slog` for logging, `errors.Is/As`, context on every
  I/O call, table tests, `t.TempDir()` for files.
