package policy

import "testing"

// Why: the rule editor offers FamilyNames as chips; every one of them must be
// accepted by ParseRules or the editor would let an admin pick an invalid chip.
func TestFamilyNamesAreAllAccepted(t *testing.T) {
	names := FamilyNames()
	if len(names) < 2 || names[0] != AllESP32 {
		t.Fatalf("wildcard must come first: %v", names)
	}
	for _, n := range names {
		data := []byte(`[{"command":"SetOption0","want":"1","families":["` + n + `"]}]`)
		if _, err := ParseRules(data, nil); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}

// Why: a phone cannot show nine chips for "every ESP32", and a partial scope
// must never read like the whole family (that would hide a gap in the policy).
func TestScopeLabels(t *testing.T) {
	var spelled []string
	for _, f := range esp32Families() {
		spelled = append(spelled, string(f))
	}
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"wildcard", []string{AllESP32}, []string{LabelAllESP32}},
		{"all variants spelled out", spelled, []string{LabelAllESP32}},
		{"expanded rule scope plus esp8266", append([]string{"esp8266"}, spelled...), []string{LabelESP8266, LabelAllESP32}},
		{"partial scope keeps exact names", []string{"esp32", "esp32c3"}, []string{"esp32", "esp32c3"}},
		{"nothing", nil, nil},
	}
	for _, c := range cases {
		got := ScopeLabels(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("%s: %v, want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: %v, want %v", c.name, got, c.want)
			}
		}
	}
}
