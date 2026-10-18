package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultTaskCatalogLimit = 10

type ManagedTask struct {
	ThreadID                string       `json:"thread_id"`
	Label                   string       `json:"label"`
	ProjectPath             string       `json:"project_path,omitempty"`
	TaskType                string       `json:"task_type"`
	LastActivityAt          string       `json:"last_activity_at,omitempty"`
	Status                  string       `json:"status"`
	Class                   FailureClass `json:"class,omitempty"`
	CanResume               bool         `json:"can_resume"`
	CanCancel               bool         `json:"can_cancel"`
	IsRecovery              bool         `json:"is_recovery,omitempty"`
	SelectedForMonitoring   bool         `json:"selected_for_monitoring"`
	QueueOrder              int          `json:"queue_order,omitempty"`
	InfoUnavailable         bool         `json:"info_unavailable,omitempty"`
	ResumeAction            string       `json:"resume_action,omitempty"`
	RolloutPath             string       `json:"rollout_path,omitempty"`
	DueAt                   string       `json:"due_at,omitempty"`
	WaitReason              string       `json:"wait_reason,omitempty"`
	StopReason              string       `json:"stop_reason,omitempty"`
	LastRecoverySucceededAt string       `json:"last_recovery_succeeded_at,omitempty"`
}

type TaskCatalogSnapshot struct {
	Available         bool          `json:"available"`
	Refreshing        bool          `json:"refreshing,omitempty"`
	FetchedAt         string        `json:"fetched_at,omitempty"`
	UnavailableReason string        `json:"unavailable_reason,omitempty"`
	Tasks             []ManagedTask `json:"tasks"`
}

type taskCatalogAdapter interface {
	ListTasks(context.Context, int, []string) (TaskCatalogSnapshot, error)
}

type appServerThreadStatus struct {
	Type string `json:"type"`
}

type appServerThreadSummary struct {
	ID        string                `json:"id"`
	Name      *string               `json:"name"`
	CWD       string                `json:"cwd"`
	Path      *string               `json:"path"`
	UpdatedAt int64                 `json:"updatedAt"`
	Status    appServerThreadStatus `json:"status"`
}

type appServerThreadListResult struct {
	Data []appServerThreadSummary `json:"data"`
}

type appServerCatalogThreadReadResult struct {
	Thread *appServerThreadSummary `json:"thread"`
}

type appServerTurnError struct {
	Message        string          `json:"message"`
	CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
}

type appServerTurnSummary struct {
	Status string              `json:"status"`
	Error  *appServerTurnError `json:"error"`
}

type appServerTurnListResult struct {
	Data []appServerTurnSummary `json:"data"`
}

func (a *CodexAdapter) ListTasks(ctx context.Context, limit int, retryIDs []string) (TaskCatalogSnapshot, error) {
	if a == nil {
		return TaskCatalogSnapshot{UnavailableReason: "app_server_unavailable"}, errAdapterUnavailable
	}
	if limit <= 0 || limit > 100 {
		limit = defaultTaskCatalogLimit
	}
	var client appServerCaller
	var err error
	if a.endpoint != "" && a.dial != nil {
		client, err = a.dial(ctx, a.endpoint)
	}
	if client == nil && a.sidecarDial != nil {
		client, err = a.sidecarDial(ctx)
	}
	if client == nil && err == nil {
		err = errAdapterUnavailable
	}
	if err != nil {
		return TaskCatalogSnapshot{UnavailableReason: "app_server_unavailable"}, err
	}
	defer client.Close()

	var result appServerThreadListResult
	params := map[string]any{
		"limit": limit, "sortKey": "updated_at", "sortDirection": "desc",
		"archived": false, "useStateDbOnly": true,
	}
	if err := client.Call(ctx, "thread/list", params, &result); err != nil {
		return TaskCatalogSnapshot{UnavailableReason: "thread_list_failed"}, err
	}
	now := time.Now().UTC()
	if a.now != nil {
		now = a.now().UTC()
	}
	tasks := make([]ManagedTask, 0, len(result.Data))
	listed := make(map[string]struct{}, len(result.Data))
	for _, thread := range result.Data {
		if thread.ID == "" {
			continue
		}
		listed[strings.ToLower(thread.ID)] = struct{}{}
		task := managedTaskFromThread(thread)
		var turns appServerTurnListResult
		turnParams := map[string]any{
			"threadId": thread.ID, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
		}
		if err := client.Call(ctx, "thread/turns/list", turnParams, &turns); err == nil && len(turns.Data) > 0 {
			applyLatestTurnState(&task, turns.Data[0])
		}
		tasks = append(tasks, task)
	}
	// Keep the visible recent-task limit unchanged, but resolve the official
	// title of older retry rows by stable thread ID. A failed read leaves the
	// existing rollout-derived fallback in place.
	for _, threadID := range retryIDs {
		threadID = strings.ToLower(strings.TrimSpace(threadID))
		if threadID == "" {
			continue
		}
		if _, found := listed[threadID]; found {
			continue
		}
		listed[threadID] = struct{}{}
		var read appServerCatalogThreadReadResult
		if err := client.Call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false}, &read); err != nil ||
			read.Thread == nil || !strings.EqualFold(read.Thread.ID, threadID) ||
			(strings.TrimSpace(valueOrEmpty(read.Thread.Name)) == "" && strings.TrimSpace(read.Thread.CWD) == "") {
			continue
		}
		task := managedTaskFromThread(*read.Thread)
		var turns appServerTurnListResult
		turnParams := map[string]any{
			"threadId": threadID, "limit": 1, "sortDirection": "desc", "itemsView": "notLoaded",
		}
		if err := client.Call(ctx, "thread/turns/list", turnParams, &turns); err == nil && len(turns.Data) > 0 {
			applyLatestTurnState(&task, turns.Data[0])
		}
		tasks = append(tasks, task)
	}
	return TaskCatalogSnapshot{Available: true, FetchedAt: now.Format(time.RFC3339Nano), Tasks: tasks}, nil
}

func managedTaskFromThread(thread appServerThreadSummary) ManagedTask {
	label := strings.TrimSpace(valueOrEmpty(thread.Name))
	projectPath := strings.TrimSpace(thread.CWD)
	if label == "" {
		projectName := strings.TrimSpace(filepath.Base(projectPath))
		if projectName == "" || projectName == "." || projectName == string(filepath.Separator) {
			label = "Codex Task " + shortThreadID(thread.ID)
		} else {
			label = projectName + " · " + shortThreadID(thread.ID)
		}
	}
	status := strings.TrimSpace(thread.Status.Type)
	if status == "" {
		status = "unknown"
	}
	lastActivity := ""
	if thread.UpdatedAt > 0 {
		lastActivity = time.Unix(thread.UpdatedAt, 0).UTC().Format(time.RFC3339Nano)
	}
	rolloutPath := normalizeCatalogRolloutPath(valueOrEmpty(thread.Path))
	task := ManagedTask{
		ThreadID: strings.ToLower(thread.ID), Label: label, ProjectPath: projectPath,
		TaskType: "Codex", LastActivityAt: lastActivity, Status: status,
		RolloutPath: rolloutPath, ResumeAction: "resume_task",
	}
	if status == "systemError" {
		task.Status = "stopped"
		task.Class = classUnknown
		task.CanResume = rolloutPath != ""
	}
	return task
}

func applyLatestTurnState(task *ManagedTask, turn appServerTurnSummary) {
	if task == nil {
		return
	}
	switch turn.Status {
	case "inProgress":
		task.Status = "running"
		task.CanResume = false
	case "interrupted":
		task.Status = "paused"
		task.Class = classTransient
		task.CanResume = task.RolloutPath != ""
	case "failed":
		decision := classifyCatalogTurnError(turn.Error)
		if decision.Class == classRateLimit {
			task.Status = "rate_limited"
		} else {
			task.Status = "stopped"
		}
		task.Class = decision.Class
		task.CanResume = decision.Retry && task.RolloutPath != ""
	case "completed":
		if task.Status != "active" {
			task.Status = "completed"
		}
		task.CanResume = false
	}
}

func classifyCatalogTurnError(value *appServerTurnError) RetryDecision {
	if value == nil {
		return RetryDecision{Retry: true, Class: classUnknown, Reason: "missing structured turn error"}
	}
	code := catalogErrorCode(value.CodexErrorInfo)
	switch code {
	case "usageLimitExceeded", "rateLimitExceeded", "sessionBudgetExceeded":
		return RetryDecision{Retry: true, Class: classRateLimit, Reason: "structured quota limit"}
	case "serverOverloaded", "internalServerError", "responseTooManyFailedAttempts":
		return RetryDecision{Retry: true, Class: classServer, Reason: "structured server error"}
	case "httpConnectionFailed", "responseStreamConnectionFailed", "responseStreamDisconnected":
		return RetryDecision{Retry: true, Class: classTransient, Reason: "structured connection error"}
	case "unauthorized":
		return RetryDecision{Retry: true, Class: classAuthLimited, Reason: "structured authentication error"}
	case "contextWindowExceeded", "badRequest", "cyberPolicy", "misalignmentPolicyViolation":
		return RetryDecision{Class: classNone, Reason: "structured permanent error"}
	}
	return classifyFailure(value.Message, defaultConfig())
}

func catalogErrorCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		for key := range object {
			return key
		}
	}
	return ""
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type taskCatalogMonitor struct {
	mu              sync.Mutex
	refreshInterval time.Duration
	adapterKey      string
	cacheKey        string
	lastAttempt     time.Time
	value           TaskCatalogSnapshot
	generation      uint64
	refreshing      bool
}

func newTaskCatalogMonitor(interval time.Duration) *taskCatalogMonitor {
	if interval <= 0 {
		interval = time.Minute
	}
	return &taskCatalogMonitor{refreshInterval: interval}
}

func (m *taskCatalogMonitor) SetRefreshInterval(interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	m.mu.Lock()
	m.refreshInterval = interval
	m.mu.Unlock()
}

func (m *taskCatalogMonitor) Invalidate() {
	m.mu.Lock()
	m.lastAttempt = time.Time{}
	m.generation++
	m.refreshing = false
	m.mu.Unlock()
}

func (m *taskCatalogMonitor) Snapshot(ctx context.Context, now time.Time, adapter TaskAdapter, enabled bool, retryIDs []string) TaskCatalogSnapshot {
	lister, ok := adapter.(taskCatalogAdapter)
	if !ok {
		return TaskCatalogSnapshot{UnavailableReason: "thread_list_unavailable"}
	}
	m.mu.Lock()
	key := catalogCacheKey(adapter, retryIDs)
	cacheFresh := key == m.cacheKey && !m.lastAttempt.IsZero() && now.Sub(m.lastAttempt) >= 0 && now.Sub(m.lastAttempt) < m.refreshInterval
	if cacheFresh || (!enabled && !m.lastAttempt.IsZero()) {
		value := m.value
		value.Refreshing = m.refreshing
		m.mu.Unlock()
		return value
	}
	m.cacheKey = key
	m.adapterKey = adapter.CacheKey()
	m.lastAttempt = now
	m.generation++
	generation := m.generation
	m.refreshing = true
	m.mu.Unlock()

	value, err := lister.ListTasks(ctx, defaultTaskCatalogLimit, retryIDs)
	if err != nil {
		value = TaskCatalogSnapshot{UnavailableReason: "thread_list_failed"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation == generation && m.cacheKey == key {
		value.Refreshing = false
		m.value = value
		m.refreshing = false
	}
	return value
}

func (m *taskCatalogMonitor) SnapshotAsync(now time.Time, adapter TaskAdapter, enabled bool, retryIDs []string) TaskCatalogSnapshot {
	lister, ok := adapter.(taskCatalogAdapter)
	if !ok {
		return TaskCatalogSnapshot{UnavailableReason: "thread_list_unavailable"}
	}
	m.mu.Lock()
	adapterKey := adapter.CacheKey()
	key := catalogCacheKey(adapter, retryIDs)
	cacheFresh := key == m.cacheKey && !m.lastAttempt.IsZero() && now.Sub(m.lastAttempt) >= 0 && now.Sub(m.lastAttempt) < m.refreshInterval
	if cacheFresh || (!enabled && !m.lastAttempt.IsZero()) {
		value := m.value
		value.Refreshing = m.refreshing
		m.mu.Unlock()
		return value
	}
	previous := m.value
	// Selection changes alter retryIDs and therefore the cache key. Keep the
	// last catalog visible while the same endpoint fetches those extra IDs;
	// Refreshing and the original FetchedAt still identify it as old data.
	// A different endpoint or a failed prior fetch must not inherit it.
	keepPrevious := adapterKey == m.adapterKey && previous.Available
	m.cacheKey = key
	m.adapterKey = adapterKey
	m.lastAttempt = now
	m.generation++
	generation := m.generation
	m.refreshing = true
	pending := TaskCatalogSnapshot{Refreshing: true, UnavailableReason: "task_catalog_refresh_pending"}
	if keepPrevious {
		pending = previous
		pending.Refreshing = true
	}
	m.mu.Unlock()

	retryIDs = append([]string(nil), retryIDs...)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		value, err := lister.ListTasks(ctx, defaultTaskCatalogLimit, retryIDs)
		cancel()
		if err != nil {
			value = TaskCatalogSnapshot{UnavailableReason: "thread_list_failed"}
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.generation != generation || m.cacheKey != key {
			return
		}
		value.Refreshing = false
		m.value = value
		m.refreshing = false
	}()
	return pending
}

func catalogCacheKey(adapter TaskAdapter, retryIDs []string) string {
	ids := append([]string(nil), retryIDs...)
	for i := range ids {
		ids[i] = strings.ToLower(strings.TrimSpace(ids[i]))
	}
	sort.Strings(ids)
	return adapter.CacheKey() + ":" + strings.Join(ids, ",")
}

func mergeManagedTasks(catalog TaskCatalogSnapshot, retries []ManagedRetry) []ManagedTask {
	tasks := append([]ManagedTask(nil), catalog.Tasks...)
	index := make(map[string]int, len(tasks))
	for i := range tasks {
		index[strings.ToLower(tasks[i].ThreadID)] = i
	}
	for _, retry := range retries {
		task := ManagedTask{
			ThreadID: retry.ThreadID, Label: retry.Label, ProjectPath: retry.ProjectPath,
			TaskType: retry.TaskType, LastActivityAt: retry.LastActivityAt, Status: retry.State,
			Class: retry.Class, CanResume: retry.CanRetryNow || retry.CanRestart,
			CanCancel: retry.CanCancel, IsRecovery: true, DueAt: retry.DueAt, WaitReason: retry.WaitReason,
			StopReason: retry.StopReason,
		}
		if retry.CanRetryNow {
			task.ResumeAction = string(commandRetryNow)
		} else if retry.CanRestart {
			task.ResumeAction = string(commandRestartRetry)
		}
		if i, found := index[strings.ToLower(retry.ThreadID)]; found {
			catalogTask := tasks[i]
			if strings.TrimSpace(catalogTask.Label) != "" {
				task.Label = catalogTask.Label
			}
			if strings.TrimSpace(catalogTask.ProjectPath) != "" {
				task.ProjectPath = catalogTask.ProjectPath
			}
			task.RolloutPath = catalogTask.RolloutPath
			tasks[i] = task
		} else {
			tasks = append(tasks, task)
			index[strings.ToLower(retry.ThreadID)] = len(tasks) - 1
		}
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		left, leftErr := time.Parse(time.RFC3339Nano, tasks[i].LastActivityAt)
		right, rightErr := time.Parse(time.RFC3339Nano, tasks[j].LastActivityAt)
		if leftErr == nil && rightErr == nil && !left.Equal(right) {
			return left.After(right)
		}
		if leftErr != rightErr {
			return leftErr == nil
		}
		return tasks[i].ThreadID < tasks[j].ThreadID
	})
	return tasks
}

// applyTaskPlan marks catalog rows without changing their recovery eligibility.
// The plan also remains separately visible in the management snapshot when a
// selected older thread cannot currently be resolved by app-server.
func applyTaskPlan(tasks []ManagedTask, plan TaskPlan) []ManagedTask {
	present := make(map[string]struct{}, len(tasks))
	for index := range tasks {
		present[strings.ToLower(tasks[index].ThreadID)] = struct{}{}
		tasks[index].SelectedForMonitoring = plan.IsSelected(tasks[index].ThreadID)
		if order := plan.Order(tasks[index].ThreadID); order < maxTaskPlanThreads {
			tasks[index].QueueOrder = order + 1
		}
	}
	for _, id := range plan.SelectedThreadIDs {
		if _, found := present[id]; found {
			continue
		}
		tasks = append(tasks, ManagedTask{
			ThreadID: id, Label: "Codex Task " + shortThreadID(id),
			TaskType: "Codex", Status: "unknown", CanResume: false,
			SelectedForMonitoring: true, QueueOrder: plan.Order(id) + 1,
			InfoUnavailable: true,
		})
	}
	return tasks
}

// A successful matching retry turn is a verified recovery result, not proof
// that the user's larger project is finished. New turns clear the marker.
func applyRecoveryResults(tasks []ManagedTask, state RuntimeState) []ManagedTask {
	for i := range tasks {
		thread, found := state.Threads[tasks[i].ThreadID]
		if !found || thread.LastRecoverySucceededAt.IsZero() ||
			thread.LastRecoverySucceededAt.Before(thread.LastStartedAt) {
			continue
		}
		switch tasks[i].Status {
		case "pending", "starting", "running", "active", "stopped", "rate_limited":
			continue
		}
		tasks[i].LastRecoverySucceededAt = thread.LastRecoverySucceededAt.Format(time.RFC3339Nano)
	}
	return tasks
}

func normalizeCatalogRolloutPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.Separator == '\\' {
		if len(path) >= 7 && strings.EqualFold(path[:4], "\\\\?\\") && path[5] == ':' && (path[6] == '\\' || path[6] == '/') {
			path = path[4:]
		} else if len(path) > 8 && strings.EqualFold(path[:8], "\\\\?\\UNC\\") {
			path = "\\\\" + path[8:]
		}
	}
	return filepath.Clean(path)
}

func validateCatalogResumePath(config Config, threadID, rolloutPath string) (sessionRoot, error) {
	rolloutPath = normalizeCatalogRolloutPath(rolloutPath)
	if !filepath.IsAbs(rolloutPath) || !strings.EqualFold(threadIDFromPath(rolloutPath), threadID) {
		return sessionRoot{}, fmt.Errorf("invalid task rollout path")
	}
	for _, root := range discoverSessionRoots(config) {
		if filePathBelongsToRoot(rolloutPath, root) {
			return root, nil
		}
	}
	return sessionRoot{}, fmt.Errorf("task rollout path is outside configured session roots")
}
