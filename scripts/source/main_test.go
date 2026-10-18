package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRunLocalControlSetSchedulerSettingsPreservesOtherConfig(t *testing.T) {
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	before := defaultConfig()
	before.MaxParallelRetries = 7
	before.RetryPrompt = "test fallback"
	before.SharedAppServerRequested = true
	if err := writeJSONAtomic(configPath, before); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_AUTO_RETRY_QUOTA_RESUME_MODE", quotaModeSequentialCompletion)
	t.Setenv("CODEX_AUTO_RETRY_RESUME_INTERVAL_SECONDS", "45")
	t.Setenv("CODEX_AUTO_RETRY_MAX_PARALLEL_SESSIONS", "3")
	t.Setenv("CODEX_AUTO_RETRY_USAGE_REFRESH_MINUTES", "5")
	if err := runLocalControl(dataDir, "set_scheduler_settings", ""); err != nil {
		t.Fatal(err)
	}
	got, err := loadOrCreateConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	want := before
	want.QuotaResumeMode = quotaModeSequentialCompletion
	want.ResumeIntervalSeconds = 45
	want.MaxParallelSessions = 3
	want.UsageRefreshMinutes = 5
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scheduler update changed unrelated config:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestRunLocalControlSetSchedulerSettingsRejectsInvalidAtomically(t *testing.T) {
	cases := []struct {
		name, key, value string
	}{
		{"mode", "CODEX_AUTO_RETRY_QUOTA_RESUME_MODE", "unknown"},
		{"interval", "CODEX_AUTO_RETRY_RESUME_INTERVAL_SECONDS", "-1"},
		{"parallel", "CODEX_AUTO_RETRY_MAX_PARALLEL_SESSIONS", "0"},
		{"refresh", "CODEX_AUTO_RETRY_USAGE_REFRESH_MINUTES", "0"},
		{"not_a_number", "CODEX_AUTO_RETRY_RESUME_INTERVAL_SECONDS", "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			configPath := filepath.Join(dataDir, "config.json")
			if err := writeJSONAtomic(configPath, defaultConfig()); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_AUTO_RETRY_QUOTA_RESUME_MODE", quotaModeStaggeredStart)
			t.Setenv("CODEX_AUTO_RETRY_RESUME_INTERVAL_SECONDS", "30")
			t.Setenv("CODEX_AUTO_RETRY_MAX_PARALLEL_SESSIONS", "2")
			t.Setenv("CODEX_AUTO_RETRY_USAGE_REFRESH_MINUTES", "1")
			t.Setenv(tc.key, tc.value)
			if err := runLocalControl(dataDir, "set_scheduler_settings", ""); err == nil {
				t.Fatal("invalid scheduler settings were accepted")
			}
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(original) {
				t.Fatal("invalid scheduler settings modified config.json")
			}
		})
	}
}

func TestRunLocalControlSetPausedPreservesMonitoringPause(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "control.json")
	if err := writeJSONAtomic(path, ControlState{Version: currentControlVersion, MonitoringPaused: true}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_AUTO_RETRY_PAUSED", "true")
	if err := runLocalControl(dataDir, "set_paused", ""); err != nil {
		t.Fatal(err)
	}
	got, err := loadOrCreateControlState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Paused || !got.MonitoringPaused {
		t.Fatalf("set_paused overwrote monitoring pause: %+v", got)
	}
}

func TestRunLocalControlConfirmTaskCompleteQueuesCommand(t *testing.T) {
	dataDir := t.TempDir()
	const threadID = "12345678-1234-4234-8234-123456789abc"
	if err := runLocalControl(dataDir, string(commandConfirmTaskComplete), threadID); err != nil {
		t.Fatal(err)
	}
	commands, invalid, err := loadControlCommandFiles(filepath.Join(dataDir, "commands"))
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) != 0 || len(commands) != 1 || commands[0].Command.Action != commandConfirmTaskComplete || commands[0].Command.ThreadID != threadID {
		t.Fatalf("unexpected queued complete command: commands=%+v invalid=%v", commands, invalid)
	}
}

func TestReconcileSharedBackendPreferenceAtStartupEnablesSavedChoice(t *testing.T) {
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	config := defaultConfig()
	config.SharedAppServerRequested = true
	config.SharedAppServerEnabled = false
	if err := writeJSONAtomic(configPath, config); err != nil {
		t.Fatal(err)
	}

	previousEnable := enableSharedAppServerAtStartup
	previousDisable := disableSharedAppServerAtStartup
	t.Cleanup(func() {
		enableSharedAppServerAtStartup = previousEnable
		disableSharedAppServerAtStartup = previousDisable
	})
	enableCalls := 0
	enableSharedAppServerAtStartup = func(_ context.Context, gotDataDir string, got Config) (Config, error) {
		enableCalls++
		if gotDataDir != dataDir || !got.SharedAppServerRequested || got.SharedAppServerEnabled {
			t.Fatalf("unexpected startup enable request: data=%q config=%+v", gotDataDir, got)
		}
		got.SharedAppServerPort = 51234
		return got, nil
	}
	disableSharedAppServerAtStartup = func(context.Context, string, Config) error {
		t.Fatal("startup enable unexpectedly called disable")
		return nil
	}
	for _, name := range []string{"shared-availability.json", sharedFailOpenMarkerName} {
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := reconcileSharedBackendPreferenceAtStartup(context.Background(), configPath, dataDir, config)
	if err != nil {
		t.Fatal(err)
	}
	if enableCalls != 1 || !got.SharedAppServerRequested || !got.SharedAppServerEnabled || got.SharedAppServerPort != 51234 {
		t.Fatalf("saved enabled preference was not applied exactly once: calls=%d config=%+v", enableCalls, got)
	}
	for _, name := range []string{"shared-availability.json", sharedFailOpenMarkerName} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("startup enable retained %s: %v", name, err)
		}
	}
}

func TestReconcileSharedBackendPreferenceAtStartupPersistsDisableBeforeCleanup(t *testing.T) {
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	config := defaultConfig()
	config.SharedAppServerRequested = false
	config.SharedAppServerEnabled = true
	if err := writeJSONAtomic(configPath, config); err != nil {
		t.Fatal(err)
	}

	previousEnable := enableSharedAppServerAtStartup
	previousDisable := disableSharedAppServerAtStartup
	t.Cleanup(func() {
		enableSharedAppServerAtStartup = previousEnable
		disableSharedAppServerAtStartup = previousDisable
	})
	enableSharedAppServerAtStartup = func(context.Context, string, Config) (Config, error) {
		t.Fatal("startup disable unexpectedly called enable")
		return Config{}, nil
	}
	cleanupErr := errors.New("cleanup deferred for test")
	disableSharedAppServerAtStartup = func(_ context.Context, _ string, got Config) error {
		if got.SharedAppServerRequested || got.SharedAppServerEnabled {
			t.Fatalf("disable cleanup ran before the safe state was persisted: %+v", got)
		}
		return cleanupErr
	}

	got, err := reconcileSharedBackendPreferenceAtStartup(context.Background(), configPath, dataDir, config)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup error = %v, want %v", err, cleanupErr)
	}
	if got.SharedAppServerRequested || got.SharedAppServerEnabled {
		t.Fatalf("saved disabled preference was not retained after deferred cleanup: %+v", got)
	}
	persisted, readErr := loadOrCreateConfig(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if persisted.SharedAppServerRequested || persisted.SharedAppServerEnabled {
		t.Fatalf("disabled preference was not durable: %+v", persisted)
	}
}

func TestReconcileSharedBackendPreferenceAtStartupSkipsMatchingState(t *testing.T) {
	config := defaultConfig()
	previousEnable := enableSharedAppServerAtStartup
	previousDisable := disableSharedAppServerAtStartup
	t.Cleanup(func() {
		enableSharedAppServerAtStartup = previousEnable
		disableSharedAppServerAtStartup = previousDisable
	})
	enableSharedAppServerAtStartup = func(context.Context, string, Config) (Config, error) {
		t.Fatal("matching disabled state called enable")
		return Config{}, nil
	}
	disableSharedAppServerAtStartup = func(context.Context, string, Config) error {
		t.Fatal("matching disabled state called disable")
		return nil
	}
	if _, err := reconcileSharedBackendPreferenceAtStartup(context.Background(), "", "", config); err != nil {
		t.Fatal(err)
	}
}
