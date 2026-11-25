package web

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why: "Cancel" next to "Cancel transaction" is ambiguous (which one cancels the
// update?). The dialog's dismissing button names what it does: the update keeps running.
// The shared dialog fills that label from the opener, so every opener must carry it and
// the dialog must have the slot.
func TestCancelDialogsNameTheirDismissButton(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobQueued, CreatedAt: time.Now()}}
	_, body := e.do(t, "GET", "/transactions/7", "admin", nil)
	if n := strings.Count(body, `data-dlg-dismiss="Keep running"`); n != 2 {
		t.Errorf("transaction and job cancel openers with a dismiss label: %d, want 2", n)
	}
	if !strings.Contains(body, `data-close data-fill="dismiss">Cancel</button>`) {
		t.Errorf("the shared dialog has no slot for the dismiss label")
	}
}

// Why: the design system ends a trigger with "…" only when it opens a step that asks for more input. Restart only asks
// for the confirmation, so it has none (#1093); Delete leads to a page that asks what happens to the device's backups,
// so its "…" stays.
func TestRowMenuEllipsisFollowsWhetherTheStepAsksForInput(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	if !strings.Contains(body, `data-dlg-verb="Restart device"`) || !strings.Contains(body, `>Restart</a>`) || strings.Contains(body, "Restart…") {
		t.Errorf("row menu Restart only confirms: no ellipsis")
	}
	if !regexp.MustCompile(`href="/tf/devices/\d+/delete">Delete…</a>`).MatchString(body) {
		t.Errorf("row menu Delete lost its ellipsis or its confirmation page")
	}
}

// Why: the select-all box must sit on the same vertical line as the row boxes; the header
// cell takes the selection column's geometry only when it carries the "sel" class.
func TestSelectAllHeaderCellIsASelectionCell(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	if !strings.Contains(body, `<th class="sel"><input type="checkbox" data-select-all`) {
		t.Errorf("devices select-all header cell is not th.sel")
	}
}

// Why: the style guide lists every component; the row menu sample shows Restart with its
// ellipsis (it opens a confirmation), like the device list's menu.
func TestStyleguideRowMenuShowsRestartWithEllipsis(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/styleguide", "admin", nil)
	if !strings.Contains(body, `<span>Restart…</span></a>`) {
		t.Errorf("style guide row menu has no Restart…")
	}
}
