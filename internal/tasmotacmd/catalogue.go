// Package tasmotacmd is the Tasmota command catalogue: which top-level key of a stat/<topic>/RESULT
// message answers which command. The data is generated from the Tasmota source by
// cmd/tasmofleet-catalogue (see docs/research/tasmota-command-catalogue.md) and embedded here.
// The package is pure: no network, no file access at run time.
package tasmotacmd

import "sync"

// Catalogue is the content of catalogue-<version>.json.
type Catalogue struct {
	Tasmota       string         `json:"tasmota"`
	SourceSHA256  string         `json:"source_sha256"`
	Commands      []Command      `json:"commands"`
	SkippedTables []SkippedTable `json:"skipped_tables,omitempty"`

	once   sync.Once
	byName map[string][]*Command
}

// Command is one entry of a Tasmota command table.
type Command struct {
	// Command is the name as typed: the table prefix followed by the table entry, e.g. "OtaUrl", "MqttHost".
	Command string `json:"command"`
	Prefix  string `json:"prefix,omitempty"`
	Table   string `json:"table"`
	File    string `json:"file"`
	Handler string `json:"handler,omitempty"`
	// SynonymOf is set for SetOption synonyms (e.g. "SetOption127"); they share one handler.
	SynonymOf string `json:"synonym_of,omitempty"`
	// Requires lists the #if conditions around the entry (all must hold); not evaluated.
	Requires []string `json:"requires,omitempty"`
	// Index describes how the handler uses the numeric suffix (Power1, SetOption19); nil when it does not read it.
	Index *Index `json:"index,omitempty"`
	// Keys are the top-level keys of the reply. "<n>" stands for the index of the command
	// (1 when none is typed). Empty when Loose is set and nothing was derived.
	Keys []string `json:"keys,omitempty"`
	// Kind is the value kind of the reply keys: number, text, state, array, object or mixed; empty when unknown.
	Kind string `json:"kind,omitempty"`
	// Indexed is the reply when the command is sent with an index (SetOption19, FriendlyName2)
	// and the handler answers that case differently; nil when Keys, Kind and Loose hold for both.
	Indexed *IndexedReply `json:"indexed,omitempty"`
	// Via names the functions the handler calls that build the reply, when the handler does not.
	Via []string `json:"via,omitempty"`
	// Loose is the reason the reply keys cannot be derived statically; empty when Keys is complete.
	Loose string `json:"loose,omitempty"`
}

// IndexedReply is the reply of a command that carries an explicit index.
type IndexedReply struct {
	Keys  []string `json:"keys,omitempty"`
	Kind  string   `json:"kind,omitempty"`
	Loose string   `json:"loose,omitempty"`
}

// Index is the index range a handler checks, as source text: a number, a macro or a run-time expression.
type Index struct {
	Min string `json:"min,omitempty"`
	Max string `json:"max,omitempty"`
}

// SkippedTable is a command table the extractor could not read, so its commands are missing.
type SkippedTable struct {
	File   string `json:"file"`
	Table  string `json:"table"`
	Reason string `json:"reason"`
}
