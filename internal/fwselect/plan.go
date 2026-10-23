package fwselect

import (
	"errors"
	"fmt"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// errBadSize: Plan needs the real size of the target image.
var errBadSize = errors.New("fwselect: target image size must be positive")

// planMarginBytes is kept free on ESP8266 beyond the image size (§7.6).
const planMarginBytes = 16 * 1024

// plan implements §7.6. Never decided by model name.
func plan(id model.Identity, size int64) (model.Plan, error) {
	if id.Family == model.ChipUnknown {
		return "", fmt.Errorf("plan: %w", ErrUnknownHardware)
	}
	if size <= 0 {
		return "", fmt.Errorf("%w: %d", errBadSize, size)
	}
	if id.Family.IsESP32() {
		return model.PlanDirect, nil
	}
	if id.FreeKB > 0 {
		if size > int64(id.FreeKB)*1024-planMarginBytes {
			return model.PlanStaged, nil
		}
		return model.PlanDirect, nil
	}
	if id.FlashSizeKB <= 1024 {
		return model.PlanStaged, nil
	}
	return model.PlanDirect, nil
}
