---
section: Changed
---
Changelog lines are added as fragment files in `changes/unreleased/` instead of edits to `CHANGELOG.md`, so parallel pull requests no longer conflict there; the release commit assembles them with `make changelog-assemble` (#527)
