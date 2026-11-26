package ui

import (
	"strings"
	"testing"
	"time"
)

// Why (MOCK-409 decision 3): the phone's one line is short ("2 failed · 3 skipped"); the bar and the
// sheet carry the full count. A run that only skipped devices stays "All good" (skipped is no failure).
func TestStatusBackupPhoneLineIsShortAndTheBarIsFull(t *testing.T) {
	at := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	partly := StatusBar{Backup: &StatusBackup{At: at, Total: 24, Failed: 2, Skipped: 3}}
	text, sub := partly.summary()
	if text != "Backup partly failed" || sub != " · 2 failed · 3 skipped" {
		t.Errorf("phone line = %q %q, want the short form", text, sub)
	}
	if got := partly.Backup.countText(); got != "22 backed up · 2 failed · 3 skipped" {
		t.Errorf("full text = %q", got)
	}
	calm := StatusBar{Backup: &StatusBackup{At: at, Total: 24, Skipped: 3}}
	if text, _ := calm.summary(); text != "All good" {
		t.Errorf("skipped only: %q, want All good", text)
	}
	if got := calm.Backup.countText(); got != "24 backed up · 3 skipped" {
		t.Errorf("skipped only text = %q", got)
	}
}

// Why (#992): the bar counts like the Last run card, as three separate parts that add up to the devices
// of the run. Total is the TRIED devices (backed up + failed) and the skipped ones come on top, so
// "36 of 41 backed up · 5 failed · 3 skipped" read as 41 devices although the run had 44. A run that
// only skipped devices backed up none, and says "0 backed up", not "0 of 0".
func TestStatusBackupCountsAddUpToTheDevicesOfTheRun(t *testing.T) {
	for _, c := range []struct {
		name string
		b    StatusBackup
		want string
	}{
		{"issue run: 41 tried, 5 failed, 3 skipped", StatusBackup{Total: 41, Failed: 5, Skipped: 3}, "36 backed up · 5 failed · 3 skipped"},
		{"skipped only", StatusBackup{Skipped: 2}, "0 backed up · 2 skipped"},
		{"nothing backed up", StatusBackup{Total: 4, Failed: 4}, "0 backed up · 4 failed"},
	} {
		if got := c.b.countText(); got != c.want || strings.Contains(got, " of ") {
			t.Errorf("%s: countText = %q, want %q", c.name, got, c.want)
		}
	}
}

// Why (#1077, A1-014): "All good" on a fleet with no device says nothing is wrong about a system that
// has not started yet, and it is the first thing a new person reads on the phone. An empty fleet says so.
// A fact that needs attention still wins: a broker that is down on an empty fleet is the thing to read.
func TestStatusBarSaysNoDevicesYetOnAnEmptyFleet(t *testing.T) {
	if text, _ := (StatusBar{NoDevices: true}).summary(); text != "No devices yet" {
		t.Errorf("empty fleet: %q, want No devices yet", text)
	}
	if text, _ := (StatusBar{}).summary(); text != "All good" {
		t.Errorf("a fleet with devices: %q, want All good", text)
	}
	down := StatusBar{NoDevices: true, Broker: &StatusBroker{Host: "broker:1883"}}
	if text, _ := down.summary(); text != "Broker disconnected" {
		t.Errorf("empty fleet, broker down: %q, want the broker to win", text)
	}
	failed := StatusBar{NoDevices: true, Backup: &StatusBackup{Total: 3, Failed: 3}}
	if text, _ := failed.summary(); text != "Last backup failed" {
		t.Errorf("empty fleet, backup failed: %q, want the failure to win", text)
	}
}

// Why (#1077, A1-014): the phone bar was a dot, a sentence and a bare caret that screen readers skip, so the
// one control that opens the sheet had no name of its own and nothing said it was a control. The summary
// carries the visible word "Status" beside the caret.
func TestStatusBarPhoneToggleIsNamedStatus(t *testing.T) {
	var b strings.Builder
	if err := StatusBarView(ShellData{Base: "/tf"}).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	sum := b.String()
	sum = sum[strings.Index(sum, `<summary>`):strings.Index(sum, `</summary>`)]
	if !strings.Contains(sum, `<span class="sb-tog">Status`) {
		t.Errorf("the phone toggle has no Status word: %s", sum)
	}
}
