// Package model holds the plain domain types shared by every other package:
// devices, hardware identity, firmware artifacts, OTA jobs, backups, audit
// events and principals.
//
// It has no behaviour beyond small, pure helpers and imports only the
// standard library. Every other internal package may import it; it imports
// none of them. Changing a type here is a contract change: update the spec
// (docs/spec) and every consumer in the same commit.
package model
