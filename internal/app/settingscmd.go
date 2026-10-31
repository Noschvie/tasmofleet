package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// SettingsFile is the format of `tasmofleet settings export|import`: the
// operational settings of the system settings page plus the managed policy
// rules (in the POLICY_RULES format). Secrets are never exported; an import
// file may carry MQTT_PASSWORD for an automated setup, so protect such a file.
type SettingsFile struct {
	Version     int               `json:"version"`
	Settings    map[string]string `json:"settings"`
	PolicyRules json.RawMessage   `json:"policy_rules,omitempty"`
}

// ExportSettings writes the stored operational settings and the managed rules. The webhook
// address is exported whole (an import needs it); like the file's other contents it is for the
// host administrator who runs this command, not for the web UI.
func ExportSettings(ctx context.Context, svc *Services, w io.Writer) error {
	f := SettingsFile{Version: 1, Settings: map[string]string{}}
	for _, st := range svc.Sys.State() {
		if !st.Secret && !st.Locked { // secrets never; settings the environment fixes belong to the container configuration
			f.Settings[st.Key] = st.Value
		}
	}
	rules, err := newRulesService(svc, nil, nil).Export(ctx)
	if err != nil {
		return err
	}
	f.PolicyRules = bytes.TrimSpace(rules)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}

// ImportSettings validates and applies a settings file like the settings page
// would, without the step-up (the CLI runs with access to the host). Nothing is
// applied if any value is invalid.
func ImportSettings(ctx context.Context, svc *Services, r io.Reader, out io.Writer) error {
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	dec.DisallowUnknownFields()
	var f SettingsFile
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("settings file: %w", err)
	}
	if f.Version != 1 {
		return errors.New("settings file: version must be 1")
	}
	rules := newRulesService(svc, nil, nil)
	var specs []sysconfig.RuleSpec
	if len(bytes.TrimSpace(f.PolicyRules)) > 0 {
		var err error
		if specs, err = rules.ParseImport(f.PolicyRules); err != nil {
			return fmt.Errorf("settings file: %w", err)
		}
	}
	res, err := svc.Sys.Apply(ctx, sysconfig.Request{
		Actor: "cli", Values: f.Settings, StepUp: sysconfig.StepUp{Done: true, Possible: true}, Audit: svc.Audit,
	})
	if err != nil {
		return fmt.Errorf("settings file: %w", err)
	}
	for _, c := range res.Changes {
		_, _ = fmt.Fprintf(out, "%s: %s -> %s\n", c.Key, c.Old, c.New)
	}
	if specs != nil {
		id, err := rules.Save(ctx, "cli", "imported from settings file", sysconfig.AnyVersion, specs, svc.Audit)
		switch {
		case errors.Is(err, sysconfig.ErrNoChange):
			_, _ = fmt.Fprintln(out, "policy rules: unchanged")
		case err != nil:
			return fmt.Errorf("settings file: policy rules: %w", err)
		default:
			_, _ = fmt.Fprintf(out, "policy rules: saved as version %d\n", id)
		}
	}
	if len(res.RestartRequired) > 0 {
		_, _ = fmt.Fprintln(out, "restart the server for:", res.RestartRequired)
	}
	return nil
}
