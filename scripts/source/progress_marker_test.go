package main

import (
	"testing"
	"time"
)

func TestIrrelevantProgressDoesNotAddProcessedEventMarkers(t *testing.T) {
	threadID := "019fa94e-0103-7183-b405-36bd307b6dc5"
	startedAt := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	event := RelevantEvent{Kind: "task_progress", Timestamp: startedAt.Add(time.Second)}
	tests := []struct {
		name   string
		thread ThreadState
	}{
		{name: "ordinary turn", thread: ThreadState{LastStartedTurnID: "manual"}},
		{name: "queued retry", thread: ThreadState{LastStartedTurnID: "manual", Pending: &PendingRetry{FailedTurnID: "manual"}}},
		{name: "retry not acknowledged", thread: ThreadState{LastStartedTurnID: "manual", Awaiting: &AwaitingRetry{}}},
		{name: "different started turn", thread: ThreadState{LastStartedTurnID: "manual", Awaiting: &AwaitingRetry{RetryTurnID: "retry"}}},
		{name: "progress before retry start", thread: ThreadState{LastStartedTurnID: "retry", Awaiting: &AwaitingRetry{RetryTurnID: "retry", StartedAt: startedAt.Add(2 * time.Second)}}},
		{name: "progress already known", thread: ThreadState{LastStartedTurnID: "retry", CurrentTurnProgress: true, Awaiting: &AwaitingRetry{RetryTurnID: "retry", StartedAt: startedAt}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDaemon(t, isolatedConfig(t.TempDir()), successfulRunner())
			d.state.Threads[threadID] = tt.thread
			// Old derived markers are not deleted in this change; they expire
			// under the existing state-retention policy.
			oldKey := "legacy-progress-marker"
			d.state.ProcessedEvents[oldKey] = startedAt
			for i := 0; i < 100; i++ {
				progress := event
				progress.Timestamp = event.Timestamp.Add(time.Duration(i) * time.Millisecond)
				d.handleEventLocked(scannedEvent{ThreadID: threadID, Event: progress}, progress.Timestamp)
			}
			if len(d.state.ProcessedEvents) != 1 {
				t.Fatalf("irrelevant progress persisted %d extra dedup markers", len(d.state.ProcessedEvents)-1)
			}
			if _, retained := d.state.ProcessedEvents[oldKey]; !retained {
				t.Fatal("existing dedup marker was removed")
			}
			if d.state.Threads[threadID].CurrentTurnProgress != tt.thread.CurrentTurnProgress {
				t.Fatal("irrelevant progress changed retry guard")
			}
		})
	}
}

func TestCorrelatedProgressMarkerSurvivesReplayAndRecovery(t *testing.T) {
	threadID := "019fa94e-0103-7183-b405-36bd307b6dc6"
	now := time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC)
	d := newTestDaemon(t, isolatedConfig(t.TempDir()), successfulRunner())
	d.handleEventLocked(failureScannedEvent(threadID, "initial", now), now)
	first := d.state.Threads[threadID].Pending
	if first == nil {
		t.Fatal("initial retry was not queued")
	}
	setRunningRetry(d, threadID, first, "retry-1", false)
	thread := d.state.Threads[threadID]
	thread.Awaiting.StartedAt = now.Add(time.Second)
	d.state.Threads[threadID] = thread
	before := len(d.state.ProcessedEvents)
	progress := scannedEvent{ThreadID: threadID, Event: RelevantEvent{
		Kind: "task_progress", Timestamp: now.Add(2 * time.Second),
	}}
	d.handleEventLocked(progress, progress.Event.Timestamp)
	if !d.state.Threads[threadID].CurrentTurnProgress || len(d.state.ProcessedEvents) != before+1 {
		t.Fatalf("correlated progress was not retained once: progress=%v markers=%d", d.state.Threads[threadID].CurrentTurnProgress, len(d.state.ProcessedEvents)-before)
	}
	progress.Mirrored = true
	d.handleEventLocked(progress, now.Add(3*time.Second))
	newProgress := progress
	newProgress.Event.Timestamp = now.Add(4 * time.Second)
	d.handleEventLocked(newProgress, newProgress.Event.Timestamp)
	if len(d.state.ProcessedEvents) != before+1 {
		t.Fatal("mirrored or additional progress created unnecessary markers")
	}

	failedAt := now.Add(10 * time.Second)
	d.handleEventLocked(failureScannedEvent(threadID, "retry-1", failedAt), failedAt)
	second := d.state.Threads[threadID].Pending
	if second == nil || second.Attempt != 2 || second.ConsecutiveRetry != 1 {
		t.Fatalf("progress did not reset the no-progress count safely: %+v", second)
	}
	before = len(d.state.ProcessedEvents)
	d.handleEventLocked(newProgress, now.Add(11*time.Second))
	if len(d.state.ProcessedEvents) != before {
		t.Fatal("replayed progress while queued added a marker")
	}

	setRunningRetry(d, threadID, second, "retry-2", false)
	thread = d.state.Threads[threadID]
	thread.Awaiting.StartedAt = now.Add(20 * time.Second)
	d.state.Threads[threadID] = thread
	stale := scannedEvent{ThreadID: threadID, Event: RelevantEvent{
		Kind: "task_progress", Timestamp: now.Add(12 * time.Second),
	}}
	d.handleEventLocked(stale, now.Add(20*time.Second))
	if len(d.state.ProcessedEvents) != before || d.state.Threads[threadID].CurrentTurnProgress {
		t.Fatal("stale progress changed the new retry")
	}
	secondProgress := scannedEvent{ThreadID: threadID, Event: RelevantEvent{
		Kind: "task_progress", Timestamp: now.Add(21 * time.Second),
	}}
	d.handleEventLocked(secondProgress, secondProgress.Event.Timestamp)
	if len(d.state.ProcessedEvents) != before+1 || !d.state.Threads[threadID].CurrentTurnProgress {
		t.Fatal("new retry's progress was not recorded")
	}
	d.handleEventLocked(scannedEvent{ThreadID: threadID, Event: RelevantEvent{
		Kind: "task_complete", TurnID: "retry-2", Timestamp: now.Add(30 * time.Second), FinalKnown: true, FinalPresent: true,
	}}, now.Add(30*time.Second))
	thread = d.state.Threads[threadID]
	if thread.Pending != nil || thread.Awaiting != nil || thread.CurrentTurnProgress {
		t.Fatalf("successful recovery was not cleared: %+v", thread)
	}
	if thread.LastRecoverySucceededAt.IsZero() {
		t.Fatal("successful matching retry turn did not record a visible recovery outcome")
	}
	before = len(d.state.ProcessedEvents)
	d.handleEventLocked(secondProgress, now.Add(31*time.Second))
	if len(d.state.ProcessedEvents) != before {
		t.Fatal("completed retry replay added a progress marker")
	}
	d.handleEventLocked(scannedEvent{ThreadID: threadID, Event: RelevantEvent{
		Kind: "task_started", TurnID: "later-manual-turn", Timestamp: now.Add(32 * time.Second),
	}}, now.Add(32*time.Second))
	if !d.state.Threads[threadID].LastRecoverySucceededAt.IsZero() {
		t.Fatal("old resume success remained current after a new turn")
	}
}
