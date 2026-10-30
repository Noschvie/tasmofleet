package tasmotacmd

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"sync"
)

// The one catalogue file; pinning another Tasmota version replaces it (make catalogue).
//
//go:embed catalogue-*.json
var embedded embed.FS

// Reply is what the catalogue knows about the answer to one command.
type Reply struct {
	// Keys are the top-level keys of the RESULT message that answers the command, with the
	// index of the command filled in. Case differs between Tasmota versions' sources only
	// where the source itself differs; compare case-insensitively.
	Keys []string
	// Loose, when not empty, is the reason the keys cannot be derived from the source. Keys is
	// then empty: do not guess.
	Loose string
}

// Parse reads a catalogue file.
func Parse(data []byte) (*Catalogue, error) {
	var c Catalogue
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("tasmotacmd: %w", err)
	}
	return &c, nil
}

var (
	defaultOnce sync.Once
	defaultCat  *Catalogue
	defaultErr  error
)

// Default returns the embedded catalogue of the pinned Tasmota version.
func Default() (*Catalogue, error) {
	defaultOnce.Do(func() {
		files, err := fs.Glob(embedded, "catalogue-*.json")
		if err != nil {
			defaultErr = fmt.Errorf("tasmotacmd: %w", err)
			return
		}
		if len(files) != 1 {
			defaultErr = fmt.Errorf("tasmotacmd: want exactly one embedded catalogue, found %v", files)
			return
		}
		data, err := embedded.ReadFile(files[0])
		if err != nil {
			defaultErr = err
			return
		}
		defaultCat, defaultErr = Parse(data)
	})
	return defaultCat, defaultErr
}

// ReplyKeys returns the RESULT keys that answer cmd in the embedded catalogue; see Catalogue.ReplyKeys.
func ReplyKeys(cmd string) ([]string, bool) {
	c, err := Default()
	if err != nil {
		return nil, false
	}
	return c.ReplyKeys(cmd)
}

// Lookup is Catalogue.Lookup on the embedded catalogue.
func Lookup(cmd string) (Reply, bool) {
	c, err := Default()
	if err != nil {
		return Reply{}, false
	}
	return c.Lookup(cmd)
}

// ReplyKeys returns the top-level keys of the RESULT message that answers cmd. ok is false when
// the command is not in the catalogue or its keys are loose (see Lookup for the reason).
func (c *Catalogue) ReplyKeys(cmd string) ([]string, bool) {
	r, known := c.Lookup(cmd)
	if !known || r.Loose != "" || len(r.Keys) == 0 {
		return nil, false
	}
	return r.Keys, true
}

// Lookup resolves a command as it is sent, "OtaUrl", "SetOption19", "Power1 on" (arguments are
// ignored). The trailing digits are the command's index; without one the index is 1, as in
// Tasmota. known is false for a command that is not in the catalogue. When the same name is
// defined by several drivers, all of them must be derivable, and their keys are joined.
func (c *Catalogue) Lookup(cmd string) (Reply, bool) {
	c.once.Do(c.buildIndex)
	name := strings.Fields(cmd + " ")
	if len(name) == 0 {
		return Reply{}, false
	}
	base := name[0]
	idx, typed := "1", false
	end := len(base)
	for end > 0 && base[end-1] >= '0' && base[end-1] <= '9' {
		end--
	}
	if end < len(base) && end > 0 {
		n, err := strconv.Atoi(base[end:])
		if err == nil {
			idx, typed = strconv.Itoa(n), true
			base = base[:end]
		}
	}
	entries := c.byName[strings.ToLower(base)]
	if len(entries) == 0 {
		return Reply{}, false
	}
	var out Reply
	var reasons []string
	seen := map[string]bool{}
	for _, e := range entries {
		keys, loose := e.Keys, e.Loose
		if typed && e.Indexed != nil {
			keys, loose = e.Indexed.Keys, e.Indexed.Loose
		}
		if loose != "" {
			reasons = append(reasons, loose)
			continue
		}
		if len(keys) == 0 {
			reasons = append(reasons, "no reply keys were derived")
			continue
		}
		for _, k := range keys {
			k = strings.ReplaceAll(k, "<n>", idx)
			if !seen[strings.ToLower(k)] {
				seen[strings.ToLower(k)] = true
				out.Keys = append(out.Keys, k)
			}
		}
	}
	if len(reasons) > 0 {
		return Reply{Loose: strings.Join(dedupeStrings(reasons), "; ")}, true
	}
	return out, true
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (c *Catalogue) buildIndex() {
	c.byName = make(map[string][]*Command, len(c.Commands))
	for i := range c.Commands {
		k := strings.ToLower(c.Commands[i].Command)
		c.byName[k] = append(c.byName[k], &c.Commands[i])
	}
}
