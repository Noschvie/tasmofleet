package web

import "strings"

// blockedAction is a button that cannot act now: its label and the reason, "" when it can act.
type blockedAction struct{ Label, Why string }

// reasonGroup is the blocked buttons that share one reason. A row of buttons says each reason once, in one line under
// the row ("Restart and Back up: ..."), and the buttons point at it with aria-describedby (#1072).
type reasonGroup struct {
	Labels []string
	Why    string
}

// Lead is the names of the group: "Restart", "Restart and Back up", "A, B and C". A label's trailing "…" (a button
// that opens a dialog) is left out, so the lead does not run into the colon after it.
func (g reasonGroup) Lead() string {
	names := make([]string, len(g.Labels))
	for i, l := range g.Labels {
		names[i] = strings.TrimSuffix(l, "…")
	}
	n := len(names)
	if n < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:n-1], ", ") + " and " + names[n-1]
}

// groupReasons groups the blocked actions (those with a reason) by identical reason, in the order each reason first
// appears. It holds no reason text: the reasons stay with the code that decides them.
func groupReasons(as []blockedAction) []reasonGroup {
	var out []reasonGroup
next:
	for _, a := range as {
		if a.Why == "" {
			continue
		}
		for i := range out {
			if out[i].Why == a.Why {
				out[i].Labels = append(out[i].Labels, a.Label)
				continue next
			}
		}
		out = append(out, reasonGroup{Labels: []string{a.Label}, Why: a.Why})
	}
	return out
}
