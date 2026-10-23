package config

import (
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
			skipped[k] = errors.New("set by the environment")
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

// IsUI reports whether key is an operational setting owned by the system
// settings page.
func IsUI(key string) bool { return isUI(key) }

// IsLocked reports whether the operator set key in the environment.
func (c Config) IsLocked(key string) bool { return c.Locked[key] }

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
