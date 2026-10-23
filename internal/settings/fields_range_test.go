package settings

import (
	"strconv"
	"testing"
)

// Why: the number input of a form advertises Min and Max; if they drifted from
// the validator the browser would offer values the server then refuses (or
// block values it accepts).
func TestNumberRangeMatchesValidator(t *testing.T) {
	for _, f := range Fields {
		if f.Kind != KindNumber {
			continue
		}
		if !f.HasRange {
			t.Errorf("%s: number field without a range", f.Key)
			continue
		}
		for _, v := range []int{f.Min, f.Max} {
			if err := f.Validate(strconv.Itoa(v)); err != nil {
				t.Errorf("%s: bound %d rejected: %v", f.Key, v, err)
			}
		}
		if f.Validate(strconv.Itoa(f.Min-1)) == nil {
			t.Errorf("%s: %d below Min accepted", f.Key, f.Min-1)
		}
		if f.Validate(strconv.Itoa(f.Max+1)) == nil {
			t.Errorf("%s: %d above Max accepted", f.Key, f.Max+1)
		}
	}
}
