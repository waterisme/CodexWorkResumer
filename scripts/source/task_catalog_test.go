package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type fakeTaskCatalogCaller struct {
	methods []string
}

func (f *fakeTaskCatalogCaller) Call(_ context.Context, method string, _ any, destination any) error {
	f.methods = append(f.methods, method)
	switch method {
	case "thread/list":
		result := destination.(*appServerThreadListResult)
		name := "Quota test"
		path := `C:\Users\ExampleUser\.codex\sessions\2026\10\01\rollout-2026-10-01T01-02-03-019f9d5d-9c82-75b1-b7c0-20a658af0423.jsonl`
		result.Data = []appServerThreadSummary{{
			ID: "019f9d5d-9c82-75b1-b7c0-20a658af0423", Name: &name, CWD: `P:\project`, Path: &path,
			UpdatedAt: 1_800_000_000, Status: appServerThreadStatus{Type: "idle"},
		}}
		return nil
	case "thread/turns/list":
		result := destination.(*appServerTurnListResult)
		result.Data = []appServerTurnSummary{{
			Status: "failed",
			Error:  &appServerTurnError{Message: "usage limit", CodexErrorInfo: json.RawMessage(`{"usageLimitExceeded":{}}`)},
		}}
		return nil
	default:
		return errors.New("unexpected method")
	}
}

func (*fakeTaskCatalogCaller) Close() error { return nil }

func TestCodexAdapterListsRecentStructuredQuotaTask(t *testing.T) {
	caller := &fakeTaskCatalogCaller{}
	adapter := NewCodexAdapter("")
	adapter.sidecarDial = func(context.Context) (appServerCaller, error) { return caller, nil }
	adapter.now = func() time.Time { return time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) }
	snapshot, err := adapter.ListTasks(context.Background(), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Available || len(snapshot.Tasks) != 1 {
		t.Fatalf("unexpected catalog: %+v", snapshot)
	}
	task := snapshot.Tasks[0]
	if task.Label != "Quota test" || task.ProjectPath != `P:\project` {
		t.Fatalf("app-server task identity was not retained: %+v", task)
	}
	if task.Status != "rate_limited" || task.Class != classRateLimit || !task.CanResume || task.ResumeAction != "resume_task" {
		t.Fatalf("structured quota task was not resumable: %+v", task)
	}
	if len(caller.methods) != 2 || caller.methods[0] != "thread/list" || caller.methods[1] != "thread/turns/list" {
		t.Fatalf("unexpected methods: %v", caller.methods)
	}
}

type olderRetryTitleCaller struct {
	methods        []string
	olderID        string
	failOlderTurns bool
}

func (f *olderRetryTitleCaller) Call(_ context.Context, method string, params any, destination any) error {
	f.methods = append(f.methods, method)
	switch method {
	case "thread/list":
		name := "Recent task"
		destination.(*appServerThreadListResult).Data = []appServerThreadSummary{{ID: "019f9d5d-9c82-75b1-b7c0-20a658af0423", Name: &name}}
	case "thread/turns/list":
		args := params.(map[string]any)
		if args["threadId"] == f.olderID {
			if f.failOlderTurns {
				return errors.New("older turn unavailable")
			}
			destination.(*appServerTurnListResult).Data = []appServerTurnSummary{{
				Status: "failed",
				Error:  &appServerTurnError{Message: "usage limit", CodexErrorInfo: json.RawMessage(`{"usageLimitExceeded":{}}`)},
			}}
		}
		return nil
	case "thread/read":
		args := params.(map[string]any)
		if args["threadId"] != f.olderID || args["includeTurns"] != false {
			return errors.New("unexpected older thread lookup")
		}
		name := "Official older task title"
		path := `C:\Users\ExampleUser\.codex\sessions\2026\10\01\rollout-2026-10-01T01-02-03-` + f.olderID + `.jsonl`
		destination.(*appServerCatalogThreadReadResult).Thread = &appServerThreadSummary{
			ID: f.olderID, Name: &name, CWD: `P:\official-project`, Path: &path,
		}
	default:
		return errors.New("unexpected method")
	}
	return nil
}

func (*olderRetryTitleCaller) Close() error { return nil }

func TestCodexAdapterReadsOfficialTitleForOlderRetry(t *testing.T) {
	olderID := "019f9d5d-9c82-75b1-b7c0-20a658af0424"
	caller := &olderRetryTitleCaller{olderID: olderID}
	adapter := NewCodexAdapter("")
	adapter.sidecarDial = func(context.Context) (appServerCaller, error) { return caller, nil }
	snapshot, err := adapter.ListTasks(context.Background(), 1, []string{olderID, olderID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 2 || snapshot.Tasks[1].Label != "Official older task title" ||
		snapshot.Tasks[1].ProjectPath != `P:\official-project` ||
		snapshot.Tasks[1].Status != "rate_limited" || snapshot.Tasks[1].Class != classRateLimit || !snapshot.Tasks[1].CanResume {
		t.Fatalf("older retry did not receive official identity: %+v", snapshot.Tasks)
	}
	if len(caller.methods) != 4 || caller.methods[2] != "thread/read" || caller.methods[3] != "thread/turns/list" {
		t.Fatalf("older retry should be read only once: %v", caller.methods)
	}
}

func TestCodexAdapterOlderTurnLookupFailureDoesNotClaimQuota(t *testing.T) {
	olderID := "019f9d5d-9c82-75b1-b7c0-20a658af0424"
	caller := &olderRetryTitleCaller{olderID: olderID, failOlderTurns: true}
	adapter := NewCodexAdapter("")
	adapter.sidecarDial = func(context.Context) (appServerCaller, error) { return caller, nil }
	snapshot, err := adapter.ListTasks(context.Background(), 1, []string{olderID})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Tasks) != 2 || snapshot.Tasks[1].Status != "unknown" || snapshot.Tasks[1].CanResume || snapshot.Tasks[1].Class == classRateLimit {
		t.Fatalf("missing older turn was mistaken for quota: %+v", snapshot.Tasks)
	}
}

func TestMergeManagedTasksKeepsAllRecoveryRowsInActivityOrder(t *testing.T) {
	catalog := TaskCatalogSnapshot{Available: true, Tasks: []ManagedTask{{ThreadID: "a", Status: "completed", LastActivityAt: "2026-10-01T12:00:00Z"}}}
	retries := []ManagedRetry{{ThreadID: "b", State: "pending", Class: classTransient, CanRetryNow: true, CanCancel: true, LastActivityAt: "2026-10-01T11:00:00Z"}}
	tasks := mergeManagedTasks(catalog, retries)
	if len(tasks) != 2 || tasks[0].ThreadID != "a" || tasks[1].ThreadID != "b" || tasks[1].ResumeAction != string(commandRetryNow) {
		t.Fatalf("recovery task was not retained in last-activity order: %+v", tasks)
	}
}

func TestMergeManagedTasksPreservesCatalogIdentityDuringRetry(t *testing.T) {
	for _, state := range []string{"pending", "starting", "running", "stopped"} {
		t.Run(state, func(t *testing.T) {
			name := "Official task title"
			catalogTask := managedTaskFromThread(appServerThreadSummary{
				ID: "019f9d5d-9c82-75b1-b7c0-20a658af0423", Name: &name,
				CWD: `P:\official-project`, Status: appServerThreadStatus{Type: "idle"},
			})
			catalogTask.RolloutPath = `C:\sessions\official-rollout.jsonl`
			retry := ManagedRetry{
				ThreadID: "019F9D5D-9C82-75B1-B7C0-20A658AF0423",
				Label:    "stale-project · 019f9d5d", ProjectPath: `P:\stale-project`,
				State: state, Class: classTransient, CanRetryNow: state == "pending",
				CanCancel: state == "pending", CanRestart: state == "stopped",
			}
			tasks := mergeManagedTasks(TaskCatalogSnapshot{Available: true, Tasks: []ManagedTask{catalogTask}}, []ManagedRetry{retry})
			if len(tasks) != 1 {
				t.Fatalf("expected one merged task, got %+v", tasks)
			}
			task := tasks[0]
			if task.Label != name || task.ProjectPath != `P:\official-project` || task.RolloutPath != catalogTask.RolloutPath {
				t.Fatalf("retry replaced app-server identity: %+v", task)
			}
			if task.Status != state || task.Class != classTransient || task.CanCancel != retry.CanCancel {
				t.Fatalf("retry state was not applied: %+v", task)
			}
		})
	}
}

func TestMergeManagedTasksUsesRetryIdentityWhenCatalogFieldsAbsent(t *testing.T) {
	for _, test := range []struct {
		name        string
		label       string
		projectPath string
		wantLabel   string
		wantPath    string
	}{
		{name: "both absent", label: " ", projectPath: " ", wantLabel: "fallback title", wantPath: `P:\fallback`},
		{name: "title absent", label: " ", projectPath: `P:\official`, wantLabel: "fallback title", wantPath: `P:\official`},
		{name: "path absent", label: "Official title", projectPath: " ", wantLabel: "Official title", wantPath: `P:\fallback`},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := TaskCatalogSnapshot{Available: true, Tasks: []ManagedTask{{ThreadID: "task", Label: test.label, ProjectPath: test.projectPath}}}
			retry := ManagedRetry{ThreadID: "task", Label: "fallback title", ProjectPath: `P:\fallback`, State: "pending"}
			tasks := mergeManagedTasks(catalog, []ManagedRetry{retry})
			if len(tasks) != 1 || tasks[0].Label != test.wantLabel || tasks[0].ProjectPath != test.wantPath {
				t.Fatalf("missing catalog fields did not use retry fallback: %+v", tasks)
			}
		})
	}
}

func TestCatalogErrorCodeReadsTaggedObject(t *testing.T) {
	if got := catalogErrorCode(json.RawMessage(`{"usageLimitExceeded":{"resetsAt":1800000000}}`)); got != "usageLimitExceeded" {
		t.Fatalf("unexpected code: %q", got)
	}
}

func TestRecoveryResultOnlyMarksCurrentVerifiedCompletedTurn(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	state := newRuntimeState()
	state.Threads["done"] = ThreadState{LastStartedAt: now.Add(-time.Minute), LastRecoverySucceededAt: now}
	state.Threads["new-turn"] = ThreadState{LastStartedAt: now.Add(time.Minute), LastRecoverySucceededAt: now}
	state.Threads["pending"] = ThreadState{LastStartedAt: now.Add(-time.Minute), LastRecoverySucceededAt: now}
	tasks := applyRecoveryResults([]ManagedTask{
		{ThreadID: "done", Status: "completed"},
		{ThreadID: "new-turn", Status: "completed"},
		{ThreadID: "pending", Status: "pending"},
	}, state)
	if tasks[0].LastRecoverySucceededAt == "" || tasks[1].LastRecoverySucceededAt != "" ||
		tasks[2].LastRecoverySucceededAt != "" {
		t.Fatalf("recovery result leaked to a new or still-pending turn: %+v", tasks)
	}
}

type catalogRefreshTestAdapter struct {
	*fakeAdapter
	key       string
	requested chan []string
	release   <-chan struct{}
	result    TaskCatalogSnapshot
	err       error
}

func (a *catalogRefreshTestAdapter) CacheKey() string { return a.key }

func (a *catalogRefreshTestAdapter) ListTasks(ctx context.Context, _ int, ids []string) (TaskCatalogSnapshot, error) {
	a.requested <- append([]string(nil), ids...)
	select {
	case <-a.release:
		return a.result, a.err
	case <-ctx.Done():
		return TaskCatalogSnapshot{}, ctx.Err()
	}
}

func waitForCatalogRefresh(t *testing.T, monitor *taskCatalogMonitor) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		monitor.mu.Lock()
		refreshing := monitor.refreshing
		monitor.mu.Unlock()
		if !refreshing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("catalog refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTaskCatalogKeepsExistingIdentityWhileSelectionRefreshes(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	oldID := "019f9d5d-9c82-75b1-b7c0-20a658af0423"
	newID := "019f9d5d-9c82-75b1-b7c0-20a658af0424"
	old := TaskCatalogSnapshot{Available: true, FetchedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), Tasks: []ManagedTask{{ThreadID: oldID, Label: "Official task title"}}}
	monitor := newTaskCatalogMonitor(time.Minute)
	monitor.adapterKey = "codex:stdio"
	monitor.cacheKey = "codex:stdio:" + oldID
	monitor.lastAttempt = now
	monitor.value = old

	release := make(chan struct{})
	adapter := &catalogRefreshTestAdapter{
		fakeAdapter: &fakeAdapter{}, key: "codex:stdio", requested: make(chan []string, 1), release: release,
		result: TaskCatalogSnapshot{Available: true, FetchedAt: now.Add(time.Second).Format(time.RFC3339Nano), Tasks: []ManagedTask{
			{ThreadID: oldID, Label: "Official task title"}, {ThreadID: newID, Label: "Older official title"},
		}},
	}
	pending := monitor.SnapshotAsync(now.Add(time.Second), adapter, true, []string{oldID, newID})
	if !pending.Available || !pending.Refreshing || pending.FetchedAt != old.FetchedAt ||
		len(pending.Tasks) != 1 || pending.Tasks[0].Label != "Official task title" {
		t.Fatalf("selection change hid the previous official identity: %+v", pending)
	}
	if ids := <-adapter.requested; len(ids) != 2 || ids[1] != newID {
		t.Fatalf("newly selected task was not requested: %v", ids)
	}
	close(release)
	waitForCatalogRefresh(t, monitor)
	updated := monitor.SnapshotAsync(now.Add(2*time.Second), adapter, true, []string{oldID, newID})
	if !updated.Available || updated.Refreshing || len(updated.Tasks) != 2 || updated.Tasks[1].Label != "Older official title" {
		t.Fatalf("fresh selection catalog did not replace the old snapshot: %+v", updated)
	}

	failRelease := make(chan struct{})
	failing := &catalogRefreshTestAdapter{
		fakeAdapter: &fakeAdapter{}, key: "codex:stdio", requested: make(chan []string, 1), release: failRelease,
		err: errors.New("official catalog unavailable"),
	}
	stillPending := monitor.SnapshotAsync(now.Add(3*time.Second), failing, true, []string{oldID, newID, "another-id"})
	if !stillPending.Available || !stillPending.Refreshing || len(stillPending.Tasks) != 2 {
		t.Fatalf("successful catalog disappeared before failed fetch resolved: %+v", stillPending)
	}
	<-failing.requested
	close(failRelease)
	waitForCatalogRefresh(t, monitor)
	failed := monitor.SnapshotAsync(now.Add(4*time.Second), failing, true, []string{oldID, newID, "another-id"})
	if failed.Available || failed.Refreshing || failed.UnavailableReason != "thread_list_failed" || len(failed.Tasks) != 0 {
		t.Fatalf("a failed refresh was presented as a fresh catalog: %+v", failed)
	}
}

func TestTaskCatalogDoesNotCarryIdentityAcrossEndpointChange(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	monitor := newTaskCatalogMonitor(time.Minute)
	monitor.adapterKey = "codex:old-endpoint"
	monitor.cacheKey = "codex:old-endpoint:old-id"
	monitor.lastAttempt = now
	monitor.value = TaskCatalogSnapshot{Available: true, Tasks: []ManagedTask{{ThreadID: "old-id", Label: "Old account task"}}}
	release := make(chan struct{})
	adapter := &catalogRefreshTestAdapter{
		fakeAdapter: &fakeAdapter{}, key: "codex:new-endpoint", requested: make(chan []string, 1), release: release,
		result: TaskCatalogSnapshot{Available: true},
	}
	pending := monitor.SnapshotAsync(now.Add(time.Second), adapter, true, []string{"new-id"})
	if pending.Available || !pending.Refreshing || len(pending.Tasks) != 0 || pending.UnavailableReason != "task_catalog_refresh_pending" {
		t.Fatalf("previous endpoint's identity leaked into the new endpoint: %+v", pending)
	}
	<-adapter.requested
	close(release)
	waitForCatalogRefresh(t, monitor)
}
