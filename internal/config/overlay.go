package config

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// KeyError is a configuration error that belongs to one key. Its text is the
// same as before ("TASMOFLEET_KEY: message"); the key is also available
// structurally so the web UI can show the message next to the field.
type KeyError struct {
	Key string
	Err error
}

func (e *KeyError) Error() string { return Prefix + e.Key + ": " + e.Err.Error() }
func (e *KeyError) Unwrap() error { return e.Err }

// KeyErrors maps each key to the message of its (first) error in err, which
// may be a joined error. Errors without a key are dropped.
func KeyErrors(err error) map[string]string {
	out := map[string]string{}
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		var ke *KeyError
		if errors.As(e, &ke) {
			if _, seen := out[ke.Key]; !seen {
				out[ke.Key] = ke.Err.Error()
			}
		}
	}
	walk(err)
	return out
}

// KeyInfo describes one configuration key.
type KeyInfo struct {
	Key    string
	Secret bool
	UI     bool // an operational setting: edited on the system settings page, stored in the database
	Lock   bool // a sign-in setting: editable unless the environment sets it
}

// Keys lists every configuration key in declaration order.
func Keys() []KeyInfo {
	rt := reflect.TypeFor[Config]()
	var out []KeyInfo
	for i := range rt.NumField() {
		f := rt.Field(i)
		if key := f.Tag.Get("env"); key != "" {
			out = append(out, KeyInfo{Key: key, Secret: f.Tag.Get("secret") == "true", UI: f.Tag.Get("ui") == "true" || f.Tag.Get("ui") == "lock", Lock: f.Tag.Get("ui") == "lock"})
		}
	}
	return out
}

func fieldFor(c *Config, key string) (reflect.Value, bool) {
	rv := reflect.ValueOf(c).Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		if rt.Field(i).Tag.Get("env") == key {
			return rv.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// Set parses value for key exactly as the environment loader does (empty
// means the key's default is NOT applied: the value is parsed as given).
func Set(c *Config, key, value string) error {
	f, ok := fieldFor(c, key)
	if !ok {
		return &KeyError{Key: key, Err: errors.New("unknown key")}
	}
	if err := setField(f, strings.TrimSpace(value)); err != nil {
		return &KeyError{Key: key, Err: err}
	}
	switch key {
	case "BACKUP_DAYS":
		c.BackupDays = canonicalDays(c.BackupDays)
	case "SNAPSHOT_DAYS":
		c.SnapshotDays = canonicalDays(c.SnapshotDays)
	}
	return nil
}

// DefaultValue returns the default of key as the string the loader parses.
func DefaultValue(key string) string {
	rt := reflect.TypeFor[Config]()
	for i := range rt.NumField() {
		if f := rt.Field(i); f.Tag.Get("env") == key {
			return f.Tag.Get("default")
		}
	}
	return ""
}

// Validate runs the cross-field validation Load runs.
func Validate(c Config) error { return c.validate() }

// Format renders the value of key so that Set parses it back to the same
// value (lists are comma separated).
func Format(c Config, key string) (string, error) {
	f, ok := fieldFor(&c, key)
	if !ok {
		return "", &KeyError{Key: key, Err: errors.New("unknown key")}
	}
	switch v := f.Interface().(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case int:
		return strconv.Itoa(v), nil
	case time.Duration:
		return shortDuration(v), nil
	case []string:
		return strings.Join(v, ","), nil
	case []netip.Prefix:
		parts := make([]string, len(v))
		for i, p := range v {
			parts[i] = p.String()
		}
		return strings.Join(parts, ","), nil
	}
	return "", fmt.Errorf("unsupported field type %s", f.Type())
}

// Overlay applies stored values on top of base, key by key, skipping values
// that do not parse or no longer validate (for instance because a bootstrap
// setting changed after the value was stored). It never fails: a start must not
// depend on what an admin typed into the UI. The returned map holds the reason
// for every skipped key; callers log it and show it.
func Overlay(base Config, vals map[string]string) (Config, map[string]error) {
	skipped := map[string]error{}
	cur := base
	var pending []string
	for k := range vals {
		if !isUI(k) {
			skipped[k] = errors.New("not an operational setting")
			continue
		}
		if base.Locked[k] {
			skipped[k] = errors.New(cmp.Or(base.LockReason(k), "set by the environment"))
			continue
		}
		pending = append(pending, k)
	}
	sort.Strings(pending)
	// Values may only validate together (a scan needs its networks), so a
	// value that fails is retried after the others until a pass changes nothing.
	for len(pending) > 0 {
		var retry []string
		for _, k := range pending {
			next := cur
			if err := Set(&next, k, vals[k]); err != nil {
				skipped[k] = err
				continue
			}
			if err := Validate(next); err != nil {
				skipped[k] = errors.New(KeyErrorsText(err, k))
				retry = append(retry, k)
				continue
			}
			cur = next
			delete(skipped, k)
		}
		if len(retry) == len(pending) {
			break
		}
		pending = retry
	}
	return cur, skipped
}

// KeyErrorsText returns the message for key in err, or err's text.
func KeyErrorsText(err error, key string) string {
	if m := KeyErrors(err)[key]; m != "" {
		return m
	}
	return err.Error()
}

// Equal reports whether two configs hold the same value for key.
func Equal(a, b Config, key string) bool {
	x, _ := Format(a, key)
	y, _ := Format(b, key)
	return x == y
}

func isUI(key string) bool {
	for _, k := range Keys() {
		if k.Key == key {
			return k.UI
		}
	}
	return false
}

// IsLocked reports whether the operator set key in the environment.
func (c Config) IsLocked(key string) bool { return c.Locked[key] }

// LockNote explains a lock the operator did not set directly. The issuer and the client secret lock
// together (see Load): a key that is locked and still empty can only be locked by its partner, because an
// empty variable or an empty secret file counts as not set. It returns "" for every other key and for a
// key the environment set itself. The text names the prefixed variables, which are the only form the
// container environment reads, and never a value.
func (c Config) LockNote(key string) string {
	if !c.Locked[key] {
		return ""
	}
	switch {
	case key == "OIDC_ISSUER" && c.OIDCIssuer == "":
		return Prefix + "OIDC_ISSUER is not set, and the client secret from the container configuration (" + Prefix + "OIDC_CLIENT_SECRET) locked it: " +
			"set " + Prefix + "OIDC_ISSUER there as well. Until then the stored issuer is ignored, single sign-on is off and local sign-in works."
	case key == "OIDC_CLIENT_SECRET" && c.OIDCClientSecret == "":
		return Prefix + "OIDC_CLIENT_SECRET is not set, and the issuer from the container configuration (" + Prefix + "OIDC_ISSUER) locked it: " +
			"set " + Prefix + "OIDC_CLIENT_SECRET (or " + Prefix + "OIDC_CLIENT_SECRET_FILE) there as well. Until then a client secret stored on the Settings page is ignored."
	}
	return ""
}

// LockReason is the short form of LockNote: the reason a stored value of key is skipped, for the log and
// the list of ignored values. The start already carries the full LockNote once as a warning (Load) and the
// locked field shows it, so the skip reason must not repeat that paragraph word for word (#720). It
// returns "" in the same cases as LockNote.
func (c Config) LockReason(key string) string {
	if c.LockNote(key) == "" {
		return ""
	}
	if key == "OIDC_ISSUER" {
		return "locked by the client secret (" + Prefix + "OIDC_CLIENT_SECRET) from the container configuration; " + Prefix + "OIDC_ISSUER is not set"
	}
	return "locked by the issuer (" + Prefix + "OIDC_ISSUER) from the container configuration; " + Prefix + "OIDC_CLIENT_SECRET is not set"
}

// shortDuration is time.Duration.String without the zero parts: "6h", not
// "6h0m0s". The result parses back to the same duration.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
