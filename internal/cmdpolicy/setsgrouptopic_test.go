package cmdpolicy

import "testing"

// Why (#1005, review): a GroupTopic<n> that TasmoFleet sends with a value may change any of the device's four group
// topics (Tasmota moves the set ones to the front), and the stored list must then count as unread, or another device's
// MQTT writes could reach this one through a new group topic. Every part of a Backlog counts, not only the first; a
// bare GroupTopic only reads and must not make the whole fleet wait for a fresh read.
func TestSetsGroupTopic(t *testing.T) {
	for cmd, want := range map[string]bool{
		"GroupTopic2 kitchen":                    true,
		"grouptopic kitchen":                     true,
		"GroupTopic1 tasmotas":                   true,
		"GroupTopic4 0":                          true, // clears GroupTopic4: the list changes too
		" GroupTopic3 a/b ":                      true,
		"Backlog Power1 ON; GroupTopic3 kitchen": true,
		"Backlog GroupTopic2 0; GroupTopic3 x":   true,
		"Backlog Topic x; GroupTopic2 kitchen":   true, // not only the first cut-off (CutOffName stops at Topic)
		"Backlog0 GroupTopic2 kitchen":           true,
		"GroupTopic":                             false, // the bare read
		"GroupTopic2":                            false,
		"Backlog GroupTopic; Status 1":           false,
		"Topic kitchen":                          false,
		"Power1 ON":                              false,
		"Status 1":                               false,
		"":                                       false,
		"GroupTopic2 a\x00":                      false, // refused by the policy: never sent
	} {
		if got := SetsGroupTopic(cmd); got != want {
			t.Errorf("SetsGroupTopic(%q) = %v, want %v", cmd, got, want)
		}
	}
}
