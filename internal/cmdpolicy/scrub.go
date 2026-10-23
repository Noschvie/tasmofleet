package cmdpolicy

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

const mask = "***"

// secretKeyParts mark JSON keys whose value is masked (case-insensitive
// substring, "_" and "-" ignored).
var secretKeyParts = []string{"password", "passwd", "pwd", "token", "secret", "apikey", "mqttuser", "ssid"}

var (
	reUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^\s/@:]+:[^\s/@]+@`)
	reBare     = regexp.MustCompile(`[^\s/@:\[\]]+:[^\s/@\[\]]+@`) // user:pass@host without a scheme
	reBracket  = regexp.MustCompile(`\[[^\]\s:@]+:[^\]\s@]+@`)     // WebSend [user:pass@host]
	reKeyVal   = regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|secret|api_?key)(\s*[=:]\s*)[^\s&"',;]+`)
)

func secretKey(k string) bool {
	k = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k))
	for _, p := range secretKeyParts {
		if strings.Contains(k, p) {
			return true
		}
	}
	return false
}

// scrubText masks credentials that appear inside free text.
func scrubText(s string) string {
	s = reUserinfo.ReplaceAllString(s, "${1}"+mask+"@")
	s = reBracket.ReplaceAllString(s, "["+mask+"@")
	s = reBare.ReplaceAllString(s, mask+"@")
	return reKeyVal.ReplaceAllString(s, "${1}${2}"+mask)
}

// ScrubReply masks credentials in a device reply: values of keys that look
// like secrets (password, token, MqttUser, ...) and credential patterns inside
// strings (URL userinfo, WebSend [user:pass@host], password=...). JSON keeps
// its key order and number notation. Text that is not JSON is scrubbed as
// text. A reply is shown or stored only after this.
func ScrubReply(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out bytes.Buffer
	if err := scrubValue(dec, &out, false); err != nil || dec.More() {
		return []byte(scrubText(string(raw)))
	}
	if _, err := dec.Token(); err != io.EOF {
		return []byte(scrubText(string(raw)))
	}
	return out.Bytes()
}

func writeString(out *bytes.Buffer, s string) {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)           // a string always encodes
	out.Truncate(out.Len() - 1) // Encode appends a newline
}

// scrubValue copies the next JSON value to out. masked replaces the whole
// value (scalar or container) by "***".
func scrubValue(dec *json.Decoder, out *bytes.Buffer, masked bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		if masked {
			if err := skip(dec, t); err != nil {
				return err
			}
			writeString(out, mask)
			return nil
		}
		return scrubContainer(dec, out, t)
	case string:
		if masked {
			writeString(out, mask)
		} else {
			writeString(out, scrubText(t))
		}
	case json.Number:
		if masked {
			writeString(out, mask)
		} else {
			out.WriteString(t.String())
		}
	case bool, nil:
		if masked {
			writeString(out, mask)
		} else {
			b, _ := json.Marshal(t) // bool and nil always marshal
			out.Write(b)
		}
	}
	return nil
}

func scrubContainer(dec *json.Decoder, out *bytes.Buffer, open json.Delim) error {
	openCh, closeCh := byte('['), byte(']')
	if open == '{' {
		openCh, closeCh = '{', '}'
	}
	out.WriteByte(openCh)
	for first := true; dec.More(); first = false {
		if !first {
			out.WriteByte(',')
		}
		masked := false
		if open == '{' {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			writeString(out, key)
			out.WriteByte(':')
			masked = secretKey(key)
		}
		if err := scrubValue(dec, out, masked); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // closing delimiter
		return err
	}
	out.WriteByte(closeCh)
	return nil
}

// skip consumes the rest of a container whose opening delimiter was read.
func skip(dec *json.Decoder, _ json.Delim) error {
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}
