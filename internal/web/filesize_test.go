package web

import (
	"strings"
	"testing"

	"github.com/hellwand/tasmofleet/internal/ota"
)

// Why (#1104, A3-040): the same file read "642.1 KiB" on the Firmware page and "642 KB" in the update
// preview, because two helpers wrote sizes with two units. A size is one rule (KiB and MiB, the binary
// units the files really have), written by humanBytes alone; the Firmware page only keeps the unit from
// wrapping with a no-break space.
func TestUpdatePreviewAndFirmwarePageWriteTheSameSize(t *testing.T) {
	const nbsp = " "
	for _, n := range []int64{512, 657_000, 1_990_656, 5 << 20} {
		fw := strings.ReplaceAll(sizeNoWrap(n), nbsp, " ")
		row := previewRow{Item: ota.PreviewItem{Filename: "tasmota.bin.gz"}, Size: n}
		if got, want := row.FileText(), "tasmota.bin.gz · "+fw; got != want {
			t.Errorf("size %d: the update preview says %q, the Firmware page %q", n, got, want)
		}
	}
	if got := (previewRow{Item: ota.PreviewItem{Filename: "x.bin"}, Size: 657_000}).FileText(); strings.Contains(got, "KB") {
		t.Errorf("the preview still writes decimal-looking KB: %q", got)
	}
}
