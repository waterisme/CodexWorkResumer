package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ManagedRetry struct {
	ThreadID              string       `json:"thread_id" jsonschema:"Codex task identifier"`
	Label                 string       `json:"label" jsonschema:"task label derived from its project folder and session identifier"`
	ProjectPath           string       `json:"project_path,omitempty" jsonschema:"project working directory recorded by Codex"`
	TaskType              string       `json:"task_type" jsonschema:"task adapter kind; currently Codex"`
	LastActivityAt        string       `json:"last_activity_at,omitempty" jsonschema:"most recent known task activity in RFC 3339 format"`
	State                 string       `json:"state" jsonschema:"pending, starting, or running"`
	Class                 FailureClass `json:"class" jsonschema:"provider failure category"`
	DueAt                 string       `json:"due_at,omitempty" jsonschema:"next retry time in RFC 3339 format"`
	SecondsRemaining      int64        `json:"seconds_remaining" jsonschema:"whole seconds until the retry is due"`
	RecoveryAttempt       int          `json:"recovery_attempt" jsonschema:"current attempt in this fault recovery cycle"`
	MaxRecoveryAttempts   int          `json:"max_recovery_attempts,omitempty" jsonschema:"maximum attempts in this fault recovery cycle"`
	ConsecutiveRetry      int          `json:"consecutive_retry" jsonschema:"current retry without visible assistant progress"`
	MaxConsecutiveRetries int          `json:"max_consecutive_retries,omitempty" jsonschema:"maximum retries without visible assistant progress"`
	Action                RetryAction  `json:"action,omitempty" jsonschema:"current recovery action"`
	CanRetryNow           bool         `json:"can_retry_now" jsonschema:"whether retry-now is currently available"`
	CanCancel             bool         `json:"can_cancel" jsonschema:"whether the pending retry can be cancelled"`
	CanRestart            bool         `json:"can_restart" jsonschema:"whether an exhausted retry can be restarted with a fresh budget"`
	StopReason            string       `json:"stop_reason,omitempty" jsonschema:"privacy-safe reason why retries stopped"`
	WaitReason            string       `json:"wait_reason,omitempty" jsonschema:"privacy-safe reason why a pending retry is waiting"`
}

// Stopped entries are useful immediately after a limit is reached because
// they explain why recovery stopped. They are not executable queue items,
// though, and keeping every historical stop in the queue makes an otherwise
// single-task session look like many pending jobs. Keep a short visible window
// for the explicit reason, while retaining the durable state for diagnostics.
const stoppedRetryDisplayWindow = time.Hour

// Keep countdowns responsive without reparsing the potentially large runtime
// state on every one-second tray refresh. A bounded reread catches changes on
// filesystems with coarse timestamps even if size and mtime appear unchanged.
const managementStateCacheMaxAge = 5 * time.Second

type ManagementSnapshot struct {
	Version                             string             `json:"version" jsonschema:"watchdog version"`
	Build                               int                `json:"build" jsonschema:"monotonically increasing release build number"`
	Running                             bool               `json:"running" jsonschema:"whether a fresh watchdog heartbeat exists"`
	HeartbeatStale                      bool               `json:"heartbeat_stale" jsonschema:"whether the last heartbeat is too old"`
	Paused                              bool               `json:"paused" jsonschema:"whether new retry dispatches are paused"`
	MonitoringPaused                    bool               `json:"monitoring_paused" jsonschema:"whether task scanning and quota refresh are paused"`
	RetryPrompt                         string             `json:"retry_prompt" jsonschema:"fallback message used only when silent continuation is unsupported"`
	MaxConsecutiveRetries               int                `json:"max_consecutive_retries" jsonschema:"maximum retries without visible assistant progress"`
	MaxRecoveryAttempts                 int                `json:"max_recovery_attempts" jsonschema:"maximum attempts in one fault recovery cycle"`
	InitialDelaySeconds                 int                `json:"initial_delay_seconds" jsonschema:"delay before the first automatic retry"`
	MaxDelaySeconds                     int                `json:"max_delay_seconds" jsonschema:"maximum cap for increasing retry delays"`
	DelayIncrementSeconds               int                `json:"delay_increment_seconds" jsonschema:"seconds added after each linear retry"`
	DelayStrategy                       string             `json:"delay_strategy" jsonschema:"fixed, exponential, or linear retry delay"`
	ResumeIntervalSeconds               int                `json:"resume_interval_seconds" jsonschema:"minimum seconds between starting different task resumes"`
	QuotaResumeMode                     string             `json:"quota_resume_mode" jsonschema:"staggered_start or sequential_completion for quota recovery only"`
	MaxParallelSessions                 int                `json:"max_parallel_sessions" jsonschema:"best-effort maximum observed running sessions for staggered quota dispatch"`
	SchedulerOwnerThreadID              string             `json:"scheduler_owner_thread_id,omitempty" jsonschema:"quota task currently held by sequential scheduling"`
	SchedulerOwnerCanConfirm            bool               `json:"scheduler_owner_can_confirm" jsonschema:"deprecated compatibility flag for older clients; Build 13 automatically hands off after verified quiet completion"`
	UsageRefreshMinutes                 int                `json:"usage_refresh_minutes" jsonschema:"minutes between quota refresh attempts"`
	AutoScanEnabled                     bool               `json:"auto_scan_enabled" jsonschema:"whether new Codex session events are scanned automatically"`
	ScanIntervalSeconds                 int                `json:"scan_interval_seconds" jsonschema:"seconds between automatic session scans"`
	ShowNotifications                   bool               `json:"show_notifications" jsonschema:"whether Windows notifications are enabled"`
	MemoryLimitMB                       int                `json:"memory_limit_mb" jsonschema:"private memory limit for the watchdog process, from 128 to 65536 MB"`
	MemoryUsageMB                       int64              `json:"memory_usage_mb" jsonschema:"current watchdog private memory in MB"`
	MemoryGuardTriggered                bool               `json:"memory_guard_triggered" jsonschema:"whether the memory guard stopped the service"`
	SharedAppServerMemoryUsageMB        int64              `json:"shared_app_server_memory_usage_mb" jsonschema:"current shared Codex app-server private memory in MB"`
	SharedAppServerMemoryLimitMB        int                `json:"shared_app_server_memory_limit_mb" jsonschema:"monitor-only private memory limit for the shared Codex app-server"`
	SharedAppServerMemoryGuardTriggered bool               `json:"shared_app_server_memory_guard_triggered" jsonschema:"whether shared mode was disabled after its app-server exceeded the monitor limit"`
	RetrySafetyWarning                  string             `json:"retry_safety_warning,omitempty" jsonschema:"warning when retry limits are unusually aggressive"`
	SharedAppServerPort                 int                `json:"shared_app_server_port" jsonschema:"loopback port used by the optional shared Codex app-server"`
	SharedAppServerEnabled              bool               `json:"shared_app_server_enabled" jsonschema:"whether the optional shared Codex app-server recovery mode is enabled"`
	SharedAppServerRequested            bool               `json:"shared_app_server_requested" jsonschema:"whether the user wants shared recovery mode, including when temporarily unavailable"`
	CLIResumeFallbackEnabled            bool               `json:"cli_resume_fallback_enabled" jsonschema:"whether background Codex CLI resume is allowed when the primary desktop route is unavailable"`
	DesktopTransport                    string             `json:"desktop_transport" jsonschema:"official_stdio, shared_websocket, stopped, or unknown"`
	RecoveryMode                        string             `json:"recovery_mode" jsonschema:"shared_websocket, safe_launcher_required, or none"`
	AutomaticRecoverySupported          bool               `json:"automatic_recovery_supported" jsonschema:"whether the current Desktop transport accepts automatic recovery"`
	RecoveryCapabilityReason            string             `json:"recovery_capability_reason" jsonschema:"privacy-safe capability reason"`
	StartupApproved                     string             `json:"startup_approved" jsonschema:"Windows sign-in approval state for the CodexAutoRetry startup entry"`
	Now                                 string             `json:"now" jsonschema:"snapshot time in RFC 3339 format"`
	LastScanAt                          string             `json:"last_scan_at,omitempty" jsonschema:"last session scan time in RFC 3339 format"`
	PendingRetries                      int                `json:"pending_retries" jsonschema:"number of retries waiting to dispatch"`
	ActiveRetries                       int                `json:"active_retries" jsonschema:"number of retries starting or running"`
	StoppedRetries                      int                `json:"stopped_retries" jsonschema:"number of currently visible retry chains that have stopped"`
	RateLimitedRetries                  int                `json:"rate_limited_retries" jsonschema:"number of retries waiting for quota data or reset"`
	WatchedRoots                        int                `json:"watched_roots" jsonschema:"number of watched Codex session roots"`
	LastError                           string             `json:"last_error,omitempty" jsonschema:"privacy-safe watchdog error summary"`
	ControllerState                     string             `json:"controller_state,omitempty" jsonschema:"background Codex controller state"`
	Notice                              string             `json:"notice,omitempty" jsonschema:"result of the most recent management action"`
	CodexUsage                          UsageSnapshot      `json:"codex_usage" jsonschema:"current Codex five-hour and weekly quota windows"`
	TaskCatalogAvailable                bool               `json:"task_catalog_available" jsonschema:"whether the official Codex task catalog is available"`
	TaskCatalogRefreshing               bool               `json:"task_catalog_refreshing" jsonschema:"whether the task catalog is being refreshed"`
	TaskCatalogUnavailableReason        string             `json:"task_catalog_unavailable_reason,omitempty" jsonschema:"privacy-safe task catalog failure reason"`
	TaskPlan                            TaskPlan           `json:"task_plan" jsonschema:"persistent task selection and visible order; selection alone never starts a task"`
	TaskPlanError                       string             `json:"task_plan_error,omitempty" jsonschema:"privacy-safe error if persistent task selection is invalid"`
	Tasks                               []ManagedTask      `json:"tasks" jsonschema:"recent Codex tasks plus all active recovery tasks"`
	Adapters                            []AdapterStatus    `json:"adapters" jsonschema:"available task adapter capabilities"`
	Retries                             []ManagedRetry     `json:"retries" jsonschema:"current retry queue"`
	Diagnostics                         DiagnosticSnapshot `json:"diagnostics" jsonschema:"privacy-safe local runtime diagnostics"`
}

type managementService struct {
	dataDir           string
	configPath        string
	controlPath       string
	taskPlanPath      string
	commandDir        string
	statePath         string
	statusPath        string
	usage             *usageMonitor
	catalog           *taskCatalogMonitor
	taskIdentity      map[string]managedTaskIdentity
	stateCache        RuntimeState
	stateCacheAt      time.Time
	stateCacheModTime time.Time
	stateCacheSize    int64
	stateCacheExists  bool
	mu                sync.Mutex
}

type managedTaskIdentity struct {
	label       string
	projectPath string
}

var newCodexUsageAdapter = func(endpoint string, config Config) TaskAdapter {
	return NewCodexAdapterForConfig(endpoint, config)
}

func newManagementService(dataDir string) *managementService {
	return &managementService{
		dataDir:      dataDir,
		configPath:   filepath.Join(dataDir, "config.json"),
		controlPath:  filepath.Join(dataDir, "control.json"),
		taskPlanPath: filepath.Join(dataDir, "task-plan.json"),
		commandDir:   filepath.Join(dataDir, "commands"),
		statePath:    filepath.Join(dataDir, "state.json"),
		statusPath:   filepath.Join(dataDir, "status.json"),
		usage:        newUsageMonitor(defaultUsageRefresh),
		catalog:      newTaskCatalogMonitor(time.Minute),
		taskIdentity: make(map[string]managedTaskIdentity),
	}
}

func (m *managementService) snapshot(now time.Time) (ManagementSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked(now.UTC(), false)
}

func (m *managementService) snapshotForTray(now time.Time) (ManagementSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked(now.UTC(), true)
}

func (m *managementService) snapshotLocked(now time.Time, asyncUsage bool) (ManagementSnapshot, error) {
	config, err := loadOrCreateConfig(m.configPath)
	if err != nil {
		return ManagementSnapshot{}, err
	}
	control, err := loadOrCreateControlState(m.controlPath)
	if err != nil {
		return ManagementSnapshot{}, err
	}
	taskPlan, taskPlanErr := loadTaskPlan(m.taskPlanPath)
	if taskPlanErr != nil {
		// A damaged plan must not be mistaken for an unconfigured (legacy)
		// plan. Show zero selected rows and keep the error visible.
		taskPlan = defaultTaskPlan()
		taskPlan.Configured = true
	}
	state, err := m.loadStateCached(now)
	if err != nil {
		return ManagementSnapshot{}, err
	}
	if config.QuotaResumeMode != quotaModeSequentialCompletion {
		// A mode switch releases the hold on the daemon's next tick. Do not
		// present the old owner as still actionable in the interim snapshot.
		state.QuotaSchedulerOwnerID = ""
	}
	status, statusFound, err := loadStatusSnapshot(m.statusPath)
	if err != nil {
		return ManagementSnapshot{}, err
	}

	staleAfter := time.Duration(config.PollIntervalSeconds*4) * time.Second
	if staleAfter < 30*time.Second {
		staleAfter = 30 * time.Second
	}
	processRunning := statusFound && processOwnsRuntime(status.PID, m.dataDir)
	heartbeatAt := status.LastHeartbeatAt
	if heartbeatAt.IsZero() {
		heartbeatAt = status.LastScanAt
	}
	heartbeatStale := !statusFound || !processRunning || heartbeatAt.IsZero() || now.Sub(heartbeatAt) > staleAfter
	running := statusFound && status.Running && !heartbeatStale
	retries := m.managedRetries(state, now)
	visibleRetries := retries[:0]
	pending, active, stopped, rateLimited := 0, 0, 0, 0
	for _, retry := range retries {
		if !running && retry.State != "stopped" {
			continue
		}
		visibleRetries = append(visibleRetries, retry)
		if retry.State == "pending" {
			pending++
			if isQuotaWaitReason(retry.WaitReason) {
				rateLimited++
			}
		} else if retry.State == "stopped" {
			stopped++
		} else {
			active++
		}
	}
	retries = visibleRetries

	snapshot := ManagementSnapshot{
		Version:                      appVersion,
		Build:                        appBuild,
		Running:                      running,
		HeartbeatStale:               heartbeatStale,
		Paused:                       control.Paused,
		MonitoringPaused:             control.MonitoringPaused,
		RetryPrompt:                  config.RetryPrompt,
		MaxConsecutiveRetries:        config.MaxConsecutiveRetries,
		MaxRecoveryAttempts:          config.MaxRecoveryAttempts,
		InitialDelaySeconds:          config.InitialDelaySeconds,
		MaxDelaySeconds:              config.MaxDelaySeconds,
		DelayIncrementSeconds:        config.DelayIncrementSeconds,
		DelayStrategy:                config.DelayStrategy,
		ResumeIntervalSeconds:        config.ResumeIntervalSeconds,
		QuotaResumeMode:              config.QuotaResumeMode,
		MaxParallelSessions:          config.MaxParallelSessions,
		SchedulerOwnerThreadID:       state.QuotaSchedulerOwnerID,
		SchedulerOwnerCanConfirm:     schedulerOwnerCanConfirm(state, config, running && !control.MonitoringPaused),
		UsageRefreshMinutes:          config.UsageRefreshMinutes,
		AutoScanEnabled:              config.AutoScanEnabled,
		ScanIntervalSeconds:          config.ScanIntervalSeconds,
		ShowNotifications:            config.ShowNotifications,
		MemoryLimitMB:                config.MemoryLimitMB,
		SharedAppServerMemoryLimitMB: config.SharedAppServerMemoryLimitMB,
		RetrySafetyWarning:           config.retrySafetyWarning(),
		SharedAppServerPort:          config.SharedAppServerPort,
		SharedAppServerEnabled:       config.SharedAppServerEnabled,
		SharedAppServerRequested:     config.SharedAppServerRequested,
		CLIResumeFallbackEnabled:     config.CLIResumeFallbackEnabled,
		DesktopTransport:             "unknown",
		RecoveryMode:                 "none",
		StartupApproved:              readStartupApprovalStatus(),
		Now:                          now.Format(time.RFC3339Nano),
		PendingRetries:               pending,
		ActiveRetries:                active,
		StoppedRetries:               stopped,
		RateLimitedRetries:           rateLimited,
		CodexUsage:                   unavailableUsage(adapterCodex, "app_server_unavailable"),
		Adapters:                     []AdapterStatus{NewCodexAdapter("").Status(), NewWorkAdapter().Status()},
		Retries:                      retries,
		TaskPlan:                     taskPlan,
		Tasks:                        applyRecoveryResults(ensureSchedulerOwnerTask(applyTaskPlan(mergeManagedTasks(TaskCatalogSnapshot{}, retries), taskPlan), state), state),
	}
	if taskPlanErr != nil {
		snapshot.TaskPlanError = "task_plan_invalid"
	}
	if !control.MonitoringPaused {
		endpoint := ""
		if config.SharedAppServerEnabled {
			endpoint = adapterEndpoint(config.SharedAppServerPort)
		}
		adapter := newCodexUsageAdapter(endpoint, config)
		m.usage.SetRefreshInterval(time.Duration(config.UsageRefreshMinutes) * time.Minute)
		if asyncUsage {
			snapshot.CodexUsage = m.usage.SnapshotAsync(now, adapter)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			snapshot.CodexUsage = m.usage.Snapshot(ctx, now, adapter)
			cancel()
		}
		codexStatus := adapter.Status()
		codexStatus.Available = snapshot.CodexUsage.Available
		codexStatus.UnavailableReason = snapshot.CodexUsage.UnavailableReason
		snapshot.Adapters[0] = codexStatus
		m.catalog.SetRefreshInterval(time.Duration(config.ScanIntervalSeconds) * time.Second)
		retryIDs := make([]string, 0, len(retries)+len(taskPlan.SelectedThreadIDs)+1)
		for _, retry := range retries {
			retryIDs = append(retryIDs, retry.ThreadID)
		}
		// Selected older threads must remain discoverable when they fall out
		// of the recent-ten catalog page.
		retryIDs = append(retryIDs, taskPlan.SelectedThreadIDs...)
		if state.QuotaSchedulerOwnerID != "" {
			retryIDs = append(retryIDs, state.QuotaSchedulerOwnerID)
		}
		var catalog TaskCatalogSnapshot
		if asyncUsage {
			catalog = m.catalog.SnapshotAsync(now, adapter, config.AutoScanEnabled, retryIDs)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			catalog = m.catalog.Snapshot(ctx, now, adapter, config.AutoScanEnabled, retryIDs)
			cancel()
		}
		snapshot.TaskCatalogAvailable = catalog.Available
		snapshot.TaskCatalogRefreshing = catalog.Refreshing
		snapshot.TaskCatalogUnavailableReason = catalog.UnavailableReason
		snapshot.Tasks = applyRecoveryResults(ensureSchedulerOwnerTask(applyTaskPlan(mergeManagedTasks(catalog, retries), taskPlan), state), state)
	} else {
		snapshot.CodexUsage = unavailableUsage(adapterCodex, "monitoring_paused")
	}
	if statusFound {
		if running {
			snapshot.Version = status.Version
		}
		snapshot.WatchedRoots = status.WatchedRoots
		snapshot.LastError = status.LastError
		snapshot.ControllerState = status.ControllerState
		capability := capabilityForControllerState(status.ControllerState, status.SharedAppServerEnabled, config.CLIResumeFallbackEnabled)
		snapshot.DesktopTransport = status.DesktopTransport
		if snapshot.DesktopTransport == "" {
			snapshot.DesktopTransport = capability.Transport
		}
		snapshot.RecoveryMode = status.RecoveryMode
		if snapshot.RecoveryMode == "" {
			snapshot.RecoveryMode = capability.RecoveryMode
		}
		snapshot.AutomaticRecoverySupported = status.AutomaticRecoverySupported
		snapshot.RecoveryCapabilityReason = status.RecoveryCapabilityReason
		if snapshot.RecoveryCapabilityReason == "" {
			snapshot.RecoveryCapabilityReason = capability.Reason
		}
		snapshot.MemoryUsageMB = status.MemoryUsageMB
		if status.MemoryLimitMB > 0 {
			snapshot.MemoryLimitMB = status.MemoryLimitMB
		}
		snapshot.MemoryGuardTriggered = status.MemoryGuardTriggered
		snapshot.SharedAppServerMemoryUsageMB = status.SharedAppServerMemoryUsageMB
		if status.SharedAppServerMemoryLimitMB > 0 {
			snapshot.SharedAppServerMemoryLimitMB = status.SharedAppServerMemoryLimitMB
		}
		snapshot.SharedAppServerMemoryGuardTriggered = status.SharedAppServerMemoryGuardTriggered
		if status.RetrySafetyWarning != "" {
			snapshot.RetrySafetyWarning = status.RetrySafetyWarning
		}
		if !status.LastScanAt.IsZero() {
			snapshot.LastScanAt = status.LastScanAt.Format(time.RFC3339Nano)
		}
	}
	snapshot.Diagnostics = m.buildDiagnostics(now, config, control, status, statusFound, running, retries, snapshot.CodexUsage)
	return snapshot, nil
}

func schedulerOwnerCanConfirm(state RuntimeState, config Config, running bool) bool {
	if !running || config.QuotaResumeMode != quotaModeSequentialCompletion || state.QuotaSchedulerOwnerID == "" {
		return false
	}
	thread, found := state.Threads[state.QuotaSchedulerOwnerID]
	return !found || (!thread.ObservedTurnRunning && thread.Awaiting == nil)
}

// A held sequential task must remain actionable after its last retry succeeds
// and it falls out of Codex's recent-ten catalog. Neither a successful turn
// nor a missing catalog row proves that the whole task has finished.
func ensureSchedulerOwnerTask(tasks []ManagedTask, state RuntimeState) []ManagedTask {
	id := state.QuotaSchedulerOwnerID
	if id == "" {
		return tasks
	}
	thread, found := state.Threads[id]
	status := "awaiting_confirmation"
	if found {
		switch {
		case thread.ObservedTurnRunning:
			status = "running"
		case thread.Awaiting != nil:
			status = "starting"
			if thread.Awaiting.RetryTurnID != "" {
				status = "running"
			}
		case thread.Pending != nil:
			status = "pending"
		case thread.Stopped != nil:
			status = "stopped"
		}
	}
	for index := range tasks {
		if strings.EqualFold(tasks[index].ThreadID, id) {
			if status == "awaiting_confirmation" &&
				(tasks[index].Status == "running" || tasks[index].Status == "active" || tasks[index].Status == "starting") {
				return tasks
			}
			tasks[index].Status = status
			if found && thread.Stopped != nil {
				tasks[index].StopReason = thread.Stopped.Reason
				tasks[index].Class = thread.Stopped.Class
				tasks[index].IsRecovery = true
			}
			if status == "awaiting_confirmation" {
				tasks[index].InfoUnavailable = false
			}
			return tasks
		}
	}
	task := ManagedTask{
		ThreadID: id,
		Label:    "Codex Task " + shortThreadID(id),
		TaskType: "Codex",
		Status:   status,
	}
	if found {
		task.LastActivityAt = formatManagedTime(managedRetryLastActivity(thread))
		if thread.Stopped != nil {
			task.StopReason = thread.Stopped.Reason
			task.Class = thread.Stopped.Class
			task.IsRecovery = true
		}
	}
	return append(tasks, task)
}

// Call only while holding m.mu. Management actions that validate a task keep
// using loadState directly so their safety decision never relies on this cache.
func (m *managementService) loadStateCached(now time.Time) (RuntimeState, error) {
	info, err := os.Stat(m.statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return RuntimeState{}, err
	}
	exists := err == nil
	if !m.stateCacheAt.IsZero() && now.Sub(m.stateCacheAt) >= 0 &&
		now.Sub(m.stateCacheAt) < managementStateCacheMaxAge &&
		exists == m.stateCacheExists &&
		(!exists || (info.Size() == m.stateCacheSize && info.ModTime().Equal(m.stateCacheModTime))) {
		return m.stateCache, nil
	}
	state, err := loadState(m.statePath)
	if err != nil {
		m.stateCacheAt = time.Time{}
		return RuntimeState{}, err
	}
	m.stateCache = state
	m.stateCacheAt = now
	m.stateCacheExists = exists
	if exists {
		m.stateCacheSize = info.Size()
		m.stateCacheModTime = info.ModTime()
	} else {
		m.stateCacheSize = 0
		m.stateCacheModTime = time.Time{}
	}
	return state, nil
}

func (m *managementService) buildDiagnostics(now time.Time, config Config, control ControlState, status StatusSnapshot, statusFound, running bool, retries []ManagedRetry, usage UsageSnapshot) DiagnosticSnapshot {
	result := DiagnosticSnapshot{
		Version:             snapshotVersion(status, statusFound, running),
		Build:               snapshotBuild(status, statusFound, running),
		ServiceState:        "stopped",
		AppServerConnection: "unavailable",
		MonitoringState:     "running",
		AutoResumeState:     "running",
		UsageSource:         "unavailable",
		RecoveryTaskCount:   len(retries),
		WatchedRoots:        status.WatchedRoots,
		RecentError:         privacySafeDiagnosticError(status.LastError),
		LogPath:             filepath.Join(m.dataDir, "logs", "daemon.log"),
		LogMaxBytes:         maxLogBytes,
		LogFileCount:        maxLogFiles,
		NextResumeAt:        earliestResumeAt(retries),
	}
	if running {
		result.ServiceState = "running"
	}
	if control.MonitoringPaused {
		result.MonitoringState = "paused"
	}
	if control.Paused {
		result.AutoResumeState = "paused"
	}
	if statusFound {
		result.StartedAt = formatDiagnosticTime(status.StartedAt)
		result.LastTaskScanAt = formatDiagnosticTime(status.LastScanAt)
		if status.LogPath != "" {
			result.LogPath = status.LogPath
		}
		if running && !status.StartedAt.IsZero() && !status.StartedAt.After(now) {
			result.UptimeSeconds = int64(now.Sub(status.StartedAt) / time.Second)
		}
		switch status.ControllerState {
		case "ready", "official_ipc_ready":
			result.AppServerConnection = "connected"
		case "codex_restart_required", "codex_app_not_ready":
			result.AppServerConnection = "waiting_for_codex"
		case "codex_not_running":
			result.AppServerConnection = "codex_not_running"
		case "shared_app_server_disabled":
			result.AppServerConnection = "disabled"
		case "":
			result.AppServerConnection = "unknown"
		default:
			result.AppServerConnection = status.ControllerState
		}
	}
	if usage.Source != "" {
		result.UsageSource = usage.Source
	}
	monitorStatus := m.usage.Status()
	result.LastUsageAttemptAt = formatDiagnosticTime(monitorStatus.LastAttempt)
	result.LastUsageRefreshAt = usage.ObservedAt
	if result.LastUsageRefreshAt == "" {
		result.LastUsageRefreshAt = usage.FetchedAt
	}
	if !control.MonitoringPaused {
		result.NextUsageRefreshAt = formatDiagnosticTime(monitorStatus.NextRefresh)
	}
	settings, err := loadDiagnosticSettings(filepath.Join(m.dataDir, diagnosticSettingsFileName))
	if err == nil && settings.DebugUntil.After(now) {
		result.DebugEnabled = true
		result.DebugUntil = formatDiagnosticTime(settings.DebugUntil)
	}
	return result
}

func snapshotVersion(status StatusSnapshot, statusFound, running bool) string {
	if statusFound && running && status.Version != "" {
		return status.Version
	}
	return appVersion
}

func snapshotBuild(status StatusSnapshot, statusFound, running bool) int {
	if statusFound && running && status.Build > 0 {
		return status.Build
	}
	return appBuild
}

func (m *managementService) setSharedAppServerEnabled(enabled bool, now time.Time) (ManagementSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	config, err := loadOrCreateConfig(m.configPath)
	if err != nil {
		return ManagementSnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if enabled {
		config, err = updateConfigFile(m.configPath, func(current *Config) error {
			current.SharedAppServerRequested = true
			return nil
		})
		if err != nil {
			return ManagementSnapshot{}, fmt.Errorf("save shared app-server preference: %w", err)
		}
		preparedConfig, err := enableSharedAppServer(ctx, m.dataDir, config)
		if err != nil {
			return ManagementSnapshot{}, err
		}
		config = preparedConfig
		persistedConfig, err := updateConfigFile(m.configPath, func(current *Config) error {
			if !current.SharedAppServerRequested {
				return fmt.Errorf("shared app-server enable cancelled by user")
			}
			current.SharedAppServerEnabled = true
			current.SharedAppServerPort = config.SharedAppServerPort
			return nil
		})
		if err != nil {
			_ = disableSharedAppServer(context.Background(), m.dataDir, config)
			return ManagementSnapshot{}, fmt.Errorf("save shared app-server setting: %w", err)
		}
		config = persistedConfig
		for _, marker := range []string{"shared-availability.json", "shared-fail-open.json"} {
			if err := os.Remove(filepath.Join(m.dataDir, marker)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return ManagementSnapshot{}, fmt.Errorf("clear shared recovery marker: %w", err)
			}
		}
	} else {
		// Persist the fail-open setting before tearing down the endpoint. If the
		// cleanup is interrupted, the next watchdog tick still cannot take over
		// Codex's backend.
		persistedConfig, err := updateConfigFile(m.configPath, func(current *Config) error {
			current.SharedAppServerRequested = false
			current.SharedAppServerEnabled = false
			return nil
		})
		if err != nil {
			return ManagementSnapshot{}, fmt.Errorf("save shared app-server setting: %w", err)
		}
		config = persistedConfig
		if err := disableSharedAppServer(ctx, m.dataDir, config); err != nil {
			return ManagementSnapshot{}, err
		}
	}
	snapshot, err := m.snapshotLocked(now.UTC(), false)
	if err == nil {
		if enabled {
			snapshot.Notice = "Codex自動續跑助手已啟用；完全退出 Codex 後，透過安全啟動 Codex 入口接入"
		} else {
			snapshot.Notice = "Codex自動續跑助手已關閉，Codex 將使用官方後台"
		}
	}
	return snapshot, err
}

type RetrySettings struct {
	RetryPrompt              string  `json:"retry_prompt"`
	MaxConsecutiveRetries    int     `json:"max_consecutive_retries"`
	MaxRecoveryAttempts      int     `json:"max_recovery_attempts"`
	InitialDelaySeconds      int     `json:"initial_delay_seconds"`
	MaxDelaySeconds          int     `json:"max_delay_seconds"`
	DelayIncrementSeconds    int     `json:"delay_increment_seconds"`
	DelayStrategy            string  `json:"delay_strategy"`
	ShowNotifications        bool    `json:"show_notifications"`
	MemoryLimitMB            int     `json:"memory_limit_mb,omitempty"`
	ResumeIntervalSeconds    *int    `json:"resume_interval_seconds,omitempty"`
	QuotaResumeMode          *string `json:"quota_resume_mode,omitempty"`
	MaxParallelSessions      *int    `json:"max_parallel_sessions,omitempty"`
	UsageRefreshMinutes      *int    `json:"usage_refresh_minutes,omitempty"`
	AutoScanEnabled          *bool   `json:"auto_scan_enabled,omitempty"`
	ScanIntervalSeconds      *int    `json:"scan_interval_seconds,omitempty"`
	CLIResumeFallbackEnabled *bool   `json:"cli_resume_fallback_enabled,omitempty"`
}

func (m *managementService) setRetrySettings(settings RetrySettings, now time.Time) (ManagementSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := updateConfigFile(m.configPath, func(config *Config) error {
		applyRetrySettings(config, settings)
		return nil
	}); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("save retry settings: %w", err)
	}
	snapshot, err := m.snapshotLocked(now.UTC(), false)
	if err == nil {
		snapshot.Notice = "自動重試設定已儲存"
	}
	return snapshot, err
}

func (m *managementService) setLocalSettings(settings RetrySettings, paused bool, monitoringPaused *bool, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	config, err := loadOrCreateConfig(m.configPath)
	if err != nil {
		return err
	}
	originalConfig := config
	control, err := loadOrCreateControlState(m.controlPath)
	if err != nil {
		return err
	}
	desiredMonitoringPaused := control.MonitoringPaused
	if monitoringPaused != nil {
		desiredMonitoringPaused = *monitoringPaused
	}
	if _, err := updateConfigFile(m.configPath, func(config *Config) error {
		applyRetrySettings(config, settings)
		return nil
	}); err != nil {
		return fmt.Errorf("save local retry settings: %w", err)
	}
	if _, err := saveControlSettings(m.controlPath, paused, desiredMonitoringPaused, now); err != nil {
		if _, rollbackErr := updateConfigFile(m.configPath, func(config *Config) error {
			*config = originalConfig
			return nil
		}); rollbackErr != nil {
			return fmt.Errorf("save local pause state: %v; restore retry settings: %w", err, rollbackErr)
		}
		return fmt.Errorf("save local pause state: %w", err)
	}
	return nil
}

func applyRetrySettings(config *Config, settings RetrySettings) {
	config.RetryPrompt = settings.RetryPrompt
	config.MaxConsecutiveRetries = settings.MaxConsecutiveRetries
	config.MaxRecoveryAttempts = settings.MaxRecoveryAttempts
	config.InitialDelaySeconds = settings.InitialDelaySeconds
	config.MaxDelaySeconds = settings.MaxDelaySeconds
	if settings.DelayIncrementSeconds != 0 || settings.DelayStrategy == delayStrategyLinear {
		config.DelayIncrementSeconds = settings.DelayIncrementSeconds
	}
	config.DelayStrategy = settings.DelayStrategy
	config.ShowNotifications = settings.ShowNotifications
	if settings.MemoryLimitMB > 0 {
		config.MemoryLimitMB = settings.MemoryLimitMB
	}
	if settings.ResumeIntervalSeconds != nil {
		config.ResumeIntervalSeconds = *settings.ResumeIntervalSeconds
	}
	if settings.QuotaResumeMode != nil {
		config.QuotaResumeMode = *settings.QuotaResumeMode
	}
	if settings.MaxParallelSessions != nil {
		config.MaxParallelSessions = *settings.MaxParallelSessions
	}
	if settings.UsageRefreshMinutes != nil {
		config.UsageRefreshMinutes = *settings.UsageRefreshMinutes
	}
	if settings.AutoScanEnabled != nil {
		config.AutoScanEnabled = *settings.AutoScanEnabled
	}
	if settings.ScanIntervalSeconds != nil {
		config.ScanIntervalSeconds = *settings.ScanIntervalSeconds
	}
	if settings.CLIResumeFallbackEnabled != nil {
		config.CLIResumeFallbackEnabled = *settings.CLIResumeFallbackEnabled
	}
}

func (m *managementService) setRetryPrompt(prompt string, now time.Time) (ManagementSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := updateConfigFile(m.configPath, func(config *Config) error {
		config.RetryPrompt = prompt
		return nil
	}); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("save retry prompt: %w", err)
	}
	snapshot, err := m.snapshotLocked(now.UTC(), false)
	if err == nil {
		snapshot.Notice = "一般對話的重試文字已儲存"
	}
	return snapshot, err
}

func (m *managementService) setPaused(paused bool, now time.Time) (ManagementSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := saveControlState(m.controlPath, paused, now); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("save pause state: %w", err)
	}
	snapshot, err := m.snapshotLocked(now.UTC(), false)
	if err == nil {
		if paused {
			snapshot.Notice = "自動重試已暫停"
		} else {
			snapshot.Notice = "自動重試已恢復"
		}
	}
	return snapshot, err
}

func (m *managementService) setMonitoringPaused(paused bool, now time.Time) (ManagementSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := saveMonitoringPaused(m.controlPath, paused, now); err != nil {
		return ManagementSnapshot{}, fmt.Errorf("save monitoring pause state: %w", err)
	}
	snapshot, err := m.snapshotLocked(now.UTC(), false)
	if err == nil {
		if paused {
			snapshot.Notice = "監控與額度更新已暫停"
		} else {
			snapshot.Notice = "監控與額度更新已恢復"
		}
	}
	return snapshot, err
}

func (m *managementService) retryNow(threadID string, now time.Time) (ManagementSnapshot, error) {
	return m.queueThreadCommand(commandRetryNow, threadID, now)
}

func (m *managementService) cancelRetry(threadID string, now time.Time) (ManagementSnapshot, error) {
	return m.queueThreadCommand(commandCancelRetry, threadID, now)
}

func (m *managementService) restartRetry(threadID string, now time.Time) (ManagementSnapshot, error) {
	return m.queueThreadCommand(commandRestartRetry, threadID, now)
}

func (m *managementService) resumeTask(threadID, rolloutPath string, class FailureClass, now time.Time) (ManagementSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	config, err := loadOrCreateConfig(m.configPath)
	if err != nil {
		return ManagementSnapshot{}, err
	}
	threadID = strings.ToLower(strings.TrimSpace(threadID))
	rolloutPath = normalizeCatalogRolloutPath(rolloutPath)
	if _, err := validateCatalogResumePath(config, threadID, rolloutPath); err != nil {
		return ManagementSnapshot{}, err
	}
	if class == classNone {
		class = classTransient
	}
	if _, err := queueControlCommandWithTask(m.commandDir, commandResumeTask, threadID, rolloutPath, class, now); err != nil {
		return ManagementSnapshot{}, err
	}
	snapshot, err := m.snapshotLocked(now.UTC(), false)
	if err == nil {
		snapshot.Notice = "已要求續跑這個 Codex Task"
	}
	return snapshot, err
}

func (m *managementService) queueThreadCommand(action ControlCommandAction, threadID string, now time.Time) (ManagementSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	threadID = strings.ToLower(strings.TrimSpace(threadID))
	state, err := loadState(m.statePath)
	if err != nil {
		return ManagementSnapshot{}, err
	}
	thread, found := state.Threads[threadID]
	if !found || (action == commandRestartRetry && thread.Stopped == nil) ||
		(action != commandRestartRetry && thread.Pending == nil) {
		return ManagementSnapshot{}, errors.New("此 Task 目前沒有可執行的重試操作")
	}
	if _, err := queueControlCommand(m.commandDir, action, threadID, now); err != nil {
		return ManagementSnapshot{}, err
	}
	snapshot, err := m.snapshotLocked(now.UTC(), false)
	if err == nil {
		if action == commandRetryNow {
			snapshot.Notice = "已要求立即重試"
		} else if action == commandRestartRetry {
			snapshot.Notice = "已重新開始計數並要求重試"
		} else {
			snapshot.Notice = "已要求取消這次重試"
		}
	}
	return snapshot, err
}

func loadStatusSnapshot(path string) (StatusSnapshot, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return StatusSnapshot{}, false, nil
	}
	if err != nil {
		return StatusSnapshot{}, false, err
	}
	var status StatusSnapshot
	if err := json.Unmarshal(data, &status); err != nil {
		return StatusSnapshot{}, false, fmt.Errorf("parse status: %w", err)
	}
	return status, true, nil
}

func (m *managementService) managedRetries(state RuntimeState, now time.Time) []ManagedRetry {
	retries := make([]ManagedRetry, 0)
	for threadID, thread := range state.Threads {
		label, projectPath := m.managedRetryIdentity(threadID, thread)
		lastActivityAt := managedRetryLastActivity(thread)
		if thread.Pending != nil {
			remaining := int64(0)
			if duration := thread.Pending.DueAt.Sub(now); duration > 0 {
				remaining = int64((duration + time.Second - 1) / time.Second)
			}
			retries = append(retries, ManagedRetry{
				ThreadID:              threadID,
				Label:                 label,
				ProjectPath:           projectPath,
				TaskType:              "Codex",
				LastActivityAt:        formatManagedTime(lastActivityAt),
				State:                 "pending",
				Class:                 thread.Pending.Class,
				DueAt:                 thread.Pending.DueAt.Format(time.RFC3339Nano),
				SecondsRemaining:      remaining,
				RecoveryAttempt:       thread.Pending.Attempt,
				MaxRecoveryAttempts:   thread.Pending.MaxAttempts,
				ConsecutiveRetry:      thread.Pending.ConsecutiveRetry,
				MaxConsecutiveRetries: thread.Pending.MaxConsecutive,
				CanRetryNow:           true,
				CanCancel:             true,
				WaitReason:            thread.Pending.WaitReason,
			})
		}
		if thread.Awaiting != nil {
			stateName := "starting"
			if thread.Awaiting.RetryTurnID != "" {
				stateName = "running"
			}
			retries = append(retries, ManagedRetry{
				ThreadID:              threadID,
				Label:                 label,
				ProjectPath:           projectPath,
				TaskType:              "Codex",
				LastActivityAt:        formatManagedTime(lastActivityAt),
				State:                 stateName,
				Class:                 thread.Awaiting.Class,
				RecoveryAttempt:       thread.Awaiting.Attempt,
				MaxRecoveryAttempts:   thread.Awaiting.MaxAttempts,
				ConsecutiveRetry:      thread.Awaiting.ConsecutiveRetry,
				MaxConsecutiveRetries: thread.Awaiting.MaxConsecutive,
				Action:                thread.Awaiting.Action,
			})
		}
		if thread.Stopped != nil && stoppedRetryIsVisible(thread.Stopped, now) {
			retries = append(retries, ManagedRetry{
				ThreadID:              threadID,
				Label:                 label,
				ProjectPath:           projectPath,
				TaskType:              "Codex",
				LastActivityAt:        formatManagedTime(lastActivityAt),
				State:                 "stopped",
				Class:                 thread.Stopped.Class,
				RecoveryAttempt:       thread.Stopped.Attempts,
				MaxRecoveryAttempts:   thread.Stopped.MaxAttempts,
				ConsecutiveRetry:      thread.Stopped.ConsecutiveRetries,
				MaxConsecutiveRetries: thread.Stopped.MaxConsecutive,
				CanRestart:            true,
				StopReason:            thread.Stopped.Reason,
			})
		}
	}
	sort.SliceStable(retries, func(i, j int) bool {
		if retries[i].State != retries[j].State {
			if retries[i].State == "pending" {
				return false
			}
			if retries[j].State == "pending" {
				return true
			}
		}
		if retries[i].DueAt != retries[j].DueAt {
			return retries[i].DueAt < retries[j].DueAt
		}
		return retries[i].ThreadID < retries[j].ThreadID
	})
	return retries
}

func (m *managementService) managedRetryIdentity(threadID string, thread ThreadState) (string, string) {
	label := "Codex Task " + shortThreadID(threadID)
	rolloutPath := threadRolloutPath(thread)
	if rolloutPath == "" {
		return label, ""
	}
	if cached, ok := m.taskIdentity[rolloutPath]; ok {
		return cached.label, cached.projectPath
	}
	settings, err := loadLatestResumeSettings(rolloutPath)
	if err != nil {
		return label, ""
	}
	projectPath := filepath.Clean(strings.TrimSpace(settings.CWD))
	if projectPath == "." || projectPath == "" {
		return label, ""
	}
	projectName := strings.TrimSpace(filepath.Base(projectPath))
	if projectName == "" || projectName == "." || projectName == string(filepath.Separator) {
		m.taskIdentity[rolloutPath] = managedTaskIdentity{label: label, projectPath: projectPath}
		return label, projectPath
	}
	label = projectName + " · " + shortThreadID(threadID)
	m.taskIdentity[rolloutPath] = managedTaskIdentity{label: label, projectPath: projectPath}
	return label, projectPath
}

func managedRetryLastActivity(thread ThreadState) time.Time {
	latest := time.Time{}
	for _, candidate := range []time.Time{
		thread.LastFailureAt,
		thread.LastAutoRetryAt,
		thread.GoalUpdatedAt,
		thread.LastStartedAt,
		thread.LastExternalTurnAt,
		thread.LastAbortedAt,
	} {
		if candidate.After(latest) {
			latest = candidate
		}
	}
	if thread.Pending != nil && thread.Pending.FailedAt.After(latest) {
		latest = thread.Pending.FailedAt
	}
	if thread.Awaiting != nil {
		for _, candidate := range []time.Time{thread.Awaiting.FailedAt, thread.Awaiting.DispatchStartedAt, thread.Awaiting.StartedAt} {
			if candidate.After(latest) {
				latest = candidate
			}
		}
	}
	if thread.Stopped != nil {
		for _, candidate := range []time.Time{thread.Stopped.FailedAt, thread.Stopped.StoppedAt} {
			if candidate.After(latest) {
				latest = candidate
			}
		}
	}
	return latest
}

func formatManagedTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func stoppedRetryIsVisible(stopped *StoppedRetry, now time.Time) bool {
	if stopped == nil || stopped.StoppedAt.IsZero() {
		return stopped != nil
	}
	if stopped.Historical {
		return false
	}
	if !stopped.FailedAt.IsZero() && !stopped.FailedAt.After(now) && now.Sub(stopped.FailedAt) > stoppedRetryDisplayWindow {
		return false
	}
	if stopped.StoppedAt.After(now) {
		return true
	}
	return now.Sub(stopped.StoppedAt) <= stoppedRetryDisplayWindow
}
