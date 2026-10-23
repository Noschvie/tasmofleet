// Package cmdpolicy decides what may be sent to a device as a free-form
// Tasmota command (the console, and later bulk commands). It is pure: no I/O,
// standard library plus internal/model only.
//
// Rules, in order (decision 2026-10-21: admins may send any Tasmota command):
//
//  1. A command is exactly one command. Anything that could make the device
//     execute a second one (';' outside a Backlog, %3B, control characters,
//     look-alike semicolons) or hide what is sent (invisible and
//     bidirectional format characters) is malformed: Classify returns
//     ErrInvalid and a Forbidden verdict. The rejected text is never echoed in
//     errors. A Backlog may hold ASCII ';'; each part is judged on its own.
//  2. Reads, Power switching and Restart 1 are operator-level.
//  3. Everything else, every unknown name included, needs the admin role.
//  4. Destructive and lock-out commands (Reset, WifiConfig, firmware, Wi-Fi
//     off, WebServer other than 2, address and network changes, Module and
//     Template, credentials, Restart other than 1, Status 0) are still
//     sendable by an admin; Verdict.Confirm names the consequence and callers
//     with a UI ask once before sending.
//
// Redacted(cmd) is the only text that may reach the audit log or the console
// history, and ScrubReply the only way a device reply is shown. See
// docs/security/command-policy.md for the claims and their tests.
package cmdpolicy
