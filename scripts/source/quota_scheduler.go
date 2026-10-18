package main

import (
	"path/filepath"
	"time"
)

const (
	quotaModeStaggeredStart       = "staggered_start"
	quotaModeSequentialCompletion = "sequential_completion"
	// A successful turn is not by itself proof that Codex has stopped. Wait
	// for a later healthy Session scan after a short quiet period, so native
	// follow-on turns and goals can be observed before handing off the slot.
	sequentialCompletionQuietPeriod = 30 * time.Second
)

// observedRunningCountLocked is deliberately a count of observed turns, not
// whole projects. An idle Codex thread is not proof that the user's task is
// complete, and external work not seen by the rollout scanner is unknowable.
func (d *daemon) observedRunningCountLocked() int {
	count := 0
	for id, thread := range d.state.Threads {
		running := thread.ObservedTurnRunning || thread.Awaiting != nil
		if job, exists := d.active[id]; exists && job.Kind == jobRecovery {
			running = true
		}
		if running {
			count++
		}
	}
	return count
}

func (d *daemon) quotaCapacityReadyLocked(now time.Time) bool {
	if d.config.QuotaResumeMode != quotaModeStaggeredStart {
		return true
	}
	if d.observedRunningCountLocked() >= d.config.MaxParallelSessions {
		d.state.QuotaCapacityWasFull = true
		d.state.QuotaCapacityAvailableSince = time.Time{}
		return false
	}
	if d.state.QuotaCapacityWasFull {
		d.state.QuotaCapacityWasFull = false
		d.state.QuotaCapacityAvailableSince = now
	}
	interval := time.Duration(d.config.ResumeIntervalSeconds) * time.Second
	return interval == 0 || d.state.QuotaCapacityAvailableSince.IsZero() ||
		!now.Before(d.state.QuotaCapacityAvailableSince.Add(interval))
}

func (d *daemon) quotaDispatchStartedLocked(threadID string) {
	if d.config.QuotaResumeMode == quotaModeSequentialCompletion && d.state.QuotaSchedulerOwnerID == "" {
		d.state.QuotaSchedulerOwnerID = threadID
	}
	if d.config.QuotaResumeMode == quotaModeStaggeredStart &&
		d.observedRunningCountLocked() >= d.config.MaxParallelSessions {
		d.state.QuotaCapacityWasFull = true
		d.state.QuotaCapacityAvailableSince = time.Time{}
	}
}

func removeTaskPlanSelection(path, threadID string, now time.Time) error {
	plan, err := loadTaskPlan(path)
	if err != nil || !plan.Configured || !plan.IsSelected(threadID) {
		return err
	}
	selected := make([]string, 0, len(plan.SelectedThreadIDs)-1)
	for _, id := range plan.SelectedThreadIDs {
		if id != threadID {
			selected = append(selected, id)
		}
	}
	_, err = saveTaskPlan(path, plan.Revision, plan.OrderedThreadIDs, selected, now)
	return err
}

func (d *daemon) clearQuotaOwnerLocked(threadID string, now time.Time, deselect bool) bool {
	if d.state.QuotaSchedulerOwnerID != threadID {
		return false
	}
	if deselect {
		if err := removeTaskPlanSelection(filepath.Join(d.dataDir, "task-plan.json"), threadID, now); err != nil {
			d.lastError = "task_plan_unavailable"
			d.logger.Printf("quota owner retained thread=%s reason=task_plan_update", shortThreadID(threadID))
			return false
		}
	}
	d.state.QuotaSchedulerOwnerID = ""
	d.logger.Printf("quota scheduler released thread=%s", shortThreadID(threadID))
	return true
}

// Sequential handoff means the current task has observably stopped, not that
// its user's overall project has been semantically completed. An explicit
// native goal completion or a verified, quiet terminal turn can hand off.
func (d *daemon) reconcileQuotaOwnerLocked(plan TaskPlan, now time.Time) {
	id := d.state.QuotaSchedulerOwnerID
	if id == "" {
		return
	}
	if d.config.QuotaResumeMode != quotaModeSequentialCompletion {
		// Switching to staggered mode removes the sequential hold, but keeps
		// the user's watchlist selection for future quota recovery.
		d.state.QuotaSchedulerOwnerID = ""
		return
	}
	thread, found := d.state.Threads[id]
	if !found {
		return // Missing state is unknown, not completed.
	}
	if thread.GoalObservedAt.After(thread.LastAutoRetryAt) && thread.GoalObservedAt.After(thread.LastStartedAt) &&
		(thread.GoalStatus == "completed" || thread.GoalStatus == "complete") &&
		!thread.ObservedTurnRunning && thread.Pending == nil && thread.Awaiting == nil &&
		!d.quotaJobActiveLocked(id) {
		d.clearQuotaOwnerLocked(id, now, true)
		return
	}
	if d.quotaOwnerQuietlyCompletedLocked(id, thread, now) {
		// A quiet turn releases this scheduling slot, not the user's
		// persistent watchlist choice; this task may be used again later.
		d.clearQuotaOwnerLocked(id, now, false)
		return
	}
	if plan.Configured && !plan.IsSelected(id) && !thread.ObservedTurnRunning &&
		thread.Awaiting == nil && !d.quotaJobActiveLocked(id) {
		d.clearQuotaOwnerLocked(id, now, false)
		return
	}
	// Stopped is a terminal retry-safety state, including for rate limits.
	// It must not hold the unattended sequence forever. The watchlist choice
	// remains selected, while the stopped retry itself is not restarted.
	if thread.Stopped != nil &&
		thread.Pending == nil && thread.Awaiting == nil &&
		!thread.ObservedTurnRunning && !d.quotaJobActiveLocked(id) {
		d.clearQuotaOwnerLocked(id, now, false)
	}
}

func (d *daemon) quotaOwnerQuietlyCompletedLocked(id string, thread ThreadState, now time.Time) bool {
	// With automatic Session scanning disabled, a manual refresh is not an
	// unattended monitoring contract. Keep the owner until an explicit goal
	// completion or selection change.
	if !d.config.AutoScanEnabled {
		return false
	}
	// A paused, blocked, usage-limited, or active goal is not a free slot.
	// Unknown goal states also fail closed. Explicit completed goals are
	// handled above, rather than inferred from a turn's final reply.
	if thread.GoalStatus != "" || thread.GoalHeld || thread.ObservedTurnRunning ||
		thread.Pending != nil || thread.Awaiting != nil || thread.Stopped != nil ||
		thread.GoalStop != nil || d.quotaJobActiveLocked(id) {
		return false
	}
	completedAt, observedAt := thread.LastPositiveCompletionAt, thread.LastPositiveCompletionObservedAt
	positiveTurn := !completedAt.IsZero() && !observedAt.IsZero() &&
		!completedAt.Before(thread.LastStartedAt) && !completedAt.Before(thread.LastAutoRetryAt)
	if !positiveTurn {
		// An explicitly aborted turn or a controller's terminal "not
		// applicable" result also has no retry left to run. This is a
		// scheduling handoff, never a claim that the project succeeded.
		observedAt = thread.LastTerminalNoRetryAt
		if observedAt.IsZero() || observedAt.Before(thread.LastStartedAt) ||
			observedAt.Before(thread.LastAutoRetryAt) {
			return false
		}
	}
	settledAt := observedAt.Add(sequentialCompletionQuietPeriod)
	return !now.Before(settledAt) && !d.lastHealthyScan.Before(settledAt) &&
		d.lastHealthyScan.After(observedAt)
}

// A running preselected task may reach its quota later. While it is
// observably active, do not let another eligible quota item overtake it in
// sequential mode. This is best effort because external work can be unseen.
func (d *daemon) adoptSequentialRunningLocked(plan TaskPlan) {
	if d.config.QuotaResumeMode != quotaModeSequentialCompletion ||
		d.state.QuotaSchedulerOwnerID != "" || !plan.Configured {
		return
	}
	for _, id := range plan.OrderedThreadIDs {
		if !plan.IsSelected(id) {
			continue
		}
		thread, found := d.state.Threads[id]
		if found && thread.ObservedTurnRunning {
			d.state.QuotaSchedulerOwnerID = id
			d.logger.Printf("quota scheduler adopted running thread=%s", shortThreadID(id))
			return
		}
	}
}

func (d *daemon) quotaJobActiveLocked(threadID string) bool {
	job, active := d.active[threadID]
	return active && job.Kind == jobRecovery
}

func (d *daemon) confirmTaskCompleteLocked(threadID string, now time.Time) bool {
	if d.state.QuotaSchedulerOwnerID != threadID {
		return false
	}
	thread, found := d.state.Threads[threadID]
	if (found && (thread.ObservedTurnRunning || thread.Awaiting != nil)) || d.quotaJobActiveLocked(threadID) {
		return false
	}
	if !d.clearQuotaOwnerLocked(threadID, now, true) {
		return false
	}
	// This is a user's completion declaration, never an inference from a
	// successful turn. Retire only this thread's pending recovery work.
	if found {
		thread.Pending = nil
		thread.Stopped = nil
		thread.GoalStop = nil
		thread.RecoveryAttempts = 0
		thread.ConsecutiveRetries = 0
		d.state.Threads[threadID] = thread
	}
	return true
}
