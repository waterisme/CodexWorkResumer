//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegisterTaskbarCreatedMessage(t *testing.T) {
	first, err := registerTaskbarCreatedMessage()
	if err != nil || first == 0 {
		t.Fatalf("TaskbarCreated message registration failed: id=%d err=%v", first, err)
	}
	second, err := registerTaskbarCreatedMessage()
	if err != nil || second != first {
		t.Fatalf("TaskbarCreated message registration was not stable: first=%d second=%d err=%v", first, second, err)
	}
}

func TestTrayWndProcHandlesTaskbarCreated(t *testing.T) {
	const hwnd = uintptr(0x7a11)
	restored := false
	app := &trayApp{
		hwnd:            hwnd,
		taskbarCreated:  0xc011,
		restoreIconFunc: func() { restored = true },
	}
	trayApps.Store(hwnd, app)
	defer trayApps.Delete(hwnd)
	if got := trayWndProc(hwnd, app.taskbarCreated, 0, 0); got != 0 {
		t.Fatalf("TaskbarCreated handler returned %d, want 0", got)
	}
	if !restored {
		t.Fatal("TaskbarCreated message did not trigger icon restoration")
	}
}

func TestTraySingleClickRequestsMainWindowOnlyAfterDoubleClickWindowExpires(t *testing.T) {
	var state trayClickState
	if !state.onLeftButtonUp() {
		t.Fatal("single click did not arm the delayed main-window action")
	}
	if !state.onSingleClickTimer() {
		t.Fatal("single-click timer did not request the main window")
	}
	if state.onSingleClickTimer() {
		t.Fatal("single click requested the main window more than once")
	}
}

func TestTrayDoubleClickCancelsSingleClickActionAndSuppressesTrailingMouseUp(t *testing.T) {
	var state trayClickState
	if !state.onLeftButtonUp() {
		t.Fatal("first click did not arm the delayed action")
	}
	state.onDoubleClick()
	if state.onLeftButtonUp() {
		t.Fatal("trailing mouse-up after double click armed another main-window action")
	}
	if state.onSingleClickTimer() {
		t.Fatal("double click left a pending single-click action")
	}
	if !state.onLeftButtonUp() {
		t.Fatal("next independent single click was still suppressed")
	}
}

func TestTrayDoubleClickSuppressionExpiresWithoutTrailingMouseUp(t *testing.T) {
	var state trayClickState
	state.onDoubleClick()
	if state.onSingleClickTimer() {
		t.Fatal("double-click suppression timer requested a single-click action")
	}
	if !state.onLeftButtonUp() {
		t.Fatal("expired double-click suppression blocked the next independent click")
	}
}

func TestEnsureUTF8BOMAddsMissingMarker(t *testing.T) {
	bom := []byte{0xef, 0xbb, 0xbf}
	got := ensureUTF8BOM([]byte("param()"))
	if !bytes.HasPrefix(got, bom) {
		t.Fatal("UTF-8 BOM was not added")
	}
}

func TestEmbeddedSettingsScriptHasOneUTF8BOM(t *testing.T) {
	bom := []byte{0xef, 0xbb, 0xbf}
	got := ensureUTF8BOM([]byte(settingsPowerShell))
	if !bytes.HasPrefix(got, bom) {
		t.Fatal("embedded settings script is missing its UTF-8 BOM")
	}
	if bytes.HasPrefix(got[len(bom):], bom) {
		t.Fatal("embedded settings script contains a duplicate UTF-8 BOM")
	}
}

func TestRestoreTrayIconReaddsBeforeRefreshing(t *testing.T) {
	var events []string
	if !restoreTrayIcon(
		func() bool { events = append(events, "add"); return true },
		func() { events = append(events, "remove") },
		func() { events = append(events, "refresh") },
	) {
		t.Fatal("successful tray icon restore returned false")
	}
	if got := strings.Join(events, ","); got != "remove,add,refresh" {
		t.Fatalf("tray icon restore order = %q, want remove,add,refresh", got)
	}
}

func TestRestoreTrayIconDoesNotRefreshAfterAddFailure(t *testing.T) {
	refreshed := false
	if restoreTrayIcon(
		func() bool { return false },
		func() {},
		func() { refreshed = true },
	) {
		t.Fatal("failed tray icon restore returned true")
	}
	if refreshed {
		t.Fatal("failed tray icon restore refreshed a missing icon")
	}
}

func TestGoalEmptyResponseStoppedCount(t *testing.T) {
	retries := []ManagedRetry{
		{State: "stopped", StopReason: goalEmptyResponseStopReason},
		{State: "stopped", StopReason: "recovery_attempt_limit"},
		{State: "pending", StopReason: goalEmptyResponseStopReason},
	}
	if got := goalEmptyResponseStoppedCount(retries); got != 1 {
		t.Fatalf("goal-specific tray notification count = %d, want 1", got)
	}
}

func TestGoalEmptyResponseBlockFailedCount(t *testing.T) {
	retries := []ManagedRetry{
		{State: "stopped", StopReason: goalEmptyResponseBlockFailReason},
		{State: "stopped", StopReason: goalEmptyResponseStopReason},
		{State: "pending", StopReason: goalEmptyResponseBlockFailReason},
	}
	if got := goalEmptyResponseBlockFailedCount(retries); got != 1 {
		t.Fatalf("goal-block failure tray notification count = %d, want 1", got)
	}
}

func TestRetryLimitStoppedCountExcludesControllerStops(t *testing.T) {
	retries := []ManagedRetry{
		{State: "stopped", StopReason: "recovery_attempt_limit"},
		{State: "stopped", StopReason: "shared_app_server_disabled"},
		{State: "stopped", StopReason: "codex_not_running"},
		{State: "pending", StopReason: "consecutive_retry_limit"},
	}
	if got := retryLimitStoppedCount(retries); got != 1 {
		t.Fatalf("retry-limit notification count = %d, want 1", got)
	}
}

func TestTrayVisualSurfacesRateLimitAsDistinctState(t *testing.T) {
	snapshot := ManagementSnapshot{
		Running: true, RateLimitedRetries: 2,
		Retries: []ManagedRetry{{State: "pending", SecondsRemaining: 3500, WaitReason: "quota_reset"}},
	}
	iconState, tip := trayVisual(snapshot)
	if iconState != "rate_limited" || !strings.Contains(tip, "Rate Limited") || !strings.Contains(tip, "59 分鐘") {
		t.Fatalf("rate-limit tray visual = %q %q", iconState, tip)
	}
}

func TestTrayDoesNotExpediteProviderQuotaWaitFromPositivePercentage(t *testing.T) {
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	if err := writeJSONAtomic(configPath, defaultConfig()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	app := &trayApp{
		service:       &managementService{configPath: configPath, commandDir: filepath.Join(dataDir, "commands")},
		catalogQueued: make(map[string]time.Time),
	}
	app.queueDiscoveredRateLimits(ManagementSnapshot{
		AutomaticRecoverySupported: true,
		CodexUsage: UsageSnapshot{Available: true, Source: "account/rateLimits/read", FetchedAt: now.Format(time.RFC3339Nano),
			FiveHour: &UsageWindow{UsedPercent: 90, ResetsAt: now.Add(time.Hour).Unix()},
			Weekly:   &UsageWindow{UsedPercent: 14, ResetsAt: now.Add(4 * 24 * time.Hour).Unix()}},
		Retries: []ManagedRetry{{ThreadID: "019f9d5d-9c82-75b1-b7c0-20a658af0423", State: "pending",
			Class: classRateLimit, WaitReason: "quota_limited", CanRetryNow: true}},
	}, now)
	entries, err := os.ReadDir(app.service.commandDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("positive but contradictory percentage bypassed the quota wait: %v", entries)
	}
}

func TestDiscoveredQuotaResumeHonorsExplicitTaskPlan(t *testing.T) {
	selectedID := "019f9d33-41f6-70a3-ab98-df2196cb7aa7"
	otherID := "019f9d47-7e70-7120-9b69-776d8d25ae0e"
	selected := ManagedTask{ThreadID: selectedID, Status: "rate_limited", CanResume: true, RolloutPath: "rollout.jsonl"}
	other := ManagedTask{ThreadID: otherID, Status: "rate_limited", CanResume: true, RolloutPath: "rollout.jsonl"}
	if !shouldQueueDiscoveredRateLimit(other, defaultTaskPlan()) {
		t.Fatal("legacy installations without a task plan lost existing quota rescue")
	}
	plan := TaskPlan{Configured: true, OrderedThreadIDs: []string{selectedID, otherID}, SelectedThreadIDs: []string{selectedID}}
	if !shouldQueueDiscoveredRateLimit(selected, plan) || shouldQueueDiscoveredRateLimit(other, plan) {
		t.Fatal("explicit task plan did not constrain newly discovered quota rescues")
	}
	plan.SelectedThreadIDs = nil
	if shouldQueueDiscoveredRateLimit(selected, plan) {
		t.Fatal("clearing all checkboxes must disable newly discovered quota rescues")
	}
	selected.CanResume = false
	if shouldQueueDiscoveredRateLimit(selected, defaultTaskPlan()) {
		t.Fatal("a running/non-resumable task must never be queued by preselection")
	}
}

func TestTrayVisualShowsOptInCLIFallbackAsReady(t *testing.T) {
	snapshot := ManagementSnapshot{
		Running: true, ControllerState: "shared_app_server_disabled", CLIResumeFallbackEnabled: true,
	}
	iconState, tip := trayVisual(snapshot)
	if iconState != "waiting" || !strings.Contains(tip, "CLI 備援待命") {
		t.Fatalf("CLI fallback tray visual = %q %q", iconState, tip)
	}
	state := deriveUIState(ManagementSnapshot{Running: true, ControllerState: "codex_not_running", CLIResumeFallbackEnabled: true})
	if state.Code != "waiting" {
		t.Fatalf("CLI fallback readiness was reported as %s", state.Code)
	}
}

func TestTrayStatusColorsMatchStatusGuide(t *testing.T) {
	for _, definition := range uiStateCatalog {
		state := definition.Code
		if state == "error" {
			state = "stopped"
		}
		color, ok := trayStatusColors[state]
		if !ok {
			t.Fatalf("tray color missing for %s", definition.Code)
		}
		if got := fmt.Sprintf("#%06X", color); got != definition.Color {
			t.Fatalf("tray color for %s = %s, guide = %s", definition.Code, got, definition.Color)
		}
	}
}

func TestTrayVisualUsesRunningWaitingAndErrorStates(t *testing.T) {
	cases := []struct {
		name     string
		snapshot ManagementSnapshot
		want     string
	}{
		{name: "running", snapshot: ManagementSnapshot{Running: true, ActiveRetries: 1}, want: "Running"},
		{name: "waiting", snapshot: ManagementSnapshot{Running: true}, want: "Waiting"},
		{name: "error", snapshot: ManagementSnapshot{Running: false}, want: "Error"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, tip := trayVisual(test.snapshot)
			if !strings.Contains(tip, test.want) {
				t.Fatalf("tray tip %q does not contain %q", tip, test.want)
			}
		})
	}
}

func TestTrayDashboardCacheOmitsTickingCountdown(t *testing.T) {
	dataDir := t.TempDir()
	app := &trayApp{dataDir: dataDir}
	app.persistDashboard(ManagementSnapshot{
		UsageRefreshMinutes: 3, ResumeIntervalSeconds: 30,
		TaskPlan: TaskPlan{Version: 1, Revision: 2, Configured: true,
			OrderedThreadIDs:  []string{"019f9d33-41f6-70a3-ab98-df2196cb7aa7"},
			SelectedThreadIDs: []string{"019f9d33-41f6-70a3-ab98-df2196cb7aa7"}},
		CodexUsage:  UsageSnapshot{Adapter: adapterCodex, Available: true, FetchedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		Retries:     []ManagedRetry{{ThreadID: "thread", TaskType: "Codex", State: "pending", SecondsRemaining: 42}},
		Diagnostics: DiagnosticSnapshot{Version: appVersion, StartedAt: time.Now().Add(-time.Hour).Format(time.RFC3339Nano), UptimeSeconds: 3600, LogMaxBytes: maxLogBytes, LogFileCount: maxLogFiles},
	})
	data, err := os.ReadFile(filepath.Join(dataDir, "dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cached trayDashboardSnapshot
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatal(err)
	}
	if cached.ResumeIntervalSeconds != 30 || cached.UsageRefreshMinutes != 3 || len(cached.Retries) != 1 || cached.Retries[0].SecondsRemaining != 0 {
		t.Fatalf("unexpected dashboard cache: %+v", cached)
	}
	if cached.TaskPlan.Revision != 2 || !cached.TaskPlan.IsSelected("019f9d33-41f6-70a3-ab98-df2196cb7aa7") {
		t.Fatalf("dashboard did not publish task selection: %+v", cached.TaskPlan)
	}
	if len(cached.StatusCatalog) != 5 || len(cached.QuotaCatalog) != 3 {
		t.Fatalf("dashboard did not publish shared UI definitions: states=%d quotas=%d", len(cached.StatusCatalog), len(cached.QuotaCatalog))
	}
	if cached.Diagnostics.Version != appVersion || cached.Diagnostics.UptimeSeconds != 0 || cached.Diagnostics.LogFileCount != 3 {
		t.Fatalf("dashboard diagnostics were not stable or complete: %+v", cached.Diagnostics)
	}
}

func TestTrayVisualSurfacesMonitoringPause(t *testing.T) {
	iconState, tip := trayVisual(ManagementSnapshot{Running: true, MonitoringPaused: true, ActiveRetries: 1})
	if iconState != "paused" || !strings.Contains(tip, "Paused") {
		t.Fatalf("monitoring pause tray visual = %q %q", iconState, tip)
	}
}

func TestTraySnapshotRefreshIsBoundedAndChangeDriven(t *testing.T) {
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	if !traySnapshotDue(now, time.Time{}, false) {
		t.Fatal("initial tray snapshot was not requested")
	}
	if traySnapshotDue(now.Add(4*time.Second), now, false) {
		t.Fatal("unchanged tray snapshot refreshed before five seconds")
	}
	if !traySnapshotDue(now.Add(time.Second), now, true) {
		t.Fatal("state change or manual refresh signal did not force a snapshot")
	}
	if !traySnapshotDue(now.Add(5*time.Second), now, false) {
		t.Fatal("five-second fallback snapshot was skipped")
	}
	if !traySnapshotDue(now.Add(-time.Second), now, false) {
		t.Fatal("clock change did not force a new snapshot")
	}
}

func TestTrayInputChangeForcesEarlySnapshot(t *testing.T) {
	dataDir := t.TempDir()
	app := &trayApp{dataDir: dataDir}
	app.inputStamps = app.currentInputStamps()
	if app.inputsChanged() {
		t.Fatal("unchanged input files requested a new snapshot")
	}
	if err := os.WriteFile(filepath.Join(dataDir, "state.json"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !app.inputsChanged() {
		t.Fatal("runtime state change was not detected")
	}
	app.inputStamps = app.currentInputStamps()
	if app.inputsChanged() {
		t.Fatal("consumed runtime state change was detected twice")
	}
	if err := os.WriteFile(filepath.Join(dataDir, "control.json"), []byte("paused"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !app.inputsChanged() {
		t.Fatal("control change was not detected")
	}
}

func TestTrayCountdownUpdatesWithoutMutatingCachedSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	snapshot := ManagementSnapshot{Running: true, PendingRetries: 1, Retries: []ManagedRetry{
		{State: "pending", DueAt: now.Add(12 * time.Second).Format(time.RFC3339Nano), SecondsRemaining: 12},
	}}
	updated := traySnapshotWithCountdown(snapshot, now.Add(3*time.Second))
	if updated.Retries[0].SecondsRemaining != 9 {
		t.Fatalf("local countdown = %d, want 9", updated.Retries[0].SecondsRemaining)
	}
	if snapshot.Retries[0].SecondsRemaining != 12 {
		t.Fatal("local countdown changed the cached full snapshot")
	}
	if _, tip := trayVisual(updated); !strings.Contains(tip, "9 秒後 Resume") {
		t.Fatalf("local countdown was not reflected in the tray tooltip: %q", tip)
	}
	updated = traySnapshotWithCountdown(snapshot, now.Add(13*time.Second))
	if updated.Retries[0].SecondsRemaining != 0 {
		t.Fatalf("expired local countdown = %d, want 0", updated.Retries[0].SecondsRemaining)
	}
}

func TestCompletedActiveRetriesOnlyCountsDisappearedActiveTasks(t *testing.T) {
	previous := map[string]struct{}{"done": {}, "failed": {}, "running": {}}
	completed, current := completedActiveRetries(previous, []ManagedRetry{
		{ThreadID: "failed", State: "pending"},
		{ThreadID: "running", State: "running"},
		{ThreadID: "new", State: "starting"},
	})
	if completed != 1 {
		t.Fatalf("completed retries = %d, want 1", completed)
	}
	if len(current) != 2 {
		t.Fatalf("active retry set = %v", current)
	}
}
