package simulator

import (
	"bytes"
	"encoding/json"
	"strings"
)

// member is one top-level "key":value pair of a JSON object, kept as raw
// bytes so number formatting (0.000, 29.0) and key order survive replay.
type member struct {
	key string
	raw []byte // the full `"key":value` text
}

// topMembers splits a JSON object into its top-level members without
// re-encoding anything.
func topMembers(obj []byte) []member {
	obj = bytes.TrimSpace(obj)
	if len(obj) < 2 || obj[0] != '{' {
		return nil
	}
	var out []member
	depth, start := 0, 1
	inStr, esc := false, false
	flush := func(end int) {
		seg := bytes.TrimSpace(obj[start:end])
		if len(seg) == 0 {
			return
		}
		var k string
		if seg[0] == '"' {
			if i := bytes.Index(seg[1:], []byte(`"`)); i >= 0 {
				k = string(seg[1 : 1+i])
			}
		}
		out = append(out, member{key: k, raw: seg})
	}
	for i := 0; i < len(obj); i++ {
		c := obj[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				flush(i)
			}
		case ',':
			if depth == 1 {
				flush(i)
				start = i + 1
			}
		}
	}
	return out
}

// mergeObjects concatenates the members of several JSON objects into one,
// like /cm does for commands that log several messages.
func mergeObjects(objs ...[]byte) []byte {
	var parts [][]byte
	for _, o := range objs {
		for _, m := range topMembers(o) {
			parts = append(parts, m.raw)
		}
	}
	return append(append([]byte{'{'}, bytes.Join(parts, []byte{','})...), '}')
}

// jsonString encodes s like Tasmota does: no HTML escaping of < > &.
func jsonString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil { // a string always encodes; keep the reply well formed regardless
		return `""`
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
