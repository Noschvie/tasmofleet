package sysconfig

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/store"
)

func fieldState(s *Service, key string) FieldState {
	for _, f := range s.State() {
		if f.Key == key {
			return f
		}
	}
	return FieldState{}
}

// Why (N20): the fleet default device password is a credential like a device
// password. It is stored sealed (Secret), never shown back on the page, masked
// in the audit log, survives a restart, and can be removed again. It has no
// bound target: it is only a form prefill, not sent anywhere by itself.
func TestDefaultDevicePasswordIsAWriteOnlySecret(t *testing.T) {
	ctx := context.Background()
	st := newMem()
	s, aud := newSvc(t, st)
	if _, err := s.Apply(ctx, Request{Actor: "admin", Values: map[string]string{"DEVICE_DEFAULT_PASSWORD": "fleet-pw-123"}}); err != nil {
		t.Fatal(err)
	}
	row := st.userRows()["DEVICE_DEFAULT_PASSWORD"]
	if !row.Secret || row.Value != "fleet-pw-123" || row.Target != "" {
		t.Fatalf("stored row %+v: must be sealed and unbound", row)
	}
	if ev := aud.last(); strings.Contains(ev.Detail, "fleet-pw-123") || !strings.Contains(ev.Detail, "***") {
		t.Fatalf("audit must mask the password: %s", ev.Detail)
	}
	if f := fieldState(s, "DEVICE_DEFAULT_PASSWORD"); f.Value != "" || !f.HasValue || !f.Secret {
		t.Fatalf("page state %+v: only set / not set may be shown", f)
	}
	if s.Snapshot().DeviceDefaultPassword != "fleet-pw-123" {
		t.Fatal("the running config must carry the password for the forms")
	}
	s2, _ := newSvc(t, st) // restart
	if s2.Snapshot().DeviceDefaultPassword != "fleet-pw-123" {
		t.Fatal("the stored password must survive a restart")
	}
	if _, err := s2.Apply(ctx, Request{Actor: "admin", Clear: []string{"DEVICE_DEFAULT_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.userRows()["DEVICE_DEFAULT_PASSWORD"]; ok || s2.Snapshot().DeviceDefaultPassword != "" || fieldState(s2, "DEVICE_DEFAULT_PASSWORD").HasValue {
		t.Fatal("removing the password must delete it")
	}
}

// Why (N20): a sealed value that cannot be opened (new key file) must not
// prefill garbage; it is reported and entering it again heals it.
func TestUnreadableDefaultDevicePasswordIsReportedAndHealed(t *testing.T) {
	st := newMem()
	st.rows["DEVICE_DEFAULT_PASSWORD"] = store.SystemSetting{Key: "DEVICE_DEFAULT_PASSWORD", Secret: true, Unreadable: true}
	s, _ := newSvc(t, st)
	if f := fieldState(s, "DEVICE_DEFAULT_PASSWORD"); f.HasValue || f.Skipped == "" || !f.NeedsReentry {
		t.Fatalf("state %+v", f)
	}
	if s.Snapshot().DeviceDefaultPassword != "" {
		t.Fatal("an unreadable password must not be used")
	}
	if _, err := s.Apply(context.Background(), Request{Actor: "admin", Values: map[string]string{"DEVICE_DEFAULT_PASSWORD": "new-pw"}}); err != nil {
		t.Fatal(err)
	}
	if f := fieldState(s, "DEVICE_DEFAULT_PASSWORD"); !f.HasValue || f.Skipped != "" {
		t.Fatalf("after entering it again: %+v", f)
	}
}

// Why (N17): the log level is live, limited to the four levels, and locked by
// the environment like the sign-in settings.
func TestLogLevelSetting(t *testing.T) {
	ctx := context.Background()
	s, aud := newSvc(t, newMem())
	var got []string
	s.Subscribe(func(e Event) {
		if slices.Contains(e.Keys, "LOG_LEVEL") {
			got = append(got, e.New.LogLevel)
		}
	})
	res, err := s.Apply(ctx, Request{Actor: "admin", Values: map[string]string{"LOG_LEVEL": "debug"}})
	if err != nil || !slices.Contains(res.Live, "LOG_LEVEL") {
		t.Fatalf("%+v %v", res, err)
	}
	if !slices.Equal(got, []string{"debug"}) || !strings.Contains(aud.last().Detail, `"new":"debug"`) {
		t.Fatalf("subscribers %v, audit %s", got, aud.last().Detail)
	}
	var ve *ValidationError
	if _, err := s.Apply(ctx, Request{Actor: "admin", Values: map[string]string{"LOG_LEVEL": "trace"}}); !errors.As(err, &ve) {
		t.Fatalf("unknown level: %v", err)
	}
	locked, _ := newSvc(t, newMem(), "TASMOFLEET_LOG_LEVEL=warn")
	if !fieldState(locked, "LOG_LEVEL").Locked {
		t.Fatal("env LOG_LEVEL must lock the field")
	}
	if _, err := locked.Apply(ctx, Request{Actor: "admin", Values: map[string]string{"LOG_LEVEL": "debug"}}); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked: %v", err)
	}
}

// Why (N19): the /metrics switch is live and defaults to on.
func TestMetricsEnabledSetting(t *testing.T) {
	s, _ := newSvc(t, newMem())
	if !s.Snapshot().MetricsEnabled {
		t.Fatal("default must be on")
	}
	res, err := s.Apply(context.Background(), Request{Actor: "admin", Values: map[string]string{"METRICS_ENABLED": "false"}})
	if err != nil || !slices.Contains(res.Live, "METRICS_ENABLED") || s.Snapshot().MetricsEnabled {
		t.Fatalf("%+v %v", res, err)
	}
}
