package restore

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/store"
	"github.com/hellwand/tasmofleet/internal/tasmota"
)

// switchDecoder is fakeDecoder that stops decoding when off is set (the decoder is gone or fails).
type switchDecoder struct {
	fakeDecoder
	off atomic.Bool
}

func (d *switchDecoder) Decode(ctx context.Context, dmp []byte) ([]byte, error) {
	if d.off.Load() {
		return nil, errors.New("decoder unavailable")
	}
	return d.fakeDecoder.Decode(ctx, dmp)
}

// listFails is Backups whose List fails (the store cannot be read); the restore uses List only to find the decoded copy.
type listFails struct{ Backups }

func (listFails) List(context.Context, model.DeviceID) ([]model.Backup, error) {
	return nil, errors.New("store unavailable")
}

// Why (#855): when the check backup differs from the restored one in bytes and a decoded copy is missing, the person has
// to know WHICH backup lacks it: "decoder missing" was shown when the decoder runs fine and the restored backup is an
// old one taken without it, which sends the person to look for a decoder that is not the problem. Each combination of
// the two backups lacking a decoded copy has its own sentence, and none of them is a verified result.
func TestRestoreSaysWhichBackupLacksTheDecodedCopyItCouldNotCompare(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                            string
		sourceLacksDecoded, decoderDown bool
		listFails                       bool // the backups of the device cannot be listed: the copy may exist
		want                            string
	}{
		{"only the restored backup", true, false, false, "the backup being restored has no decoded copy"},
		{"only the check backup", false, true, false, "the check backup taken after the restore has no decoded copy"},
		{"both", true, true, false, "neither the backup being restored nor the check backup taken after the restore has a decoded copy"},
		// Why (the review of #865): a decoded copy that could not be read is not a missing one; saying it is missing
		// would be the same wrong lead as "decoder missing".
		{"the restored backup's copy cannot be read", false, false, true, "the decoded copy of the backup being restored could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dec := &switchDecoder{}
			e := newFlowWith(t, "15.6.0", dec)
			a := e.dumpBackup(t, e.dumpA)
			if tc.sourceLacksDecoded {
				made, _ := e.bk.List(t.Context(), e.dev)
				for _, b := range made {
					if b.Kind == model.BackupDecoded && b.TakenAt.Equal(a.TakenAt) {
						if err := e.st.DeleteBackup(t.Context(), b.ID); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			dec.off.Store(tc.decoderDown)
			if tc.listFails {
				e.hb.Backups = listFails{e.hb.Backups}
			}
			// A restart without the settings: the check backup differs from the restored one in bytes (see
			// TestRestartWithoutTheSettingsIsNotVerified).
			e.sim.SetConfigDump(e.dumpB)
			e.cl.failCmd = func(c string) error {
				if c != "RestoreConfig" {
					return nil
				}
				_, err := e.cl.Client.Command(t.Context(), tasmota.TargetFor(e.device), "Restart 1")
				return err
			}
			e.cl.swallow = true
			job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{AllowUnknownFamily: tc.sourceLacksDecoded || tc.listFails})
			if err != nil {
				t.Fatal(err)
			}
			done := e.finish(t, job.ID)
			if done.State != store.RestoreDone || done.Verified {
				t.Fatalf("job %+v: a restore that could not be compared must not be verified", done)
			}
			if !strings.Contains(done.Detail, tc.want) || strings.Contains(done.Detail, "decoder missing") ||
				(tc.listFails && strings.Contains(done.Detail, "no decoded copy")) {
				t.Errorf("detail %q, want it to say %q", done.Detail, tc.want)
			}
		})
	}
}
