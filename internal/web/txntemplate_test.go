package web

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
)

// Why (#327, #609): the template job's backup step links the backup the job took before it changed the
// device, the restore path when something went wrong. A backup is stamped with the second its dump was read,
// and the job takes it right after it was created, often in the same second: that backup is the job's own and
// must be linked. One taken in an earlier second is from before the job and is not.
func TestTheTemplateJobLinksTheBackupReadInItsFirstSecond(t *testing.T) {
	created := time.Date(2026, 11, 17, 10, 0, 0, 600_000_000, time.UTC)
	j := model.Job{DeviceID: 1, CreatedAt: created}
	r := httptest.NewRequest("GET", "/", nil)
	sameSecond := model.Backup{ID: 1, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: created.Truncate(time.Second)}
	h := &handler{d: Deps{Backups: &fakeBackups{list: []model.Backup{sameSecond}}}}
	if got := h.templateBackup(r, j); !got.At.Equal(sameSecond.TakenAt) {
		t.Fatalf("the backup read in the job's first second is not linked: %+v", got)
	}
	before := model.Backup{ID: 2, DeviceID: 1, Kind: model.BackupConfigDump, TakenAt: created.Truncate(time.Second).Add(-time.Second)}
	h = &handler{d: Deps{Backups: &fakeBackups{list: []model.Backup{before}}}}
	if got := h.templateBackup(r, j); !got.At.IsZero() {
		t.Fatalf("a backup from before the job is linked: %+v", got)
	}
}
