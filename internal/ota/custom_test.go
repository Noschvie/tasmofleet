package ota

import (
	"context"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// recFW records which resolver the engine used.
type recFW struct {
	upstream, custom []string
}

func (r *recFW) Ensure(_ context.Context, release, name string) (model.Artifact, error) {
	r.upstream = append(r.upstream, release+"/"+name)
	return model.Artifact{ID: 1, Release: release, Filename: name}, nil
}

func (r *recFW) EnsureCustom(_ context.Context, key, release, name string) (model.Artifact, error) {
	r.custom = append(r.custom, key+"/"+release+"/"+name)
	return model.Artifact{ID: 2, Release: release, Filename: name, Custom: key}, nil
}

func (*recFW) URLFor(model.Artifact) string { return "" }

// Why: a job that installs an upload must never be served the upstream file
// of the same version (and the reverse): the two resolvers are never mixed or
// used as a fallback for each other. Jobs without a custom key behave exactly
// as before uploads existed (regression guard for every upstream update).
func TestEnsureKeepsUpstreamAndCustomApart(t *testing.T) {
	r := &recFW{}
	e := &Engine{fw: r}
	ctx := context.Background()
	if a, err := e.ensure(ctx, "", "16.0.0", "tasmota.bin.gz"); err != nil || a.Custom != "" {
		t.Fatalf("upstream: %+v, %v", a, err)
	}
	if a, err := e.ensure(ctx, "abc", "16.0.0", "tasmota.bin.gz"); err != nil || a.Custom != "abc" {
		t.Fatalf("custom: %+v, %v", a, err)
	}
	if len(r.upstream) != 1 || r.upstream[0] != "16.0.0/tasmota.bin.gz" || len(r.custom) != 1 || r.custom[0] != "abc/16.0.0/tasmota.bin.gz" {
		t.Fatalf("upstream %v, custom %v", r.upstream, r.custom)
	}
}

// Why: a cache that cannot serve uploads must fail a custom job, not quietly
// install the upstream release of the same number.
func TestEnsureCustomWithoutSupportFailsClosed(t *testing.T) {
	e := &Engine{fw: fakeFW{}}
	if _, err := e.ensure(context.Background(), "abc", "16.0.0", "tasmota.bin.gz"); err == nil || !strings.Contains(err.Error(), "custom") {
		t.Fatalf("err = %v", err)
	}
}

// Why: "retry failed" must install the same image again, not fall back to
// the upstream release of that version.
func TestRetryRequestKeepsTheCustomImage(t *testing.T) {
	r := retryRequest(model.Job{TargetRelease: "16.0.0", TargetCustom: "abc"})
	if r.Custom != "abc" || r.Release != "16.0.0" {
		t.Fatalf("%+v", r)
	}
}
