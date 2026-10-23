// Package tasmofleet holds files of the repository root that the binary ships. It exists because
// an embed directive cannot reach above a package's directory; internal/changelog parses what it
// embeds.
package tasmofleet

import _ "embed" // for go:embed

// Changelog is CHANGELOG.md as built into this binary (What's new, design v2 N5).
//
//go:embed CHANGELOG.md
var Changelog string
