package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	currentDiagnosticSettingsVersion = 1
	debugModeDuration                = 30 * time.Minute
	diagnosticSettingsFileName       = "diagnostics.json"
	usageRefreshSignalFileName       = "usage-refresh.signal"
	taskScanSignalFileName           = "task-scan.signal"
	taskCatalogRefreshSignalFileName = "task-catalog-refresh.signal"
)

type DiagnosticSettings struct {
	Version    int       `json:"version"`
	DebugUntil time.Time `json:"debug_until,omitempty"`
}

type DiagnosticSnapshot struct {
	Version             string `json:"version"`
	Build               int    `json:"build"`
	StartedAt           string `json:"started_at,omitempty"`
	UptimeSeconds       int64  `json:"uptime_seconds"`
	ServiceState        string `json:"service_state"`
	AppServerConnection string `json:"app_server_connection"`
	MonitoringState     string `json:"monitoring_state"`
	AutoResumeState     string `json:"auto_resume_state"`
	LastTaskScanAt      string `json:"last_task_scan_at,omitempty"`
	UsageSource         string `json:"usage_source"`
	LastUsageAttemptAt  string `json:"last_usage_attempt_at,omitempty"`
	LastUsageRefreshAt  string `json:"last_usage_refresh_at,omitempty"`
	NextUsageRefreshAt  string `json:"next_usage_refresh_at,omitempty"`
	NextResumeAt        string `json:"next_resume_at,omitempty"`
	RecoveryTaskCount   int    `json:"recovery_task_count"`
	WatchedRoots        int    `json:"watched_roots"`
	RecentError         string `json:"recent_error,omitempty"`
	LogPath             string `json:"log_path"`
	LogMaxBytes         int64  `json:"log_max_bytes"`
	LogFileCount        int    `json:"log_file_count"`
	DebugEnabled        bool   `json:"debug_enabled"`
	DebugUntil          string `json:"debug_until,omitempty"`
}

func formatDiagnosticTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func earliestResumeAt(retries []ManagedRetry) string {
	earliest := time.Time{}
	for _, retry := range retries {
		if retry.State != "pending" || retry.DueAt == "" {
			continue
		}
		dueAt, err := time.Parse(time.RFC3339Nano, retry.DueAt)
		if err == nil && (earliest.IsZero() || dueAt.Before(earliest)) {
			earliest = dueAt
		}
	}
	return formatDiagnosticTime(earliest)
}

func privacySafeDiagnosticError(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return ""
	}
	for _, item := range []struct {
		contains string
		category string
	}{
		{"rate", "rate_limit"},
		{"quota", "rate_limit"},
		{"timeout", "timeout"},
		{"network", "network"},
		{"connection", "network"},
		{"auth", "authentication"},
		{"config", "configuration"},
		{"control", "control_state"},
		{"scan", "session_scan"},
		{"status", "status_write"},
		{"state", "state_write"},
		{"memory", "memory_guard"},
	} {
		if strings.Contains(normalized, item.contains) {
			return item.category
		}
	}
	return "runtime_error"
}

func defaultDiagnosticSettings() DiagnosticSettings {
	return DiagnosticSettings{Version: currentDiagnosticSettingsVersion}
}

func loadDiagnosticSettings(path string) (DiagnosticSettings, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultDiagnosticSettings(), nil
	}
	if err != nil {
		return DiagnosticSettings{}, err
	}
	settings := defaultDiagnosticSettings()
	if err := json.Unmarshal(data, &settings); err != nil {
		return DiagnosticSettings{}, fmt.Errorf("parse diagnostic settings: %w", err)
	}
	if settings.Version != currentDiagnosticSettingsVersion {
		return DiagnosticSettings{}, fmt.Errorf("diagnostic settings version must be %d", currentDiagnosticSettingsVersion)
	}
	settings.DebugUntil = settings.DebugUntil.UTC()
	return settings, nil
}

func setDebugMode(path string, enabled bool, now time.Time) (DiagnosticSettings, error) {
	settings := defaultDiagnosticSettings()
	if enabled {
		settings.DebugUntil = now.UTC().Add(debugModeDuration)
	}
	if err := writeJSONAtomic(path, settings); err != nil {
		return DiagnosticSettings{}, err
	}
	return settings, nil
}

func requestUsageRefresh(dataDir string, now time.Time) error {
	return os.WriteFile(
		filepath.Join(dataDir, usageRefreshSignalFileName),
		[]byte(now.UTC().Format(time.RFC3339Nano)+"\n"),
		0o600,
	)
}

func consumeUsageRefreshRequest(dataDir string) bool {
	path := filepath.Join(dataDir, usageRefreshSignalFileName)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return os.Remove(path) == nil
}

func requestTaskScan(dataDir string, now time.Time) error {
	payload := []byte(now.UTC().Format(time.RFC3339Nano) + "\n")
	if err := os.WriteFile(
		filepath.Join(dataDir, taskScanSignalFileName),
		payload,
		0o600,
	); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, taskCatalogRefreshSignalFileName), payload, 0o600)
}

func consumeTaskScanRequest(dataDir string) bool {
	path := filepath.Join(dataDir, taskScanSignalFileName)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return os.Remove(path) == nil
}

func consumeTaskCatalogRefreshRequest(dataDir string) bool {
	path := filepath.Join(dataDir, taskCatalogRefreshSignalFileName)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return os.Remove(path) == nil
}

func runLocalDiagnostics(dataDir, action string, now time.Time) error {
	action = strings.TrimSpace(action)
	if action == "" {
		action = strings.TrimSpace(os.Getenv("CODEX_AUTO_RETRY_ACTION"))
	}
	switch action {
	case "enable_debug":
		_, err := setDebugMode(filepath.Join(dataDir, diagnosticSettingsFileName), true, now)
		return err
	case "disable_debug":
		_, err := setDebugMode(filepath.Join(dataDir, diagnosticSettingsFileName), false, now)
		return err
	case "refresh_usage":
		return requestUsageRefresh(dataDir, now)
	case "refresh_tasks":
		return requestTaskScan(dataDir, now)
	case "set_task_scan":
		enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("CODEX_AUTO_RETRY_AUTO_SCAN_ENABLED")))
		if err != nil {
			return fmt.Errorf("invalid automatic scan setting")
		}
		seconds, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CODEX_AUTO_RETRY_SCAN_INTERVAL_SECONDS")))
		if err != nil {
			return fmt.Errorf("invalid scan interval")
		}
		_, err = updateConfigFile(filepath.Join(dataDir, "config.json"), func(config *Config) error {
			config.AutoScanEnabled = enabled
			config.ScanIntervalSeconds = seconds
			return nil
		})
		return err
	default:
		return fmt.Errorf("unsupported diagnostic action")
	}
}
