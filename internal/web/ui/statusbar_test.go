package ui

import (
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
	if got := partly.Backup.countText(); got != "22 of 24 backed up · 2 failed · 3 skipped" {
		t.Errorf("full text = %q", got)
	}
	calm := StatusBar{Backup: &StatusBackup{At: at, Total: 24, Skipped: 3}}
	if text, _ := calm.summary(); text != "All good" {
		t.Errorf("skipped only: %q, want All good", text)
	}
	if got := calm.Backup.countText(); got != "24 of 24 backed up · 3 skipped" {
		t.Errorf("skipped only text = %q", got)
	}
}
