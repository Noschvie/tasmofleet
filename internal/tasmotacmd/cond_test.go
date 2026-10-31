package tasmotacmd

import "testing"

func TestEvalCondKnowsWhatItKnowsAndNothingElse(t *testing.T) {
	// A feature warning is only right when the fact is known: an unknown define must never turn
	// into "the device lacks it", or the editor would warn about every custom build.
	facts := map[string]Tri{"ESP32": True, "ESP8266": False, "USE_A": True, "USE_B": False}
	def := func(n string) Tri { return facts[n] }
	for _, tc := range []struct {
		expr string
		want Tri
	}{
		{"USE_A", True},
		{"!USE_A", False},
		{"USE_B", False},
		{"USE_B && USE_X", False}, // one known "no" decides an AND
		{"USE_A || USE_X", True},  // one known "yes" decides an OR
		{"USE_A && USE_X", Unknown},
		{"defined(ESP32) && !defined(ESP8266)", True},
		{"defined ESP32", True},
		{"USE_X", Unknown},
		{"(USE_B || USE_A) && ESP32", True},
		{"ESP_IDF_VERSION_MAJOR >= 5", Unknown},
		{"defined(USE_A) && ESP_IDF_VERSION_MAJOR >= 5", Unknown},
		{`__has_include("esp_hosted.h")`, Unknown},
		{"USE_A &&", Unknown},
		{"", Unknown},
	} {
		if got := EvalCond(tc.expr, def); got != tc.want {
			t.Errorf("%q = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestCondNames(t *testing.T) {
	got := CondNames("defined(USE_A) && !ESP32")
	if len(got) != 2 || got[0] != "USE_A" || got[1] != "ESP32" {
		t.Fatalf("%v", got)
	}
}
