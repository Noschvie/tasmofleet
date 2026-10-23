# Contributing

Thanks for your interest. TasmoFleet is pre-1.0 and maintained by one person, so
please open an issue before larger changes to agree on the approach.

- **Bugs:** open an issue with the version, what you did, what you expected and
  what happened. Logs help; remove passwords and addresses you do not want to share.
- **Pull requests:** one topic per PR, against `main`. CI (`.github/workflows/ci.yml`)
  must be green. Run `make check` locally first; for UI changes also `make e2e`.
- **Rules:** [CLAUDE.md](CLAUDE.md) is the working guide for humans and AI agents
  alike: test first (tests say why the behaviour matters), no network in tests (use
  the Tasmota simulator), package boundaries, safety rules that must never regress,
  and the changelog rule (add your line under "Unreleased" in
  [CHANGELOG.md](CHANGELOG.md)).
- **AI-assisted contributions** are welcome; say so in the PR and make sure you
  have read and tested what you submit.
- **License:** contributions are accepted under the project license,
  [AGPL-3.0-or-later](LICENSE).
