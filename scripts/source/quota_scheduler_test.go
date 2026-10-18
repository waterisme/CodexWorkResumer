package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testQuotaA = "019f9d33-41f6-70a3-ab98-df2196cb7aa7"
	testQuotaB = "019f9d47-7e70-7120-9b69-776d8d25ae0e"
	testQuotaC = "019f9d58-e254-7be2-91f8-a7df30958a12"
)

func quotaPending(id string, at time.Time) ThreadState {
	return ThreadState{Pending: &PendingRetry{
		EventKey: id, FailedTurnID: "failed", Class: classRateLimit,
		DueAt: at, Attempt: 1,
	}}
}

func TestQuotaSchedulerSequentialHoldsAcrossTurnCompletionAndRestart(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	planPath := filepath.Join(d.dataDir, "task-plan.json")
	if _, err := saveTaskPlan(planPath, 0,
		[]string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
		t.Fatal(err)
	}
	d.state.Threads[testQuotaA] = quotaPending(testQuotaA, now)
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	first := d.dispatchDueLocked(now)
	if len(first) != 1 || first[0].ThreadID != testQuotaA || d.state.QuotaSchedulerOwnerID != testQuotaA {
		t.Fatalf("first quota task was not held: jobs=%+v owner=%s", first, d.state.QuotaSchedulerOwnerID)
	}
	delete(d.active, testQuotaA) // controller accepted; Codex lifecycle now owns the turn.
	d.handleEventLocked(scannedEvent{ThreadID: testQuotaA, Event: RelevantEvent{
		Kind: "task_started", TurnID: "retry-a", Timestamp: now.Add(time.Second),
	}}, now.Add(time.Second))
	d.handleEventLocked(scannedEvent{ThreadID: testQuotaA, Event: RelevantEvent{
		Kind: "task_complete", TurnID: "retry-a", Timestamp: now.Add(2 * time.Second),
		FinalKnown: true, FinalPresent: true,
	}}, now.Add(2*time.Second))
	if d.state.Threads[testQuotaA].ObservedTurnRunning || d.state.Threads[testQuotaA].Awaiting != nil {
		t.Fatal("successful turn was not recorded as stopped")
	}
	if got := d.dispatchDueLocked(now.Add(30 * time.Second)); len(got) != 0 {
		t.Fatalf("turn completion was mistaken for whole-task completion: %+v", got)
	}
	if err := writeJSONAtomic(d.statePath, d.state); err != nil {
		t.Fatal(err)
	}
	restarted, err := newDaemon(cfg, d.dataDir, d.logger, successfulRunner())
	if err != nil {
		t.Fatal(err)
	}
	if restarted.state.QuotaSchedulerOwnerID != testQuotaA || len(restarted.dispatchDueLocked(now.Add(time.Minute))) != 0 {
		t.Fatal("restart released the sequential owner or dispatched B")
	}
	// A can hit a fresh quota limit after resuming; it remains ahead of B.
	restarted.state.Threads[testQuotaA] = quotaPending(testQuotaA, now.Add(2*time.Minute))
	next := restarted.dispatchDueLocked(now.Add(2 * time.Minute))
	if len(next) != 1 || next[0].ThreadID != testQuotaA {
		t.Fatalf("second quota cycle jumped past A: %+v", next)
	}
	delete(restarted.active, testQuotaA)
	thread := restarted.state.Threads[testQuotaA]
	thread.Awaiting = nil
	restarted.state.Threads[testQuotaA] = thread
	if _, err := queueControlCommand(restarted.commandDir, commandConfirmTaskComplete, testQuotaA, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	restarted.refreshControlsLocked(now.Add(3 * time.Minute))
	plan, err := loadTaskPlan(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.state.QuotaSchedulerOwnerID != "" || plan.IsSelected(testQuotaA) {
		t.Fatal("explicit completion did not clear owner and selection")
	}
	if got := restarted.dispatchDueLocked(now.Add(3 * time.Minute)); len(got) != 1 || got[0].ThreadID != testQuotaB {
		t.Fatalf("B did not become eligible after explicit completion: %+v", got)
	}
}

func TestQuotaSchedulerSequentialAutomaticallyAdvancesAfterQuietSuccessfulTurn(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(d.dataDir, "task-plan.json")
	if _, err := saveTaskPlan(path, 0, []string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
		t.Fatal(err)
	}
	d.state.Threads[testQuotaA] = quotaPending(testQuotaA, now)
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	if got := d.dispatchDueLocked(now); len(got) != 1 || got[0].ThreadID != testQuotaA {
		t.Fatalf("A was not dispatched first: %+v", got)
	}
	delete(d.active, testQuotaA)
	d.handleEventLocked(scannedEvent{ThreadID: testQuotaA, Event: RelevantEvent{
		Kind: "task_started", TurnID: "retry-a", Timestamp: now.Add(time.Second),
	}}, now.Add(time.Second))
	observedAt := now.Add(2 * time.Second)
	d.lastHealthyScan = observedAt
	d.handleEventLocked(scannedEvent{ThreadID: testQuotaA, Event: RelevantEvent{
		Kind: "task_complete", TurnID: "retry-a", Timestamp: observedAt,
		FinalKnown: true, FinalPresent: true,
	}}, observedAt)
	if got := d.dispatchDueLocked(now.Add(40 * time.Second)); len(got) != 0 {
		t.Fatalf("one successful turn released A without a later healthy scan: %+v", got)
	}
	d.lastHealthyScan = now.Add(70 * time.Second)
	if got := d.dispatchDueLocked(now.Add(70 * time.Second)); len(got) != 1 || got[0].ThreadID != testQuotaB {
		t.Fatalf("B did not start after verified quiet completion: %+v", got)
	}
	if d.state.QuotaSchedulerOwnerID != testQuotaB {
		t.Fatalf("B did not take sequential ownership: %s", d.state.QuotaSchedulerOwnerID)
	}
	plan, err := loadTaskPlan(path)
	if err != nil || !plan.IsSelected(testQuotaA) || !plan.IsSelected(testQuotaB) {
		t.Fatalf("quiet handoff changed the persistent watchlist: plan=%+v err=%v", plan, err)
	}
}

func TestQuotaSchedulerSequentialCompletionFailsClosedForUncertainOrPausedWork(t *testing.T) {
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		change func(*ThreadState)
	}{
		{name: "missing final evidence", change: func(thread *ThreadState) { thread.LastPositiveCompletionAt = time.Time{} }},
		{name: "new turn", change: func(thread *ThreadState) {
			thread.LastStartedAt = now.Add(20 * time.Second)
			thread.ObservedTurnRunning = true
		}},
		{name: "quota pending", change: func(thread *ThreadState) { pending := quotaPending(testQuotaA, now); thread.Pending = pending.Pending }},
		{name: "goal active", change: func(thread *ThreadState) { thread.GoalStatus = "active" }},
		{name: "goal paused", change: func(thread *ThreadState) { thread.GoalStatus = "paused"; thread.GoalHeld = true }},
		{name: "unknown goal", change: func(thread *ThreadState) { thread.GoalStatus = "mystery" }},
		{name: "awaiting retry", change: func(thread *ThreadState) { thread.Awaiting = &AwaitingRetry{} }},
		{name: "stale scan", change: func(thread *ThreadState) {}},
		{name: "auto scan disabled", change: func(thread *ThreadState) {}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := isolatedConfig(t.TempDir())
			cfg.QuotaResumeMode = quotaModeSequentialCompletion
			if test.name == "auto scan disabled" {
				cfg.AutoScanEnabled = false
			}
			d := newTestDaemon(t, cfg, successfulRunner())
			if _, err := saveTaskPlan(filepath.Join(d.dataDir, "task-plan.json"), 0,
				[]string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
				t.Fatal(err)
			}
			d.state.QuotaSchedulerOwnerID = testQuotaA
			thread := ThreadState{
				LastStartedTurnID: "completed-a", LastStartedAt: now,
				LastPositiveCompletionAt:         now.Add(time.Second),
				LastPositiveCompletionObservedAt: now.Add(2 * time.Second),
			}
			test.change(&thread)
			d.state.Threads[testQuotaA] = thread
			d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
			if test.name != "stale scan" {
				d.lastHealthyScan = now.Add(time.Minute)
			}
			got := d.dispatchDueLocked(now.Add(time.Minute))
			for _, job := range got {
				if job.ThreadID == testQuotaB {
					t.Fatalf("B ran while A remained uncertain: jobs=%+v", got)
				}
			}
			if d.state.QuotaSchedulerOwnerID != testQuotaA {
				t.Fatalf("uncertain owner was released: jobs=%+v owner=%s", got, d.state.QuotaSchedulerOwnerID)
			}
		})
	}
}

func TestQuotaSchedulerSequentialStoppedQuotaYieldsNextTask(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(d.dataDir, "task-plan.json")
	if _, err := saveTaskPlan(path, 0, []string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
		t.Fatal(err)
	}
	d.state.QuotaSchedulerOwnerID = testQuotaA
	d.state.Threads[testQuotaA] = ThreadState{Stopped: &StoppedRetry{
		Class: classRateLimit, Reason: "max_recovery_attempts",
	}}
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	jobs := d.dispatchDueLocked(now)
	if len(jobs) != 1 || jobs[0].ThreadID != testQuotaB || d.state.QuotaSchedulerOwnerID != testQuotaB {
		t.Fatalf("stopped quota retry blocked next task: jobs=%+v owner=%s", jobs, d.state.QuotaSchedulerOwnerID)
	}
	plan, err := loadTaskPlan(path)
	if err != nil || !plan.IsSelected(testQuotaA) {
		t.Fatalf("stopped task lost watchlist selection: plan=%+v err=%v", plan, err)
	}
}

func TestQuotaSchedulerSequentialTerminalNoRetryNeedsLaterHealthyScan(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	if _, err := saveTaskPlan(filepath.Join(d.dataDir, "task-plan.json"), 0,
		[]string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
		t.Fatal(err)
	}
	d.state.QuotaSchedulerOwnerID = testQuotaA
	d.state.Threads[testQuotaA] = ThreadState{
		LastStartedAt: now, LastAutoRetryAt: now,
		LastTerminalNoRetryAt: now.Add(time.Second),
	}
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	if jobs := d.dispatchDueLocked(now.Add(time.Minute)); len(jobs) != 0 {
		t.Fatalf("terminal result handed off before a healthy scan: %+v", jobs)
	}
	d.lastHealthyScan = now.Add(time.Minute)
	if jobs := d.dispatchDueLocked(now.Add(time.Minute)); len(jobs) != 1 || jobs[0].ThreadID != testQuotaB {
		t.Fatalf("terminal result kept the next task blocked: %+v", jobs)
	}
}

func TestQuotaSchedulerSequentialOfficialGoalCompletionFromController(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, &fakeResumeRunner{result: DispatchResult{
		Outcome: outcomeNotApplicable, Reason: "goal_completed",
	}})
	now := time.Now().UTC().Add(-time.Second)
	path := filepath.Join(d.dataDir, "task-plan.json")
	if _, err := saveTaskPlan(path, 0, []string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
		t.Fatal(err)
	}
	d.state.Threads[testQuotaA] = quotaPending(testQuotaA, now)
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	jobs := d.dispatchDueLocked(now)
	if len(jobs) != 1 || jobs[0].ThreadID != testQuotaA {
		t.Fatalf("first task not dispatched: %+v", jobs)
	}
	d.wg.Add(1)
	go d.runJob(context.Background(), jobs[0])
	d.waitForJobs()
	thread := daemonThreadSnapshot(d, testQuotaA)
	if thread.GoalStatus != "completed" || !thread.GoalObservedAt.After(thread.LastAutoRetryAt) {
		t.Fatalf("controller's official completion was not recorded: %+v", thread)
	}
	if jobs = d.dispatchDueLocked(now.Add(time.Minute)); len(jobs) != 1 || jobs[0].ThreadID != testQuotaB {
		t.Fatalf("officially completed goal blocked next task: %+v", jobs)
	}
	plan, err := loadTaskPlan(path)
	if err != nil || plan.IsSelected(testQuotaA) {
		t.Fatalf("official goal completion did not clear completed watch: plan=%+v err=%v", plan, err)
	}
}

func TestQuotaSchedulerSequentialSkippedRetryHandsOffAfterScan(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, &fakeResumeRunner{result: DispatchResult{
		Outcome: outcomeNotApplicable, Reason: "turn_missing",
	}})
	now := time.Now().UTC().Add(-time.Second)
	path := filepath.Join(d.dataDir, "task-plan.json")
	if _, err := saveTaskPlan(path, 0, []string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
		t.Fatal(err)
	}
	d.state.Threads[testQuotaA] = quotaPending(testQuotaA, now)
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	jobs := d.dispatchDueLocked(now)
	if len(jobs) != 1 || jobs[0].ThreadID != testQuotaA {
		t.Fatalf("first task not dispatched: %+v", jobs)
	}
	d.wg.Add(1)
	go d.runJob(context.Background(), jobs[0])
	d.waitForJobs()
	thread := daemonThreadSnapshot(d, testQuotaA)
	if thread.LastTerminalNoRetryAt.IsZero() || thread.Pending != nil || thread.Awaiting != nil {
		t.Fatalf("terminal skipped retry was not recorded: %+v", thread)
	}
	if jobs = d.dispatchDueLocked(now.Add(time.Minute)); len(jobs) != 0 {
		t.Fatalf("skipped retry handed off without a later healthy scan: %+v", jobs)
	}
	d.lastHealthyScan = now.Add(time.Minute)
	if jobs = d.dispatchDueLocked(now.Add(time.Minute)); len(jobs) != 1 || jobs[0].ThreadID != testQuotaB {
		t.Fatalf("skipped retry blocked next task: %+v", jobs)
	}
	plan, err := loadTaskPlan(path)
	if err != nil || !plan.IsSelected(testQuotaA) {
		t.Fatalf("skipped retry lost watchlist selection: plan=%+v err=%v", plan, err)
	}
}

func TestQuotaSchedulerSequentialAbortedTurnHandsOffAfterScan(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	if _, err := saveTaskPlan(filepath.Join(d.dataDir, "task-plan.json"), 0,
		[]string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
		t.Fatal(err)
	}
	d.state.QuotaSchedulerOwnerID = testQuotaA
	d.state.Threads[testQuotaA] = ThreadState{
		LastStartedTurnID: "turn-a", LastStartedAt: now,
		LastAutoRetryAt: now, ObservedTurnRunning: true,
	}
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	d.handleEventLocked(scannedEvent{ThreadID: testQuotaA, Event: RelevantEvent{
		Kind: "turn_aborted", TurnID: "turn-a", Timestamp: now.Add(time.Second),
	}}, now.Add(time.Second))
	if d.state.Threads[testQuotaA].LastTerminalNoRetryAt.IsZero() {
		t.Fatal("aborted turn did not record a terminal no-retry result")
	}
	d.lastHealthyScan = now.Add(time.Minute)
	if jobs := d.dispatchDueLocked(now.Add(time.Minute)); len(jobs) != 1 || jobs[0].ThreadID != testQuotaB {
		t.Fatalf("aborted turn blocked next task: %+v", jobs)
	}
}

func TestQuotaSchedulerSequentialAdoptedRunningTaskAdvancesAutomatically(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(d.dataDir, "task-plan.json")
	if _, err := saveTaskPlan(path, 0, []string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
		t.Fatal(err)
	}
	d.state.Threads[testQuotaA] = ThreadState{LastStartedTurnID: "manual-a", LastStartedAt: now, ObservedTurnRunning: true}
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	if got := d.dispatchDueLocked(now); len(got) != 0 || d.state.QuotaSchedulerOwnerID != testQuotaA {
		t.Fatalf("running A was not adopted: jobs=%+v owner=%s", got, d.state.QuotaSchedulerOwnerID)
	}
	observedAt := now.Add(10 * time.Second)
	d.handleEventLocked(scannedEvent{ThreadID: testQuotaA, Event: RelevantEvent{
		Kind: "task_complete", TurnID: "manual-a", Timestamp: observedAt, FinalKnown: true, FinalPresent: true,
	}}, observedAt)
	d.lastHealthyScan = now.Add(80 * time.Second)
	if got := d.dispatchDueLocked(now.Add(80 * time.Second)); len(got) != 1 || got[0].ThreadID != testQuotaB {
		t.Fatalf("preselected A did not release B after quiet completion: %+v", got)
	}
}

func TestQuotaSchedulerSequentialNativeFollowOnTurnRetainsOwner(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	if _, err := saveTaskPlan(filepath.Join(d.dataDir, "task-plan.json"), 0,
		[]string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
		t.Fatal(err)
	}
	d.state.QuotaSchedulerOwnerID = testQuotaA
	d.state.Threads[testQuotaA] = ThreadState{LastStartedTurnID: "a-1", LastStartedAt: now, ObservedTurnRunning: true}
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	d.handleEventLocked(scannedEvent{ThreadID: testQuotaA, Event: RelevantEvent{
		Kind: "task_complete", TurnID: "a-1", Timestamp: now.Add(time.Second), FinalKnown: true, FinalPresent: true,
	}}, now.Add(2*time.Second))
	d.handleEventLocked(scannedEvent{ThreadID: testQuotaA, Event: RelevantEvent{
		Kind: "task_started", TurnID: "a-2", Timestamp: now.Add(3 * time.Second),
	}}, now.Add(4*time.Second))
	d.lastHealthyScan = now.Add(80 * time.Second)
	if got := d.dispatchDueLocked(now.Add(80 * time.Second)); len(got) != 0 || d.state.QuotaSchedulerOwnerID != testQuotaA {
		t.Fatalf("follow-on turn was overtaken: jobs=%+v owner=%s", got, d.state.QuotaSchedulerOwnerID)
	}
}

func TestQuotaSchedulerSequentialAdoptsPreselectedRunningTask(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(d.dataDir, "task-plan.json")
	plan, err := saveTaskPlan(path, 0, []string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now)
	if err != nil {
		t.Fatal(err)
	}
	d.state.Threads[testQuotaA] = ThreadState{LastStartedTurnID: "manual-a", LastStartedAt: now, ObservedTurnRunning: true}
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	if got := d.dispatchDueLocked(now); len(got) != 0 || d.state.QuotaSchedulerOwnerID != testQuotaA {
		t.Fatalf("B overtook preselected running A: jobs=%+v owner=%s", got, d.state.QuotaSchedulerOwnerID)
	}
	if _, err := saveTaskPlan(path, plan.Revision, []string{testQuotaA, testQuotaB}, []string{testQuotaB}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := d.dispatchDueLocked(now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("unchecking A stopped its observed turn or launched B: %+v", got)
	}
	thread := d.state.Threads[testQuotaA]
	thread.ObservedTurnRunning = false
	d.state.Threads[testQuotaA] = thread
	if got := d.dispatchDueLocked(now.Add(2 * time.Second)); len(got) != 1 || got[0].ThreadID != testQuotaB {
		t.Fatalf("B remained blocked after unchecked A stopped: %+v", got)
	}
}

func TestQuotaSchedulerSwitchToStaggeredClearsSequentialHold(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(d.dataDir, "task-plan.json")
	if _, err := saveTaskPlan(path, 0, []string{testQuotaA, testQuotaB}, []string{testQuotaA, testQuotaB}, now); err != nil {
		t.Fatal(err)
	}
	d.state.QuotaSchedulerOwnerID = testQuotaA
	d.state.Threads[testQuotaA] = ThreadState{}
	d.state.Threads[testQuotaB] = quotaPending(testQuotaB, now)
	d.config.QuotaResumeMode = quotaModeStaggeredStart
	jobs := d.dispatchDueLocked(now)
	if len(jobs) != 1 || jobs[0].ThreadID != testQuotaB || d.state.QuotaSchedulerOwnerID != "" {
		t.Fatalf("staggered mode kept a stale sequential hold: jobs=%+v owner=%s", jobs, d.state.QuotaSchedulerOwnerID)
	}
	plan, err := loadTaskPlan(path)
	if err != nil || !plan.IsSelected(testQuotaA) {
		t.Fatalf("mode switch removed the user's watchlist selection: plan=%+v err=%v", plan, err)
	}
}

func TestSchedulerOwnerRemainsVisibleForManualConfirmation(t *testing.T) {
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	state := newRuntimeState()
	state.QuotaSchedulerOwnerID = testQuotaA
	state.Threads[testQuotaA] = ThreadState{LastStartedAt: now}
	cfg := defaultConfig()
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	tasks := ensureSchedulerOwnerTask(nil, state)
	if len(tasks) != 1 || tasks[0].ThreadID != testQuotaA || tasks[0].Status != "awaiting_confirmation" ||
		tasks[0].Label != "Codex Task "+shortThreadID(testQuotaA) ||
		!schedulerOwnerCanConfirm(state, cfg, true) {
		t.Fatalf("invisible or unactionable held owner: tasks=%+v", tasks)
	}
	official := []ManagedTask{{ThreadID: testQuotaA, Label: "Official task title", Status: "completed"}}
	tasks = ensureSchedulerOwnerTask(official, state)
	if len(tasks) != 1 || tasks[0].Label != "Official task title" || tasks[0].Status != "awaiting_confirmation" {
		t.Fatalf("official title was lost or turn completion misreported whole-task completion: %+v", tasks)
	}
	catalogRunning := []ManagedTask{{ThreadID: testQuotaA, Label: "Official task title", Status: "active"}}
	if got := ensureSchedulerOwnerTask(catalogRunning, state); got[0].Status != "active" {
		t.Fatalf("current catalog activity was hidden by older scanner state: %+v", got)
	}
	thread := state.Threads[testQuotaA]
	thread.ObservedTurnRunning = true
	state.Threads[testQuotaA] = thread
	tasks = ensureSchedulerOwnerTask(official, state)
	if tasks[0].Status != "running" || schedulerOwnerCanConfirm(state, cfg, true) {
		t.Fatalf("observed running task could be confirmed complete: %+v", tasks)
	}
}

func TestStoppedSchedulerOwnerKeepsReasonWhenRetryFallsOutOfRecentList(t *testing.T) {
	state := newRuntimeState()
	state.QuotaSchedulerOwnerID = testQuotaA
	state.Threads[testQuotaA] = ThreadState{Stopped: &StoppedRetry{
		Class: classRateLimit, Reason: "max_recovery_attempts",
	}}
	plan := TaskPlan{
		Configured: true, OrderedThreadIDs: []string{testQuotaA},
		SelectedThreadIDs: []string{testQuotaA},
	}
	for _, catalog := range [][]ManagedTask{
		nil,
		{{ThreadID: testQuotaA, Label: "Official task title", Status: "completed"}},
	} {
		tasks := ensureSchedulerOwnerTask(applyTaskPlan(catalog, plan), state)
		if len(tasks) != 1 || tasks[0].Status != "stopped" || !tasks[0].SelectedForMonitoring ||
			tasks[0].StopReason != "max_recovery_attempts" || tasks[0].Class != classRateLimit ||
			!tasks[0].IsRecovery {
			t.Fatalf("stopped owner lost watch or retry reason: %+v", tasks)
		}
	}
}

func TestQuotaSchedulerStaggeredWaitsFullGapAfterCapacityReturns(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeStaggeredStart
	cfg.MaxParallelSessions = 2
	cfg.ResumeIntervalSeconds = 30
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	for _, id := range []string{testQuotaA, testQuotaB, testQuotaC} {
		d.state.Threads[id] = quotaPending(id, now)
	}
	for index, at := range []time.Time{now, now.Add(30 * time.Second)} {
		jobs := d.dispatchDueLocked(at)
		if len(jobs) != 1 || jobs[0].ThreadID != []string{testQuotaA, testQuotaB}[index] {
			t.Fatalf("unexpected quota dispatch %d: %+v", index, jobs)
		}
	}
	if got := d.dispatchDueLocked(now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("parallel limit failed: %+v", got)
	}
	delete(d.active, testQuotaA)
	thread := d.state.Threads[testQuotaA]
	thread.Awaiting = nil
	d.state.Threads[testQuotaA] = thread
	if got := d.dispatchDueLocked(now.Add(61 * time.Second)); len(got) != 0 {
		t.Fatalf("slot release caused instant burst: %+v", got)
	}
	if got := d.dispatchDueLocked(now.Add(90 * time.Second)); len(got) != 0 {
		t.Fatalf("slot release gap was too short: %+v", got)
	}
	if got := d.dispatchDueLocked(now.Add(91 * time.Second)); len(got) != 1 || got[0].ThreadID != testQuotaC {
		t.Fatalf("C did not run after full slot-release gap: %+v", got)
	}
}

func TestQuotaSchedulerOwnerDoesNotBlockTransientRescue(t *testing.T) {
	cfg := isolatedConfig(t.TempDir())
	cfg.QuotaResumeMode = quotaModeSequentialCompletion
	cfg.ResumeIntervalSeconds = 0
	d := newTestDaemon(t, cfg, successfulRunner())
	now := time.Now().UTC()
	d.state.QuotaSchedulerOwnerID = testQuotaA
	d.state.Threads[testQuotaA] = ThreadState{ObservedTurnRunning: true}
	d.state.Threads[testQuotaB] = ThreadState{Pending: &PendingRetry{
		EventKey: "transient", Class: classServer, DueAt: now, Attempt: 1,
	}}
	jobs := d.dispatchDueLocked(now)
	if len(jobs) != 1 || jobs[0].ThreadID != testQuotaB || jobs[0].Class != classServer {
		t.Fatalf("sequential quota owner suppressed transient recovery: %+v", jobs)
	}
}

func TestQuotaSchedulerConfigMigrationKeepsLegacyBehavior(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	legacy := defaultConfig()
	legacy.ConfigVersion = 14
	legacy.SharedAppServerEnabled = true
	legacy.SharedAppServerRequested = true
	legacy.UsageRefreshMinutes = 1
	legacy.ResumeIntervalSeconds = 30
	if err := writeJSONAtomic(path, legacy); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadOrCreateConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ConfigVersion != 15 || !loaded.SharedAppServerEnabled || !loaded.SharedAppServerRequested ||
		loaded.UsageRefreshMinutes != 1 || loaded.ResumeIntervalSeconds != 30 ||
		loaded.QuotaResumeMode != quotaModeStaggeredStart || loaded.MaxParallelSessions != 2 {
		t.Fatalf("migration changed existing settings: %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(dir, "task-plan.json")); !os.IsNotExist(err) {
		t.Fatalf("config migration silently created a task selection file: %v", err)
	}
}
