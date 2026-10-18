package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDebugModeExpiresAfterThirtyMinutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), diagnosticSettingsFileName)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	settings, err := setDebugMode(path, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := settings.DebugUntil, now.Add(30*time.Minute); !got.Equal(want) {
		t.Fatalf("debug until = %s, want %s", got, want)
	}
	loaded, err := loadDiagnosticSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.DebugUntil.After(now.Add(29*time.Minute)) || loaded.DebugUntil.After(now.Add(30*time.Minute)) {
		t.Fatalf("unexpected debug expiry: %s", loaded.DebugUntil)
	}
}

func TestUsageRefreshSignalIsConsumedOnce(t *testing.T) {
	dir := t.TempDir()
	if err := requestUsageRefresh(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !consumeUsageRefreshRequest(dir) {
		t.Fatal("expected refresh signal")
	}
	if consumeUsageRefreshRequest(dir) {
		t.Fatal("refresh signal should be one-shot")
	}
}

func TestDiagnosticErrorSummaryDoesNotExposePaths(t *testing.T) {
	if got := privacySafeDiagnosticError(`open C:\Users\Person\secret\status.json: access denied`); got != "status_write" {
		t.Fatalf("unexpected category: %q", got)
	}
	if got := privacySafeDiagnosticError(`open C:\Users\Person\secret\private.txt: access denied`); got != "runtime_error" {
		t.Fatalf("unexpected fallback category: %q", got)
	}
}
