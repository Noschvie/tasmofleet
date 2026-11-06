# Principles: no compatibility shims, product principles

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md).

## No backward-compatibility shims

tasmofleet has no stable external API before 1.0 (maintainer's rule).
Don't add or keep compatibility layers: no redirects for moved routes, no deprecated
config/env aliases, no old CLI command names, no API compat layers. Breaking changes are
fine. Record each one in `CHANGELOG.md` (section "Unreleased", then the release).
**The only exception is user data:** migrations stay append-only, and the live database
must upgrade cleanly.

## Product principles (details in spec §1.1a)

- **Minimal mandatory configuration**: environment = bootstrap only. Every other setting
  (SSO included) is live in the UI and stored in the database; env values lock their
  field. New settings get a UI/DB setting, not a new environment variable.
- **Proportionate security (home-lab tool)**: keep the basics (CSRF, authz, no secrets in
  logs/HTML, chip-family guard, secrets only to their bound target). Step-up only for
  local account management and auth/SSO changes (not for restore, 2026-10-20). Reviews: fix High/Medium,
  document Low/Info as residual risk, no review loops.

