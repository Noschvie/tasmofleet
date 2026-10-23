// Package tasmoadmin parses TasmoAdmin data files for the one-time import:
// devices.csv (inventory) and MyConfig.json (global settings such as the
// default device username/password and the OTA URL). It only parses; the
// import itself goes through inventory.Service.Upsert.
package tasmoadmin

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Errors. Row problems are wrapped in *RowError; match with errors.Is.
var (
	ErrNoAddress      = errors.New("tasmoadmin: row has no address")
	ErrEncryptedNoKey = errors.New("tasmoadmin: password is encrypted but no key was given")
	ErrDecrypt        = errors.New("tasmoadmin: password cannot be decrypted (wrong key or damaged cell)")
	ErrBadPort        = errors.New("tasmoadmin: unusable port, using 80")
	ErrMalformedRow   = errors.New("tasmoadmin: malformed CSV row")
	ErrRecordTooBig   = errors.New("tasmoadmin: record has too many or too long fields")
	ErrTooManyRecords = errors.New("tasmoadmin: too many records, the rest is ignored")
	ErrNotImplemented = errors.New("tasmoadmin: not implemented")
)

// maxInput bounds what the parsers read; real files are a few KiB.
const maxInput = 8 << 20

// Bounds that keep a hostile file from amplifying into memory: real files
// have 7-16 short columns and a few dozen rows.
const (
	maxRecords    = 1000
	maxFields     = 32
	maxFieldBytes = 4096
)

const (
	encPrefix = "enc:v1:"
	ivLen     = 12
	tagLen    = 16
	keyLen    = 32
)

// Column indexes of devices.csv (see docs/research/tasmoadmin-data-formats.md).
const (
	colID          = 0
	colNames       = 1
	colIP          = 2
	colUser        = 3
	colPassword    = 4
	colIsUpdatable = 10
	colPort        = 11
	colFriendly    = 12
	colTopic       = 14
)

// RowError reports a problem with one devices.csv record. Line is the
// 1-based line where the record starts. The message never contains cell
// values from the password column.
type RowError struct {
	Line int
	ID   string // TasmoAdmin device id (column 0), may be empty
	Err  error
}

func (e *RowError) Error() string {
	if e.ID != "" {
		return fmt.Sprintf("devices.csv line %d (id %s): %v", e.Line, e.ID, e.Err)
	}
	return fmt.Sprintf("devices.csv line %d: %v", e.Line, e.Err)
}

// Unwrap returns the underlying sentinel.
func (e *RowError) Unwrap() error { return e.Err }

// Config is the subset of MyConfig.json the importer uses.
type Config struct {
	DefaultUsername string
	DefaultPassword string
	OtaServerIP     string
	OtaServerPort   string
}

// ParseDevices reads devices.csv (fputcsv with backslash escape, 7 to 16
// columns). Rows without an IP are reported, not silently dropped. key is
// the content of .device-password.key (base64 of 32 bytes) used to decrypt
// "enc:v1:" passwords (AES-256-GCM, iv||tag||ciphertext); nil leaves
// encrypted passwords empty and reports them. Column 10 (isUpdatable=0)
// maps to Device.OTAExcluded, column 14 to Topic.
//
// key may be the file content (base64, whitespace allowed) or the 32 raw
// bytes. A key that is neither is a fatal error (third return value); row
// problems are returned in the []error slice as *RowError. Devices with a
// non-fatal problem (undecryptable password, bad port) are still returned.
//
// The device name is the friendly name (column 12), else the relay names
// (column 1); several relay names are joined with "-" like TasmoAdmin does.
// Notes records the TasmoAdmin id.
func ParseDevices(r io.Reader, key []byte) ([]model.Device, []error, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil {
		return nil, nil, fmt.Errorf("tasmoadmin: read devices.csv: %w", err)
	}
	if len(raw) > maxInput {
		return nil, nil, errors.New("tasmoadmin: devices.csv is implausibly large")
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))

	var (
		devs []model.Device
		errs []error
	)
	for _, rec := range splitRecords(string(raw)) {
		if rec.err != nil {
			errs = append(errs, &RowError{Line: rec.line, Err: rec.err})
			continue
		}
		d, rowErrs := deviceFromRow(rec, gcm)
		for _, e := range rowErrs {
			errs = append(errs, &RowError{Line: rec.line, ID: rec.field(colID), Err: e})
		}
		if errors.Is(errors.Join(rowErrs...), ErrNoAddress) {
			continue
		}
		devs = append(devs, d)
	}
	return devs, errs, nil
}

func deviceFromRow(rec record, gcm cipher.AEAD) (model.Device, []error) {
	var errs []error
	d := model.Device{
		Address:  strings.TrimSpace(rec.field(colIP)),
		HTTPUser: rec.field(colUser),
		Topic:    strings.TrimSpace(rec.field(colTopic)),
		Port:     80,
	}
	if d.Address == "" {
		return d, []error{ErrNoAddress}
	}
	names := rec.field(colFriendly)
	if strings.TrimSpace(names) == "" {
		names = rec.field(colNames)
	}
	d.Name = strings.Join(splitNonEmpty(names, "|"), "-")
	if id := rec.field(colID); id != "" {
		d.Notes = "Imported from TasmoAdmin (id " + id + ")"
	}
	d.OTAExcluded = strings.TrimSpace(rec.field(colIsUpdatable)) == "0"

	if p := strings.TrimSpace(rec.field(colPort)); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			errs = append(errs, ErrBadPort)
		} else {
			d.Port = n
		}
	}

	pw, err := decodePassword(rec.field(colPassword), gcm)
	d.HTTPPassword = pw
	if err != nil {
		errs = append(errs, err)
	}
	if d.HTTPPassword == "" {
		// TasmoAdmin stores a user for every device. A user without a
		// password means no auth on the device (spec 11.1), so it is dropped.
		d.HTTPUser = ""
	}
	return d, errs
}

func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// newGCM builds the cipher for key; nil key means "cannot decrypt".
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) == 0 {
		return nil, nil
	}
	raw := key
	if len(key) != keyLen {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(key)))
		if err != nil || len(dec) != keyLen {
			return nil, errors.New("tasmoadmin: password key must be base64 of 32 bytes (content of .device-password.key)")
		}
		raw = dec
	}
	block, err := aes.NewCipher(raw)
	if len(raw) == keyLen && len(key) != keyLen {
		clear(raw) // the decoded copy; the cipher keeps its own schedule
	}
	if err != nil {
		return nil, fmt.Errorf("tasmoadmin: password key: %w", err)
	}
	return cipher.NewGCM(block)
}

// decodePassword returns the plaintext of a password cell. A cell counts as
// encrypted when it has the enc:v1: prefix, decodes strictly as base64 and is
// longer than iv+tag; anything else is a legacy plaintext cell.
func decodePassword(cell string, gcm cipher.AEAD) (string, error) {
	if !strings.HasPrefix(cell, encPrefix) {
		return cell, nil
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(cell, encPrefix))
	if err != nil {
		return cell, nil //nolint:nilerr // not valid base64: a legacy plaintext password that merely starts with the prefix
	}
	if len(payload) <= ivLen+tagLen {
		return cell, nil // too short to be iv+tag+data: plaintext
	}
	if gcm == nil {
		return "", ErrEncryptedNoKey
	}
	iv, tag, ct := payload[:ivLen], payload[ivLen:ivLen+tagLen], payload[ivLen+tagLen:]
	plain, err := gcm.Open(nil, iv, append(append([]byte{}, ct...), tag...), nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}

// record is one parsed CSV record.
type record struct {
	line   int
	fields []string
	err    error
}

func (r record) field(i int) string {
	if i < len(r.fields) {
		return r.fields[i]
	}
	return ""
}

// splitRecords implements the dialect PHP's fputcsv writes with enclosure `"`
// and escape `\`: fields containing , " or line breaks are quoted; inside
// quotes both "" and \" stand for one quote. encoding/csv cannot read the
// backslash form. Blank lines are skipped.
func splitRecords(s string) []record {
	var recs []record
	line := 1
	i := 0
	for i < len(s) {
		if len(recs) >= maxRecords {
			recs = append(recs, record{line: line, err: ErrTooManyRecords})
			break
		}
		start := line
		var fields []string
		var cur strings.Builder
		inQuotes, wasQuoted, done, bad := false, false, false, false
		put := func(c byte) {
			if cur.Len() >= maxFieldBytes {
				bad = true // keep scanning so the record boundary stays right
				return
			}
			cur.WriteByte(c)
		}
		for i < len(s) && !done {
			c := s[i]
			switch {
			case inQuotes:
				switch {
				case c == '\\' && i+1 < len(s) && s[i+1] == '"':
					put('"')
					i += 2
					continue
				case c == '"' && i+1 < len(s) && s[i+1] == '"':
					put('"')
					i += 2
					continue
				case c == '"':
					inQuotes = false
				default:
					if c == '\n' {
						line++
					}
					put(c)
				}
			case c == '"' && cur.Len() == 0 && !wasQuoted:
				inQuotes, wasQuoted = true, true
			case c == ',':
				if len(fields) >= maxFields {
					bad = true
				} else {
					fields = append(fields, cur.String())
				}
				cur.Reset()
				wasQuoted = false
			case c == '\r':
				// part of CRLF; ignored
			case c == '\n':
				line++
				done = true
			default:
				put(c)
			}
			i++
		}
		if inQuotes {
			recs = append(recs, record{line: start, err: ErrMalformedRow})
			break
		}
		if bad {
			recs = append(recs, record{line: start, err: ErrRecordTooBig})
			continue
		}
		fields = append(fields, cur.String())
		if len(fields) == 1 && fields[0] == "" && !wasQuoted {
			continue // blank line
		}
		recs = append(recs, record{line: start, fields: fields})
	}
	return recs
}

// ParseConfig reads MyConfig.json. TasmoAdmin writes every value as a string,
// but numbers are accepted too. The login name and password hash in the file
// belong to TasmoAdmin's own login and are deliberately not imported
// (spec 11.2); MyConfig has no default device credentials, so DefaultUsername
// and DefaultPassword stay empty.
func ParseConfig(r io.Reader) (Config, error) {
	dec := json.NewDecoder(io.LimitReader(r, maxInput))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return Config{}, fmt.Errorf("tasmoadmin: MyConfig.json: %w", err)
	}
	str := func(k string) string {
		switch v := m[k].(type) {
		case string:
			return strings.TrimSpace(v)
		case json.Number:
			return v.String()
		}
		return ""
	}
	return Config{OtaServerIP: str("ota_server_ip"), OtaServerPort: str("ota_server_port")}, nil
}
