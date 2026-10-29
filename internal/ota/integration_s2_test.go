//go:build integration

package ota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// ESP32-S2 has two upstream builds behind one image tag (#85, #88). Without the user's explicit
// build choice nothing is sent to the device: the preview asks for the choice and Submit refuses.
func TestITESP32S2NeedsABuildChoice(t *testing.T) {
	e := newItEnv(t)
	dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32S2})
	eng := e.start(2)
	items, err := eng.Preview(context.Background(), []Request{{DeviceID: dev, Release: itRelease}})
	if err != nil || len(items) != 1 || items[0].Decision != decisionRefuse || !items[0].BuildChoice {
		t.Fatalf("Preview: %+v, %v", items, err)
	}
	for _, build := range []string{"", "c3", "p4", "../s2"} {
		_, err = eng.Submit(context.Background(), "it", []Request{{DeviceID: dev, Release: itRelease, SkipBackup: true, Build: build}})
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("Submit with build %q: %v, want ErrRefused", build, err)
		}
	}
	for _, c := range e.ctrl[dev].Commands() {
		lc := strings.ToLower(c)
		if strings.HasPrefix(lc, "upgrade 1") || strings.HasPrefix(lc, "otaurl http") {
			t.Fatalf("refused device received %q", c)
		}
	}
	if act, _ := e.st.ActiveJobs(context.Background()); len(act) != 0 {
		t.Fatalf("a job was created: %+v", act)
	}
}

// With the choice, the S2 downloads exactly that build, passes the guard and the update ends
// SUCCEEDED although the device reports the shared tag afterwards (variant unknown): verify must
// compare with what the device can report, not with the selected "tasmota".
func TestITESP32S2ExplicitBuild(t *testing.T) {
	for _, build := range []string{"s2", "s2cdc"} {
		t.Run(build, func(t *testing.T) {
			e := newItEnv(t)
			dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32S2})
			orig := e.ctrl[dev].OtaURL()
			eng := e.start(2)
			items, err := eng.Preview(context.Background(), []Request{{DeviceID: dev, Release: itRelease, Build: build}})
			if err != nil || items[0].Decision != "upgrade" || items[0].Filename != "tasmota32"+build+".bin" {
				t.Fatalf("Preview: %+v, %v", items, err)
			}
			e.submit(eng, Request{DeviceID: dev, Build: build})
			j := e.expectSuccess(dev, orig, 1, model.PlanDirect)
			if j.TargetBuild != build || j.TargetVariant != "tasmota" {
				t.Fatalf("job build %q variant %q", j.TargetBuild, j.TargetVariant)
			}
			e.assertFlashed(dev, "/fw/"+itRelease+"/tasmota32"+build+".bin")
		})
	}
}

// The process dies in the middle of an S2 update: the resumed engine knows the build from the
// stored job alone and installs the same file; nothing is asked of the user again.
func TestITESP32S2RestartKeepsTheBuild(t *testing.T) {
	for _, tc := range []restartCase{{model.JobSetOtaURL, 1}, {model.JobUpgradeIssued, 1}, {model.JobAwaitReboot, 1}, {model.JobVerify, 1}} {
		t.Run(fmt.Sprintf("%s_%d", tc.state, tc.nth), func(t *testing.T) {
			e := newItEnv(t)
			e.keepBackup = true
			dev := e.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32S2})
			orig := e.ctrl[dev].OtaURL()
			crashed := e.crashAt(tc.state, tc.nth)
			eng := e.start(2)
			e.submit(eng, Request{DeviceID: dev, Build: "s2cdc"})
			select {
			case <-crashed:
			case <-time.After(60 * time.Second):
				t.Fatalf("job never reached %s #%d", tc.state, tc.nth)
			}
			e.stop()
			e.clearHook()
			if j := e.jobOf(dev); j.State.Terminal() || j.TargetBuild != "s2cdc" {
				t.Fatalf("job %s build %q at the crash point", j.State, j.TargetBuild)
			}
			e.start(2)
			e.expectSuccess(dev, orig, 1, model.PlanDirect)
			e.assertFlashed(dev, "/fw/"+itRelease+"/tasmota32s2cdc.bin")
		})
	}
}
