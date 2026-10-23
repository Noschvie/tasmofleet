# Per-user interface preferences

Scope: `POST /prefs/{key}` and the `user_prefs` table (table column widths and
hidden columns, page size). Code: `internal/web/tableprefs.go`,
`internal/store/prefs.go`. A preference is keyed by the principal's subject, so
one user's layout never reaches another. Keys and values are validated by
package web; nothing in them is secret or acted on by the server beyond
rendering.

Reviewed 2026-10-19 (Opus): no High or Medium finding.

## Lifecycle

Deleting a local account also deletes its preferences
(`DeleteLocalUser`, `internal/store/auth.go`), so a later account with the same
name starts with the defaults.

## Residual risks

- **Subject namespace without a mode prefix (Low, accepted).** Local accounts use
  the subject `local:<name>`. In header or OIDC mode an identity that is literally
  named `local:admin` would share preferences with the break-glass account. The
  effect is cosmetic (column widths) and the pattern is the same as for sessions
  and the audit log, so it is accepted.
- Preferences of OIDC and header users are not removed when the identity
  provider drops the user (tasmofleet does not learn about it). They are small
  and inert.
