package config

import (
	"bufio"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/schedule"
)

// Prefix is prepended to every key in the environment and the file.
const Prefix = "TASMOFLEET_"

// Load reads env (prefix TASMOFLEET_) and the optional file named by
// TASMOFLEET_CONFIG_FILE (default <DATA_DIR>/tasmofleet.env, ignored if
// missing), applies defaults and validates. Environment wins over the file.
// Errors name the key.
//
// Only bootstrap and security settings are read into the Config. Operational
// settings (the fields tagged ui:"true", plus POLICY_RULES) belong to the system
// settings page; a value still set in the environment or the file is kept in
// Config.Legacy so that package sysconfig can import it once.
func Load(environ []string) (Config, error) {
	env := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		// An empty value counts as not set: Unraid passes unfilled template
		// fields as "KEY=", and that must mean "use the default, lock nothing".
		if ok && strings.HasPrefix(k, Prefix) && strings.TrimSpace(v) != "" {
			env[strings.TrimPrefix(k, Prefix)] = v
		}
	}
	dataDir := env["DATA_DIR"]
	if dataDir == "" {
		dataDir = "/data"
	}
	file := env["CONFIG_FILE"]
	explicit := file != ""
	if !explicit {
		file = dataDir + "/tasmofleet.env"
	}
	fileVals, err := readFile(file)
	switch {
	case err == nil:
		for k, v := range fileVals {
			if _, set := env[k]; !set && v != "" {
				env[k] = v
			}
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
	default:
		return Config{}, fmt.Errorf("config file %s: %w", file, err)
	}

	haProfile := inHomeAssistant(environ)
	if haProfile {
		if err := applyHAProfile(env); err != nil {
			return Config{}, err
		}
	}

	var c Config
	c.HAProfile = haProfile
	c.Legacy = map[string]string{}
	c.Locked = map[string]bool{}
	rv := reflect.ValueOf(&c).Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		key := f.Tag.Get("env")
		if key == "" {
			continue
		}
		val, ok := env[key]
		if fileKey, has := env[key+"_FILE"]; has && f.Tag.Get("secret") == "true" {
			b, err := os.ReadFile(fileKey) //nolint:gosec // path comes from the operator's configuration
			if err != nil {
				return Config{}, fmt.Errorf("%s%s_FILE: %w", Prefix, key, err)
			}
			// An empty file (or one holding only a newline) counts as not set, like an
			// empty variable (#148): it must neither lock the field nor clash with the variable.
			if fileVal := strings.TrimRight(string(b), "\r\n"); strings.TrimSpace(fileVal) != "" {
				if ok {
					return Config{}, fmt.Errorf("%s%s and %s%s_FILE are both set", Prefix, key, Prefix, key)
				}
				val, ok = fileVal, true
			}
		}
		if f.Tag.Get("ui") == "true" {
			if ok {
				c.Legacy[key] = strings.TrimSpace(val)
			}
			val = f.Tag.Get("default") // the database decides, not the environment
		} else if !ok {
			val = f.Tag.Get("default")
		} else if f.Tag.Get("ui") == "lock" {
			c.Locked[key] = true
		}
		if err := setField(rv.Field(i), strings.TrimSpace(val)); err != nil {
			return Config{}, &KeyError{Key: key, Err: err}
		}
	}
	// The identity provider and its client secret lock together: a secret the
	// operator fixed must only go to the issuer the operator fixed, and the other way round.
	if c.Locked["OIDC_ISSUER"] || c.Locked["OIDC_CLIENT_SECRET"] {
		c.Locked["OIDC_ISSUER"], c.Locked["OIDC_CLIENT_SECRET"] = true, true
	}
	// Nobody is locked out by a half-set pair (local accounts always sign in), but the stored issuer is
	// then skipped and stored single sign-on is off: say so once, with the variable that is missing (#720).
	if note := c.LockNote("OIDC_ISSUER"); note != "" {
		c.Warnings = append(c.Warnings, note)
	}
	// The transport follows from the mode, never from the port or a variable (#152).
	// Without a certificate, a proxy or an explicit opt-in to plain HTTP the server
	// serves a self-signed certificate, so a fresh container is reachable at once
	// without sending passwords in clear text. The container port stays LISTEN's
	// default :8080 in every mode.
	if c.TLSCertFile == "" && len(c.TrustedProxies) == 0 && !c.AllowInsecureHTTP {
		c.TLSSelfSigned, c.AutoTLS = true, true
	}
	if v, set := env["TLS_SELF_SIGNED"]; set {
		c.Warnings = append(c.Warnings, tlsSelfSignedWarning(v, c))
	}
	if v, ok := env["POLICY_RULES"]; ok && strings.TrimSpace(v) != "" {
		c.Legacy["POLICY_RULES"] = strings.TrimSpace(v)
	}
	for _, key := range removedKeys {
		if _, set := env[key]; set {
			c.Warnings = append(c.Warnings, Prefix+key+" is set but no longer exists and is ignored (removed 2026-10-19: the direct admin listener is gone; use a local admin on the login page and `tasmofleet user passwd`)")
		}
	}
	if v, set := env["BACKUP_INTERVAL"]; set {
		c.Warnings = append(c.Warnings, Prefix+"BACKUP_INTERVAL is set ("+v+") but no longer exists and is ignored (removed in 0.33: device backups run on chosen weekdays at a time of day, set on Devices, Device backups; an interval stored in the database became daily 03:00). Delete it.")
	}
	if v, set := env["SNAPSHOT_INTERVAL"]; set {
		c.Warnings = append(c.Warnings, Prefix+"SNAPSHOT_INTERVAL is set ("+v+") but no longer exists and is ignored (removed in 0.34: database snapshots run on chosen weekdays at a time of day, set on System, TasmoFleet backup; an interval stored in the database became daily 03:00). Delete it.")
	}
	if _, set := env["MQTT_ACL_CONFIRMED"]; set { // its value (a broker and user) is not echoed
		c.Warnings = append(c.Warnings, Prefix+"MQTT_ACL_CONFIRMED is set but no longer exists and is ignored (removed in 0.41: the checkbox on System, MQTT is \"Hide the broker ACL warning\" and its setting is MQTT_ACL_WARNING_HIDDEN, written by the checkbox, not by hand; a value stored in the database was carried over). Delete it.")
	}
	c.Warnings = append(c.Warnings, unprefixedSignInWarnings(environ)...)
	return c, c.validate()
}

// signInKeys are the sign-in settings without the prefix, in field order: AUTH_MODE, PUBLIC_URL, every
// OIDC_* key, the group lists and the _FILE variant of each secret among them. Read from the field
// tags, so a new OIDC_* key is covered without a second list.
func signInKeys() []string {
	var keys []string
	rt := reflect.TypeFor[Config]()
	for i := range rt.NumField() {
		f := rt.Field(i)
		k := f.Tag.Get("env")
		switch {
		case k == "AUTH_MODE", k == "PUBLIC_URL", k == "ADMIN_GROUPS", k == "OPERATOR_GROUPS", strings.HasPrefix(k, "OIDC_"):
			keys = append(keys, k)
			if f.Tag.Get("secret") == "true" {
				keys = append(keys, k+"_FILE")
			}
		}
	}
	return keys
}

// unprefixedSignInWarnings is one warning per sign-in variable that is set in the environment without
// the TASMOFLEET_ prefix (#721). Such a variable is not read at all, so it silently does nothing; the
// warning names the key that works and never the value. Other unprefixed names are not ours and stay
// silent, and so do short names in the env file, where the prefix is optional. An empty value counts
// as not set, like everywhere else.
func unprefixedSignInWarnings(environ []string) []string {
	set := map[string]bool{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.TrimSpace(v) != "" {
			set[k] = true
		}
	}
	var out []string
	for _, k := range signInKeys() {
		if set[k] {
			out = append(out, k+" is set in the environment without the "+Prefix+" prefix and is ignored; the name that works is "+Prefix+k+". The value is not used.")
		}
	}
	return out
}

// tlsSelfSignedWarning explains what happens to a TASMOFLEET_TLS_SELF_SIGNED value
// that is still set (it was a variable until 0.16.x). The value is ignored; the
// only saved combinations whose scheme differs from before are named exactly.
func tlsSelfSignedWarning(value string, c Config) string {
	msg := Prefix + "TLS_SELF_SIGNED is set but no longer exists and is ignored (removed in 0.17: TLS follows the mode, see docs/configuration.md). Delete it."
	if on, _ := strconv.ParseBool(strings.TrimSpace(value)); on && !c.TLSSelfSigned && c.TLSCertFile == "" {
		msg += " It was true, but with TRUSTED_PROXIES or ALLOW_INSECURE_HTTP the listener now serves plain HTTP on " + c.Listen +
			": point the reverse proxy at http:// instead of https://, or remove TRUSTED_PROXIES to get the self-signed certificate."
	}
	return msg
}

// removedKeys are settings that existed once. They are ignored, but a value
// still set in the environment or the env file is reported at start so that
// nobody believes a removed listener is still protecting or serving anything.
var removedKeys = []string{"ADMIN_LISTEN", "ADMIN_NETWORKS", "ADMIN_TLS_CERT_FILE", "ADMIN_TLS_KEY_FILE", "ADMIN_TLS_SANS"}

func readFile(path string) (map[string]string, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from the operator's configuration
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=value", n)
		}
		k = strings.TrimPrefix(strings.TrimSpace(k), Prefix)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out, sc.Err()
}

func setField(v reflect.Value, s string) error {
	switch v.Interface().(type) {
	case string:
		v.SetString(s)
	case bool:
		if s == "" {
			return nil
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case int:
		if s == "" {
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		v.SetInt(int64(n))
	case time.Duration:
		if s == "" {
			return nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		v.SetInt(int64(d))
	case []string:
		v.Set(reflect.ValueOf(splitList(s)))
	case []netip.Prefix:
		var ps []netip.Prefix
		for _, item := range splitList(s) {
			p, err := netip.ParsePrefix(item)
			if err != nil {
				return err
			}
			ps = append(ps, p.Masked())
		}
		v.Set(reflect.ValueOf(ps))
	default:
		return fmt.Errorf("unsupported field type %s", v.Type())
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func (c Config) validate() error {
	var errs []error
	bad := func(key, msg string) { errs = append(errs, &KeyError{Key: key, Err: errors.New(msg)}) }

	switch c.AuthMode {
	case "oidc":
		if c.OIDCIssuer == "" {
			bad("OIDC_ISSUER", "required when AUTH_MODE=oidc")
		}
		if c.OIDCClientID == "" {
			bad("OIDC_CLIENT_ID", "required when AUTH_MODE=oidc")
		}
		if c.PublicURL == "" {
			bad("PUBLIC_URL", "required when AUTH_MODE=oidc (redirect_uri is built from it)")
		}
	case "header":
		if len(c.TrustedProxies) == 0 {
			bad("TRUSTED_PROXIES", "required when AUTH_MODE=header")
		}
	case "local":
	default:
		bad("AUTH_MODE", "must be oidc, header or local")
	}
	// Group-based modes never mean "any authenticated user": at least one
	// group must be mapped to a role explicitly.
	if (c.AuthMode == "oidc" || c.AuthMode == "header") && len(c.AdminGroups)+len(c.OperatorGroups) == 0 {
		bad("ADMIN_GROUPS", "at least one of ADMIN_GROUPS, OPERATOR_GROUPS is required when AUTH_MODE="+c.AuthMode)
	}
	switch {
	case (c.TLSCertFile == "") != (c.TLSKeyFile == ""):
		bad("TLS_CERT_FILE", "TLS_CERT_FILE and TLS_KEY_FILE must be set together")
	}
	if c.SessionIdle <= 0 || c.SessionMax < c.SessionIdle {
		bad("SESSION_MAX", "SESSION_IDLE must be > 0 and SESSION_MAX >= SESSION_IDLE")
	}
	if c.PublicURL != "" {
		if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			bad("PUBLIC_URL", "must be an absolute http(s) URL")
		}
	}
	if c.PublicURL != "" && strings.HasPrefix(strings.ToLower(c.PublicURL), "http://") && (c.TLSCertFile != "" || c.TLSSelfSigned) {
		bad("PUBLIC_URL", "is http:// but the listener serves TLS; use https://")
	}
	if c.FWUpstreamOverride != "" {
		if u, err := url.Parse(c.FWUpstreamOverride); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad("FIRMWARE_UPSTREAM_OVERRIDE", "must be an absolute http(s) URL")
		}
	}
	if c.FWBaseURL != "" {
		if u, err := url.Parse(c.FWBaseURL); err != nil || u.Scheme != "http" || u.Host == "" {
			bad("FIRMWARE_BASE_URL", "must be an absolute plain http:// URL (ESP8266 OTA cannot use TLS)")
		}
	}
	if c.FWRefreshInterval < 0 {
		bad("FIRMWARE_REFRESH_INTERVAL", "must not be negative (0 = refresh only at start)")
	}
	if c.FWKeepReleases < 0 {
		bad("FIRMWARE_KEEP_RELEASES", "must not be negative (0 = never prune)")
	}
	if c.BasePath != "" && (!strings.HasPrefix(c.BasePath, "/") || strings.HasSuffix(c.BasePath, "/")) {
		bad("BASE_PATH", `must start with "/" and not end with "/"`)
	}
	c.validateOperational(bad)
	if c.OTAParallel < 1 {
		bad("OTA_PARALLEL", "must be >= 1")
	}
	if c.CRWarnPercent < 50 || c.CRWarnPercent > 100 {
		bad("CR_WARN_PERCENT", "must be between 50 and 100")
	}
	if c.BackupKeep < 1 {
		bad("BACKUP_KEEP", "must be >= 1")
	}
	return errors.Join(errs...)
}

// knownFirmwareSources are the source names package app can build.
var knownFirmwareSources = []string{"github", "ota.tasmota.com"}

// KnownFirmwareSources lists the valid FIRMWARE_SOURCES entries.
func KnownFirmwareSources() []string { return slices.Clone(knownFirmwareSources) }

// LogLevels are the valid LOG_LEVEL values (any letter case from the environment).
func LogLevels() []string { return []string{"debug", "info", "warn", "error"} }

// validateOperational checks the settings the web UI may edit. They are
// validated here, once, so the environment and the UI cannot disagree.
func (c Config) validateOperational(bad func(key, msg string)) {
	for _, s := range c.FWSources {
		if !slices.Contains(knownFirmwareSources, s) {
			bad("FIRMWARE_SOURCES", fmt.Sprintf("unknown source %q (known: %s)", s, strings.Join(knownFirmwareSources, ", ")))
		}
	}
	if !slices.Contains(LogLevels(), strings.ToLower(c.LogLevel)) {
		bad("LOG_LEVEL", "must be one of "+strings.Join(LogLevels(), ", "))
	}
	if c.MQTTURL != "" {
		if _, err := ParseBrokerURL(c.MQTTURL); err != nil {
			bad("MQTT_URL", "must be a broker URL such as tcp://host:1883 or ssl://host:8883")
		}
	}
	if c.PollInterval < time.Second {
		bad("POLL_INTERVAL", "must be at least 1s")
	}
	if c.TelePeriod < time.Second {
		bad("TELEPERIOD", "must be at least 1s")
	}
	if c.BackupMaxAge < 0 {
		bad("BACKUP_MAX_AGE", "must not be negative")
	}
	if _, err := schedule.ParseDays(strings.Join(c.BackupDays, ",")); err != nil {
		bad("BACKUP_DAYS", err.Error())
	}
	if _, _, err := schedule.ParseClock(c.BackupTime); err != nil {
		bad("BACKUP_TIME", err.Error())
	}
	if _, err := schedule.ParseDays(strings.Join(c.SnapshotDays, ",")); err != nil {
		bad("SNAPSHOT_DAYS", err.Error())
	}
	if _, _, err := schedule.ParseClock(c.SnapshotTime); err != nil {
		bad("SNAPSHOT_TIME", err.Error())
	}
	if c.SnapshotKeep < 1 {
		bad("SNAPSHOT_KEEP", "must be >= 1")
	}
	if c.PolicyLiveInterval != 0 && c.PolicyLiveInterval < time.Minute {
		bad("POLICY_LIVE_INTERVAL", "must be 0 (no background reads) or at least 1m")
	}
	if c.AuditKeep < 24*time.Hour {
		bad("AUDIT_RETENTION", "must be at least 24h")
	}
	c.validateNotify(bad)
	if c.DiscoveryScanInterval < time.Hour {
		bad("DISCOVERY_SCAN_INTERVAL", "must be at least 1h")
	}
	if c.DiscoveryScan {
		if len(c.DiscoveryScanCIDRs) == 0 {
			bad("DISCOVERY_SCAN_CIDRS", "required when DISCOVERY_SCAN is on")
		}
	}
	for _, p := range c.DiscoveryScanCIDRs {
		if !coveredBy(p, c.DeviceNetworks) {
			bad("DISCOVERY_SCAN_CIDRS", p.String()+" is not inside DEVICE_NETWORKS")
		}
		if p.Addr().Is4() && p.Bits() < 22 || p.Addr().Is6() {
			bad("DISCOVERY_SCAN_CIDRS", p.String()+": a scan covers at most 1024 IPv4 addresses (/22)")
		}
	}
}

// coveredBy reports whether prefix p lies completely inside one of nets.
func coveredBy(p netip.Prefix, nets []netip.Prefix) bool {
	for _, n := range nets {
		if n.Bits() <= p.Bits() && n.Contains(p.Addr()) {
			return true
		}
	}
	return false
}
