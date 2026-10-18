package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var errStopRequested = errors.New("stop requested")

const (
	statusHeartbeatInterval         = 10 * time.Second
	statePruneInterval              = 60 * time.Second
	stateIdleRecheckInterval        = 60 * time.Second
	stateActiveRecheckInterval      = 10 * time.Second
	stateFileCheckInterval          = 10 * time.Second
	readyControllerProbeInterval    = 30 * time.Second
	recoveryControllerProbeInterval = 10 * time.Second
)

type daemon struct {
	config      Config
	dataDir     string
	statePath   string
	statusPath  string
	stopPath    string
	controlPath string
	commandDir  string
	logger      *safeLogger
	runner      resumeRunner
	startedAt   time.Time

	mu                                  sync.Mutex
	wg                                  sync.WaitGroup
	state                               RuntimeState
	active                              map[string]RetryJob
	activeCtx                           map[string]context.CancelFunc
	lastScan                            time.Time
	lastHealthyScan                     time.Time
	lastStatePersist                    time.Time
	lastStatePrune                      time.Time
	lastStateFileCheck                  time.Time
	lastPersistedStateHash              [sha256.Size]byte
	hasPersistedState                   bool
	lastPersistedFileSize               int64
	lastPersistedFileModTime            time.Time
	hasPersistedFileMetadata            bool
	lastHeartbeat                       time.Time
	lastStatusPersist                   time.Time
	lastPublishedStatus                 StatusSnapshot
	lastRootCount                       int
	lastError                           string
	controllerState                     string
	lastControllerProbe                 time.Time
	paused                              bool
	monitoringPaused                    bool
	memoryUsageBytes                    uint64
	memoryGuardTriggered                bool
	sharedAppServerMemoryBytes          uint64
	sharedAppServerMemoryGuardTriggered bool
	lastSharedAppServerMemoryCheck      time.Time
	writeState                          func(string, []byte) error
	writeStatusFile                     func(string, any) error
	stateWriteDeferred                  bool
	statusWriteDeferred                 bool
	taskPlanErrorLogged                 bool
}

func newDaemon(config Config, dataDir string, logger *safeLogger, runner resumeRunner) (*daemon, error) {
	configPath := filepath.Join(dataDir, "config.json")
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		if _, err := updateConfigFile(configPath, func(current *Config) error {
			*current = config
			return nil
		}); err != nil {
			return nil, err
		}
	}
	statePath := filepath.Join(dataDir, "state.json")
	state, err := loadState(statePath)
	if err != nil {
		return nil, err
	}
	controlPath := filepath.Join(dataDir, "control.json")
	control, err := loadOrCreateControlState(controlPath)
	if err != nil {
		return nil, err
	}
	daemon := &daemon{
		config:           config,
		dataDir:          dataDir,
		statePath:        statePath,
		statusPath:       filepath.Join(dataDir, "status.json"),
		stopPath:         filepath.Join(dataDir, "stop.signal"),
		controlPath:      controlPath,
		commandDir:       filepath.Join(dataDir, "commands"),
		logger:           logger,
		runner:           runner,
		startedAt:        time.Now().UTC(),
		state:            state,
		active:           make(map[string]RetryJob),
		activeCtx:        make(map[string]context.CancelFunc),
		paused:           control.Paused,
		monitoringPaused: control.MonitoringPaused,
		controllerState:  "starting",
		writeState:       writeBytesAtomic,
		writeStatusFile:  writeJSONAtomic,
	}
	daemon.reconcileStartupState(time.Now().UTC())
	return daemon, nil
}

func (d *daemon) run(ctx context.Context) error {
	_ = os.Remove(d.stopPath)
	if err := d.tick(ctx, time.Now().UTC()); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Duration(d.config.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := d.tick(ctx, now.UTC()); err != nil {
				return err
			}
		}
	}
}

func (d *daemon) tick(ctx context.Context, now time.Time) error {
	if _, err := os.Stat(d.stopPath); err == nil {
		_ = os.Remove(d.stopPath)
		return errStopRequested
	}
	control, controlErr := loadOrCreateControlState(d.controlPath)
	d.mu.Lock()
	if controlErr != nil {
		d.lastError = controlErr.Error()
		d.logger.Printf("control state failed category=control_state")
	} else {
		d.paused = control.Paused
		d.monitoringPaused = control.MonitoringPaused
	}
	d.lastHeartbeat = now
	if d.monitoringPaused {
		statusErr := d.writeStatusLocked(true, d.lastRootCount)
		d.mu.Unlock()
		if statusErr != nil {
			d.logger.Printf("status save deferred category=status_write")
		}
		return nil
	}
	d.mu.Unlock()

	d.processSharedRecovery(ctx, now)
	controllerReady := d.controllerRestartReady(ctx, now)
	d.mu.Lock()
	reopened := false
	if controllerReady {
		reopened = d.reopenRestartRequiredLocked(now)
	}
	d.reloadConfigLocked()
	baseline := !d.state.Initialized
	manualScan := consumeTaskScanRequest(d.dataDir)
	scanInterval := time.Duration(d.config.ScanIntervalSeconds) * time.Second
	shouldScan := baseline || manualScan || (d.config.AutoScanEnabled && (d.lastScan.IsZero() || now.Sub(d.lastScan) >= scanInterval))
	rootCount := d.lastRootCount
	var events []scannedEvent
	var scanErr error
	if shouldScan {
		roots := discoverSessionRoots(d.config)
		rootCount = len(roots)
		events, scanErr = scanSessions(roots, &d.state, now, baseline)
		if baseline {
			d.state.Initialized = true
			d.logger.Printf("initialized session baseline roots=%d", len(roots))
		}
		if scanErr != nil {
			d.lastError = scanErr.Error()
			d.logger.Printf("scan failed category=session_scan")
		} else {
			d.lastError = ""
			if rootCount > 0 {
				d.lastHealthyScan = now
			}
		}
		d.lastScan = now
		d.lastRootCount = rootCount
		if manualScan {
			d.logger.Debugf("manual task scan completed roots=%d events=%d", rootCount, len(events))
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Event.Timestamp.Before(events[j].Event.Timestamp)
	})
	for _, item := range events {
		// Ignore old history only when another watched store already owns this
		// thread. Failures written while the watchdog was stopped must survive a
		// restart.
		if item.Mirrored && item.Event.Timestamp.Before(d.startedAt.Add(-3*time.Second)) {
			continue
		}
		d.handleEventLocked(item, now)
	}
	controlsChanged := d.refreshControlsLocked(now)
	expired := d.expireUnacknowledgedLocked(now)
	sharedBackendEnabled := d.config.SharedAppServerEnabled
	// Acknowledged turns normally finish through rollout events. If a previous
	// process stopped after task_started, release the mutex while the optional
	// app-server lifecycle probe verifies that the turn is still active. The
	// probe never starts a task and its result is revalidated under the lock.
	d.mu.Unlock()
	d.checkSharedAppServerMemory(ctx, now)
	if !sharedBackendEnabled {
		d.reconcileSharedBackendCleanup(ctx)
	}
	lifecycleChanged := d.reconcileAwaitingLifecycle(ctx, now)
	d.mu.Lock()
	pendingBeforeDispatch := pendingRetryCount(d.state)
	jobs := d.dispatchDueLocked(now)
	d.lastHeartbeat = now
	pruneDue := d.lastStatePrune.IsZero() || now.Sub(d.lastStatePrune) >= statePruneInterval ||
		len(d.state.ProcessedEvents) > maxProcessedEventEntries ||
		len(d.state.Files) > maxFileCursorEntries
	if pruneDue {
		d.state.prune(now)
		d.lastStatePrune = now
	}
	pendingCount := pendingRetryCount(d.state)
	activeCount := len(d.active)
	var stateErr error
	stateFileNeedsRepair := false
	if d.hasPersistedFileMetadata && (d.lastStateFileCheck.IsZero() || now.Sub(d.lastStateFileCheck) >= stateFileCheckInterval) {
		d.lastStateFileCheck = now
		info, err := os.Stat(d.statePath)
		stateFileNeedsRepair = err != nil || info.Size() != d.lastPersistedFileSize ||
			!info.ModTime().Equal(d.lastPersistedFileModTime)
	}
	recheckInterval := stateIdleRecheckInterval
	if pendingCount > 0 || activeCount > 0 {
		recheckInterval = stateActiveRecheckInterval
	}
	if shouldScan || len(jobs) > 0 || reopened || controlsChanged || expired || lifecycleChanged ||
		pendingBeforeDispatch != pendingCount || pruneDue || stateFileNeedsRepair || d.stateWriteDeferred ||
		d.lastStatePersist.IsZero() || now.Sub(d.lastStatePersist) >= recheckInterval {
		stateErr = d.persistStateLocked()
		if stateErr == nil {
			d.lastStatePersist = now
		}
	}
	statusErr := d.writeStatusLocked(true, rootCount)
	d.mu.Unlock()
	if shouldScan {
		d.logger.Debugf("scan completed roots=%d events=%d queued_jobs=%d pending_retries=%d active_retries=%d", rootCount, len(events), len(jobs), pendingCount, activeCount)
	}

	for _, job := range jobs {
		d.wg.Add(1)
		go d.runJob(ctx, job)
	}
	if scanErr != nil {
		return nil
	}
	if stateErr != nil {
		// The in-memory state remains authoritative. Windows indexers, security
		// scanners, and settings readers can briefly hold state.json without
		// delete sharing, so one failed atomic replacement must not terminate the
		// watchdog and remove its tray icon. The next tick retries persistence.
		d.logger.Printf("state save deferred category=state_write")
	}
	if statusErr != nil {
		d.logger.Printf("status save deferred category=status_write")
	}
	return nil
}

func pendingRetryCount(state RuntimeState) int {
	count := 0
	for _, thread := range state.Threads {
		if thread.Pending != nil {
			count++
		}
	}
	return count
}

func (d *daemon) checkSharedAppServerMemory(ctx context.Context, now time.Time) {
	d.mu.Lock()
	if !d.config.SharedAppServerEnabled || d.sharedAppServerMemoryGuardTriggered ||
		(!d.lastSharedAppServerMemoryCheck.IsZero() && now.Sub(d.lastSharedAppServerMemoryCheck) < memoryCheckInterval) {
		d.mu.Unlock()
		return
	}
	limitMB := d.config.SharedAppServerMemoryLimitMB
	reader, ok := d.runner.(sharedBackendMemoryReader)
	d.lastSharedAppServerMemoryCheck = now
	d.mu.Unlock()
	if !ok || limitMB <= 0 {
		return
	}
	sample, err := reader.SharedBackendMemory(ctx)
	if err != nil {
		if d.logger != nil {
			d.logger.Printf("shared app-server memory sample unavailable category=memory_probe")
		}
		return
	}
	d.mu.Lock()
	d.sharedAppServerMemoryBytes = sample.PrivateBytes
	if sample.PrivateBytes < uint64(limitMB)*1024*1024 || d.sharedAppServerMemoryGuardTriggered {
		d.mu.Unlock()
		return
	}
	d.sharedAppServerMemoryGuardTriggered = true
	d.controllerState = "shared_app_server_memory_limit_exceeded"
	d.lastError = "shared_app_server_memory_limit_exceeded"
	if d.logger != nil {
		d.logger.Printf("shared app-server memory guard triggered private_memory_mb=%d limit_mb=%d action=disable_shared_mode", memoryBytesToMB(sample.PrivateBytes), limitMB)
	}
	d.mu.Unlock()

	// This is intentionally a fail-open operation, not a process kill. If
	// Desktop is still connected, cleanup is deferred until it closes so the
	// visible Codex session is not torn down by the monitor.
	state := d.failOpenSharedBackend(ctx, "shared_app_server_memory_limit_exceeded")
	d.mu.Lock()
	if state == "shared_app_server_disabled" {
		d.config.SharedAppServerEnabled = false
	}
	d.controllerState = state
	d.mu.Unlock()
}

func (d *daemon) reconcileSharedBackendCleanup(ctx context.Context) {
	cleaner, ok := d.runner.(sharedBackendCleanupReconciler)
	if !ok {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := cleaner.ReconcileSharedBackendCleanup(cleanupCtx)
	cancel()
	if err == nil || errors.Is(err, errSharedServerMigrationDeferred) {
		return
	}
	d.mu.Lock()
	d.lastError = controllerFailureReason(DispatchResult{}, err)
	d.mu.Unlock()
	if d.logger != nil {
		d.logger.Printf("shared app-server deferred cleanup failed category=%s", controllerFailureReason(DispatchResult{}, err))
	}
}

func (d *daemon) persistStateLocked() error {
	data, err := marshalRuntimeState(d.state)
	if err != nil {
		d.stateWriteDeferred = true
		d.lastError = "state_write_deferred"
		return err
	}
	hash := sha256.Sum256(data)
	if d.hasPersistedState && hash == d.lastPersistedStateHash {
		// A missing or externally replaced state.json must be repaired even
		// when the in-memory state is unchanged. Stat is cheap compared with
		// encoding and atomically rewriting a large state file.
		if info, err := os.Stat(d.statePath); err == nil && d.hasPersistedFileMetadata &&
			info.Size() == d.lastPersistedFileSize && info.ModTime().Equal(d.lastPersistedFileModTime) {
			d.stateWriteDeferred = false
			if d.lastError == "state_write_deferred" {
				d.lastError = ""
			}
			return nil
		}
	}
	writer := d.writeState
	if writer == nil {
		writer = writeBytesAtomic
	}
	if err := writer(d.statePath, data); err != nil {
		d.stateWriteDeferred = true
		d.lastError = "state_write_deferred"
		return err
	}
	d.lastPersistedStateHash = hash
	d.hasPersistedState = true
	if info, err := os.Stat(d.statePath); err == nil {
		d.lastPersistedFileSize = info.Size()
		d.lastPersistedFileModTime = info.ModTime()
		d.hasPersistedFileMetadata = true
		d.lastStateFileCheck = d.lastHeartbeat
	} else {
		d.hasPersistedFileMetadata = false
	}
	if d.stateWriteDeferred {
		d.stateWriteDeferred = false
		if d.lastError == "state_write_deferred" {
			d.lastError = ""
		}
		d.logger.Printf("state save recovered category=state_write")
	}
	return nil
}

func (d *daemon) recordMemorySample(sample memorySample) {
	d.mu.Lock()
	d.memoryUsageBytes = sample.PrivateBytes
	d.mu.Unlock()
}

func (d *daemon) handleMemoryLimit(sample memorySample) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.memoryUsageBytes = sample.PrivateBytes
	if d.memoryGuardTriggered {
		return
	}
	d.memoryGuardTriggered = true
	d.controllerState = "memory_limit_exceeded"
	d.lastError = "memory_limit_exceeded"
	if d.logger != nil {
		d.logger.Printf("memory guard triggered pid=%d private_memory_mb=%d limit_mb=%d", os.Getpid(), memoryBytesToMB(sample.PrivateBytes), d.config.MemoryLimitMB)
	}
	// Publish the terminal state before the process exits so the manager can
	// distinguish a guarded shutdown from a stale heartbeat.
	_ = d.writeStatusLocked(false, len(discoverSessionRoots(d.config)))
}

func (d *daemon) controllerRestartReady(ctx context.Context, now time.Time) bool {
	runner, ok := d.runner.(controllerStateRunner)
	if !ok {
		return false
	}
	d.mu.Lock()
	// A stale failure state must clear even after every stopped task was
	// resolved; keying the probe off the queue alone left
	// "codex_restart_required" displayed forever once no retry was waiting.
	// Shared mode is a resident service boundary, not only a retry dependency.
	// Keep probing it even when the retry queue is empty so an app-server that
	// exits after login is recreated before Codex receives a dead endpoint.
	needsProbe := d.config.SharedAppServerEnabled ||
		d.controllerState == "codex_restart_required" ||
		d.controllerState == "codex_not_running" ||
		d.controllerState == "shared_app_server_config_invalid"
	if !needsProbe {
		for _, thread := range d.state.Threads {
			if thread.Stopped != nil && (thread.Stopped.Reason == "codex_restart_required" ||
				thread.Stopped.Reason == "codex_not_running") {
				needsProbe = true
				break
			}
		}
	}
	probeInterval := recoveryControllerProbeInterval
	if d.config.SharedAppServerEnabled && d.controllerState == "ready" {
		probeInterval = readyControllerProbeInterval
	}
	if !needsProbe || (!d.lastControllerProbe.IsZero() && now.Sub(d.lastControllerProbe) < probeInterval) {
		d.mu.Unlock()
		return false
	}
	sharedEnabled := d.config.SharedAppServerEnabled
	latchedConfigFailure := d.controllerState == "shared_app_server_config_invalid"
	d.lastControllerProbe = now
	d.mu.Unlock()
	if sharedEnabled && latchedConfigFailure {
		state := d.failOpenSharedBackend(ctx, "shared_app_server_config_invalid")
		d.mu.Lock()
		if state == "shared_app_server_disabled" {
			d.config.SharedAppServerEnabled = false
		}
		d.controllerState = state
		if state != "shared_app_server_disabled" {
			d.lastError = "shared_app_server_config_invalid"
		}
		d.mu.Unlock()
		return false
	}
	state := runner.ControllerState(ctx)
	if sharedEnabled && controllerFailureNeedsFailOpen(state) {
		state = d.failOpenSharedBackend(ctx, state)
	}
	d.mu.Lock()
	if state == "shared_app_server_disabled" {
		d.config.SharedAppServerEnabled = false
	}
	d.controllerState = state
	d.mu.Unlock()
	return state == "ready"
}

func (d *daemon) failOpenSharedBackend(ctx context.Context, reason string) string {
	_ = recordSharedUnavailable(d.dataDir, reason)
	cleaner, supported := d.runner.(sharedBackendFailureHandler)
	if !supported {
		return reason
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(ctx, 5*time.Second)
	cleanupErr := cleaner.FailOpenSharedBackend(cleanupCtx)
	cleanupCancel()
	if cleanupErr != nil {
		d.logger.Printf("shared app-server fail-open cleanup failed category=%s", reason)
		return reason
	}
	d.logger.Printf("shared app-server disabled after runtime failure category=%s", reason)
	return "shared_app_server_disabled"
}

func (d *daemon) reopenRestartRequiredLocked(now time.Time) bool {
	changed := false
	for threadID, thread := range d.state.Threads {
		stopped := thread.Stopped
		if stopped == nil || stopped.Reason != "codex_restart_required" {
			continue
		}
		attempt := stopped.Attempts + 1
		consecutive := stopped.ConsecutiveRetries + 1
		if attempt > stopped.MaxAttempts || consecutive > stopped.MaxConsecutive {
			continue
		}
		thread.Stopped = nil
		thread.RecoveryAttempts = attempt
		thread.ConsecutiveRetries = consecutive
		thread.Pending = &PendingRetry{
			EventKey: stopped.EventKey, FailedTurnID: stopped.FailedTurnID, FailedAt: stopped.FailedAt,
			OriginTurnStartedAt: stopped.OriginTurnStartedAt,
			Class:               stopped.Class, DueAt: now, CodexHome: stopped.CodexHome, RolloutPath: stopped.RolloutPath,
			Attempt: attempt, MaxAttempts: stopped.MaxAttempts,
			ConsecutiveRetry: consecutive, MaxConsecutive: stopped.MaxConsecutive,
		}
		d.state.Threads[threadID] = thread
		changed = true
		d.logger.Printf("retry reopened thread=%s reason=codex_restart_completed", shortThreadID(threadID))
	}
	return changed
}

func (d *daemon) refreshControlsLocked(now time.Time) bool {
	control, err := loadOrCreateControlState(d.controlPath)
	if err != nil {
		d.lastError = err.Error()
		d.logger.Printf("control state failed category=control_state")
		return false
	}
	d.paused = control.Paused
	d.monitoringPaused = control.MonitoringPaused
	commands, invalid, err := loadControlCommandFiles(d.commandDir)
	if err != nil {
		d.lastError = err.Error()
		d.logger.Printf("control commands failed category=control_commands")
		return false
	}
	for _, path := range invalid {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			d.lastError = removeErr.Error()
		}
		d.logger.Printf("control command discarded category=invalid_command")
	}
	for _, item := range commands {
		d.applyControlCommandLocked(item.Command, now)
		if removeErr := os.Remove(item.Path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			d.lastError = removeErr.Error()
			d.logger.Printf("control command cleanup failed category=control_commands")
		}
	}
	return len(commands) > 0
}

func (d *daemon) expireUnacknowledgedLocked(now time.Time) bool {
	changed := false
	for threadID, thread := range d.state.Threads {
		if thread.Awaiting == nil || thread.Awaiting.RetryTurnID != "" || thread.Awaiting.StartDeadline.After(now) {
			continue
		}
		if _, active := d.active[threadID]; active {
			continue
		}
		d.rescheduleAwaitingLocked(threadID, thread, now, "start_not_acknowledged")
		changed = true
	}
	return changed
}

const (
	awaitingLifecycleConfirmations = 2
	awaitingLifecycleProbeInterval = 5 * time.Second
)

type awaitingLifecycleCandidate struct {
	threadID  string
	eventKey  string
	turnID    string
	codexHome string
}

// reconcileAwaitingLifecycle repairs the narrow crash window after Codex has
// acknowledged a retry turn but before its task_complete event reaches the
// rollout file. An active turn remains untouched. A non-active result must be
// observed twice, separated by a probe interval, before the retry is advanced;
// this prevents a transient app-server status from creating a duplicate turn.
func (d *daemon) reconcileAwaitingLifecycle(ctx context.Context, now time.Time) bool {
	reader, ok := d.runner.(retryLifecycleReader)
	if !ok {
		return false
	}

	changed := false
	d.mu.Lock()
	candidates := make([]awaitingLifecycleCandidate, 0)
	for threadID, thread := range d.state.Threads {
		awaiting := thread.Awaiting
		if awaiting == nil || awaiting.RetryTurnID == "" {
			continue
		}
		startedAt := awaiting.StartedAt
		if startedAt.IsZero() {
			startedAt = awaiting.DispatchStartedAt
		}
		// A headless CLI resume is not owned by the Desktop IPC controller, so
		// probing that controller would incorrectly stop a successfully started
		// fallback. Rollout task_started/task_complete events remain authoritative.
		// Bound a missing completion to avoid retaining stale state forever, but
		// never redispatch automatically because the CLI process may still exist.
		if awaiting.DispatchMode == "cli_fallback" {
			if !startedAt.IsZero() && now.Sub(startedAt) >= maxAutomaticRecoveryDuration {
				d.stopAwaitingForControllerLocked(threadID, thread, now, "cli_fallback_timeout")
				changed = true
			}
			continue
		}
		if startedAt.IsZero() || now.Sub(startedAt) < time.Duration(d.config.StartAckTimeoutSeconds)*time.Second {
			continue
		}
		if !awaiting.LastLifecycleCheckAt.IsZero() && now.Sub(awaiting.LastLifecycleCheckAt) < awaitingLifecycleProbeInterval {
			continue
		}
		awaiting.LastLifecycleCheckAt = now
		thread.Awaiting = awaiting
		d.state.Threads[threadID] = thread
		changed = true
		candidates = append(candidates, awaitingLifecycleCandidate{
			threadID: threadID, eventKey: awaiting.EventKey, turnID: awaiting.RetryTurnID,
			codexHome: awaiting.CodexHome,
		})
	}
	d.mu.Unlock()

	for _, candidate := range candidates {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, err := reader.RetryThreadStatus(probeCtx, candidate.threadID, candidate.codexHome)
		cancel()

		d.mu.Lock()
		thread, found := d.state.Threads[candidate.threadID]
		awaiting := thread.Awaiting
		if !found || awaiting == nil || awaiting.EventKey != candidate.eventKey || awaiting.RetryTurnID != candidate.turnID {
			d.mu.Unlock()
			continue
		}
		if err != nil {
			reason := controllerFailureReason(DispatchResult{}, err)
			d.controllerState = reason
			nextFailures := awaiting.DispatchFailures + 1
			if controllerFailureNeedsAction(reason) || nextFailures >= d.config.ControllerFailureLimit {
				d.stopAwaitingForControllerLocked(candidate.threadID, thread, now, reason)
				changed = true
				d.mu.Unlock()
				continue
			}
			// A transient controller/read failure is not evidence that the turn
			// stopped. Keep the inactive confirmation count intact, but bound
			// repeated probe failures using the same local-controller limit as
			// initial dispatch.
			awaiting.DispatchFailures = nextFailures
			thread.Awaiting = awaiting
			d.state.Threads[candidate.threadID] = thread
			d.logger.Printf("retry lifecycle probe deferred thread=%s reason=%s controller_failures=%d/%d", shortThreadID(candidate.threadID), reason, nextFailures, d.config.ControllerFailureLimit)
			d.mu.Unlock()
			continue
		}
		d.controllerState = "ready"
		awaiting.DispatchFailures = 0
		if retryThreadIsActive(status) {
			awaiting.LifecycleChecks = 0
			thread.Awaiting = awaiting
			d.state.Threads[candidate.threadID] = thread
			d.mu.Unlock()
			continue
		}
		awaiting.LifecycleChecks++
		thread.Awaiting = awaiting
		if awaiting.LifecycleChecks < awaitingLifecycleConfirmations {
			d.state.Threads[candidate.threadID] = thread
			d.logger.Printf("retry lifecycle inactive confirmation thread=%s status=%s confirmation=%d/%d", shortThreadID(candidate.threadID), status, awaiting.LifecycleChecks, awaitingLifecycleConfirmations)
			d.mu.Unlock()
			continue
		}
		d.advanceInactiveAwaitingLocked(candidate.threadID, thread, now, status)
		changed = true
		d.mu.Unlock()
	}
	return changed
}

func retryThreadIsActive(status string) bool {
	switch status {
	case "active", "running", "in_progress":
		return true
	default:
		return false
	}
}

func (d *daemon) advanceInactiveAwaitingLocked(threadID string, thread ThreadState, now time.Time, status string) {
	awaiting := thread.Awaiting
	if awaiting == nil {
		return
	}
	nextAttempt := awaiting.Attempt + 1
	nextConsecutive := awaiting.ConsecutiveRetry + 1
	if thread.CurrentTurnProgress {
		nextConsecutive = 1
	}
	if nextAttempt > awaiting.MaxAttempts || nextConsecutive > awaiting.MaxConsecutive {
		completedAttempts := completedRetryCount(nextAttempt, awaiting.MaxAttempts)
		completedConsecutive := completedRetryCount(nextConsecutive, awaiting.MaxConsecutive)
		reason := retryStopReason(nextAttempt, awaiting.MaxAttempts, nextConsecutive, awaiting.MaxConsecutive)
		if awaiting.Class == classEmptyResponse && thread.GoalStatus == "active" {
			reason = goalEmptyResponseStopReason
		}
		thread.Pending = nil
		thread.Awaiting = nil
		thread.RecoveryAttempts = completedAttempts
		thread.ConsecutiveRetries = completedConsecutive
		thread.CurrentTurnProgress = false
		historicalSince := awaiting.StartedAt
		if historicalSince.IsZero() {
			historicalSince = awaiting.DispatchStartedAt
		}
		thread.Stopped = &StoppedRetry{
			EventKey: awaiting.EventKey, FailedTurnID: awaiting.FailedTurnID, FailedAt: awaiting.FailedAt,
			OriginTurnStartedAt: awaiting.OriginTurnStartedAt,
			Class:               awaiting.Class, StoppedAt: now, CodexHome: awaiting.CodexHome, RolloutPath: awaiting.RolloutPath,
			Attempts: completedAttempts, MaxAttempts: awaiting.MaxAttempts,
			ConsecutiveRetries: completedConsecutive, MaxConsecutive: awaiting.MaxConsecutive, Reason: reason,
			Historical: !historicalSince.IsZero() && now.Sub(historicalSince) > stoppedRetryDisplayWindow,
		}
		if reason == goalEmptyResponseStopReason {
			thread.GoalStop = &GoalStopRequest{EventKey: awaiting.EventKey, Reason: reason, RequestedAt: now, DueAt: now}
		}
		d.state.Threads[threadID] = thread
		d.logger.Printf("retry lifecycle stopped thread=%s status=%s recovery_attempts=%d consecutive_retries=%d reason=%s", shortThreadID(threadID), status, completedAttempts, completedConsecutive, reason)
		return
	}

	thread.RecoveryAttempts = nextAttempt
	thread.ConsecutiveRetries = nextConsecutive
	thread.CurrentTurnProgress = false
	thread.Awaiting = nil
	thread.Stopped = nil
	thread.GoalStop = nil
	thread.Pending = &PendingRetry{
		EventKey: awaiting.EventKey, FailedTurnID: awaiting.FailedTurnID, FailedAt: awaiting.FailedAt,
		OriginTurnStartedAt: awaiting.OriginTurnStartedAt, Class: awaiting.Class,
		DueAt: now.Add(retryDelay(nextConsecutive, d.config)), WaitReason: awaiting.WaitReason,
		CodexHome: awaiting.CodexHome, RolloutPath: awaiting.RolloutPath,
		Attempt: nextAttempt, MaxAttempts: awaiting.MaxAttempts,
		ConsecutiveRetry: nextConsecutive, MaxConsecutive: awaiting.MaxConsecutive,
		DispatchFailures: awaiting.DispatchFailures, ParentNotified: awaiting.ParentNotified,
		GoalLimitRestart: awaiting.GoalLimitRestart,
	}
	d.state.Threads[threadID] = thread
	d.logger.Printf("retry lifecycle rescheduled thread=%s status=%s attempt=%d consecutive_retry=%d", shortThreadID(threadID), status, nextAttempt, nextConsecutive)
}

func (d *daemon) rescheduleAwaitingLocked(threadID string, thread ThreadState, now time.Time, reason string) {
	d.rescheduleAwaitingWithPolicyLocked(threadID, thread, now, reason, true)
}

func (d *daemon) rescheduleAwaitingWithPolicyLocked(threadID string, thread ThreadState, now time.Time, reason string, countFailure bool) {
	d.rescheduleAwaitingAtLocked(threadID, thread, now, reason, time.Time{}, countFailure)
}

func (d *daemon) rescheduleAwaitingAtLocked(threadID string, thread ThreadState, now time.Time, reason string, retryAt time.Time, countFailure bool) {
	awaiting := thread.Awaiting
	if awaiting == nil {
		return
	}
	dispatchFailures := awaiting.DispatchFailures
	if countFailure {
		dispatchFailures++
	}
	if controllerFailureNeedsAction(reason) || (countFailure && dispatchFailures >= d.config.ControllerFailureLimit) {
		d.stopAwaitingForControllerLocked(threadID, thread, now, reason)
		return
	}
	delayIndex := dispatchFailures
	if delayIndex < 1 {
		delayIndex = 1
	}
	delay := retryDelay(delayIndex, d.config)
	dueAt := now.Add(delay)
	if retryAt.After(now) {
		dueAt = retryAt.UTC()
		delay = dueAt.Sub(now)
	}
	waitReason := awaiting.WaitReason
	if quotaReason := quotaWaitReason(reason); quotaReason != "" {
		waitReason = quotaReason
	}
	thread.Awaiting = nil
	thread.Pending = &PendingRetry{
		EventKey:            awaiting.EventKey,
		FailedTurnID:        awaiting.FailedTurnID,
		FailedAt:            awaiting.FailedAt,
		OriginTurnStartedAt: awaiting.OriginTurnStartedAt,
		Class:               awaiting.Class,
		DueAt:               dueAt,
		WaitReason:          waitReason,
		CodexHome:           awaiting.CodexHome,
		RolloutPath:         awaiting.RolloutPath,
		Attempt:             awaiting.Attempt,
		MaxAttempts:         awaiting.MaxAttempts,
		ConsecutiveRetry:    awaiting.ConsecutiveRetry,
		MaxConsecutive:      awaiting.MaxConsecutive,
		DispatchFailures:    dispatchFailures,
		ParentNotified:      awaiting.ParentNotified,
		GoalLimitRestart:    awaiting.GoalLimitRestart,
	}
	d.state.Threads[threadID] = thread
	d.logger.Printf("retry rescheduled thread=%s reason=%s delay_seconds=%d", shortThreadID(threadID), reason, int(delay.Seconds()))
}

func (d *daemon) stopAwaitingForControllerLocked(threadID string, thread ThreadState, now time.Time, reason string) {
	awaiting := thread.Awaiting
	if awaiting == nil {
		return
	}
	thread.Pending = nil
	thread.Awaiting = nil
	thread.GoalStop = nil
	thread.RecoveryAttempts = completedRetryCount(awaiting.Attempt, awaiting.MaxAttempts)
	thread.ConsecutiveRetries = completedRetryCount(awaiting.ConsecutiveRetry, awaiting.MaxConsecutive)
	thread.Stopped = &StoppedRetry{
		EventKey: awaiting.EventKey, FailedTurnID: awaiting.FailedTurnID, FailedAt: awaiting.FailedAt,
		OriginTurnStartedAt: awaiting.OriginTurnStartedAt,
		Class:               awaiting.Class, StoppedAt: now, CodexHome: awaiting.CodexHome, RolloutPath: awaiting.RolloutPath,
		Attempts: thread.RecoveryAttempts, MaxAttempts: awaiting.MaxAttempts,
		ConsecutiveRetries: thread.ConsecutiveRetries, MaxConsecutive: awaiting.MaxConsecutive, Reason: reason,
	}
	d.state.Threads[threadID] = thread
	d.lastError = reason
	d.logger.Printf("retry stopped thread=%s reason=%s controller_failures=%d", shortThreadID(threadID), reason, awaiting.DispatchFailures+1)
}

func (d *daemon) stopPendingForControllerLocked(threadID string, thread ThreadState, now time.Time, reason string) {
	pending := thread.Pending
	if pending == nil {
		return
	}
	thread.Pending = nil
	thread.Awaiting = nil
	thread.GoalStop = nil
	thread.RecoveryAttempts = completedRetryCount(pending.Attempt, pending.MaxAttempts)
	thread.ConsecutiveRetries = completedRetryCount(pending.ConsecutiveRetry, pending.MaxConsecutive)
	thread.CurrentTurnProgress = false
	thread.Stopped = &StoppedRetry{
		EventKey: pending.EventKey, FailedTurnID: pending.FailedTurnID, FailedAt: pending.FailedAt,
		OriginTurnStartedAt: pending.OriginTurnStartedAt,
		Class:               pending.Class, StoppedAt: now, CodexHome: pending.CodexHome, RolloutPath: pending.RolloutPath,
		Attempts: thread.RecoveryAttempts, MaxAttempts: pending.MaxAttempts,
		ConsecutiveRetries: thread.ConsecutiveRetries, MaxConsecutive: pending.MaxConsecutive,
		Reason: reason,
	}
	d.state.Threads[threadID] = thread
	d.lastError = reason
	d.logger.Printf("retry stopped before dispatch thread=%s reason=%s", shortThreadID(threadID), reason)
}

func controllerFailureNeedsAction(reason string) bool {
	switch reason {
	case "codex_not_running", "codex_restart_required", "codex_home_not_shared", "shared_app_server_port_reserved", "shared_app_server_port_conflict", "shared_app_server_environment_conflict", "shared_app_server_ownership_unknown", "shared_app_server_disabled", "shared_app_server_config_invalid", "shared_app_server_migration_deferred", "codex_ipc_goal_control_unsupported":
		return true
	default:
		return false
	}
}

func isQuotaWaitReason(reason string) bool {
	return reason == "quota_limited" || reason == "quota_status_unavailable"
}

func quotaWaitReason(reason string) string {
	if isQuotaWaitReason(reason) {
		return reason
	}
	return ""
}

func controllerFailureNeedsFailOpen(reason string) bool {
	switch reason {
	case "shared_app_server_port_reserved", "shared_app_server_port_conflict", "shared_app_server_environment_conflict", "shared_app_server_ownership_unknown",
		"shared_app_server_config_invalid", "codex_background_channel_unavailable", "codex_background_dispatch_failed",
		"controller_timeout", "controller_invalid_result", "controller_unavailable":
		return true
	default:
		return false
	}
}

func (d *daemon) dispatchDueLocked(now time.Time) []RetryJob {
	available := d.config.MaxParallelRetries - len(d.active)
	if available <= 0 {
		return nil
	}
	jobs := make([]RetryJob, 0, available)
	type goalStopCandidate struct {
		threadID string
		request  GoalStopRequest
	}
	goalStops := make([]goalStopCandidate, 0)
	for threadID, thread := range d.state.Threads {
		if thread.GoalStop == nil || thread.GoalStop.DueAt.After(now) {
			continue
		}
		if _, active := d.active[threadID]; active {
			continue
		}
		goalStops = append(goalStops, goalStopCandidate{threadID: threadID, request: *thread.GoalStop})
	}
	sort.Slice(goalStops, func(i, j int) bool { return goalStops[i].request.DueAt.Before(goalStops[j].request.DueAt) })
	for _, item := range goalStops {
		if available == 0 {
			break
		}
		thread := d.state.Threads[item.threadID]
		job := RetryJob{Kind: jobGoalBlock, ThreadID: item.threadID, EventKey: item.request.EventKey,
			DispatchFailures: item.request.DispatchFailures}
		if thread.Stopped != nil {
			job.Class = thread.Stopped.Class
			job.Attempt = thread.Stopped.Attempts
			job.CodexHome = thread.Stopped.CodexHome
			job.RolloutPath = thread.Stopped.RolloutPath
		}
		d.active[item.threadID] = job
		jobs = append(jobs, job)
		available--
	}
	if available == 0 {
		return jobs
	}
	if d.paused {
		return jobs
	}
	primaryRecoveryUnavailable := !d.config.SharedAppServerEnabled || d.controllerState == "shared_app_server_disabled" ||
		d.controllerState == "shared_app_server_config_invalid"
	if primaryRecoveryUnavailable && !d.config.CLIResumeFallbackEnabled {
		// The fail-open mode deliberately has no recovery transport. Do not
		// promote a due item to AwaitingRetry: doing so looks like a retry was
		// started and then failed, even though Codex never received a request.
		stopReason := "shared_app_server_disabled"
		if controllerFailureNeedsFailOpen(d.controllerState) {
			stopReason = d.controllerState
		}
		for threadID, thread := range d.state.Threads {
			if thread.Pending == nil || thread.Pending.DueAt.After(now) || thread.Awaiting != nil {
				continue
			}
			d.stopPendingForControllerLocked(threadID, thread, now, stopReason)
		}
		return jobs
	}
	type candidate struct {
		threadID string
		pending  PendingRetry
	}
	candidates := make([]candidate, 0)
	plan, planErr := loadTaskPlan(filepath.Join(d.dataDir, "task-plan.json"))
	if planErr != nil {
		// A damaged plan must never widen an explicit user selection back to
		// the legacy auto-resume-all behaviour.
		if !d.taskPlanErrorLogged && d.logger != nil {
			d.logger.Printf("task plan unavailable; quota dispatch paused category=task_plan")
		}
		d.taskPlanErrorLogged = true
		d.lastError = "task_plan_unavailable"
	} else {
		d.taskPlanErrorLogged = false
		d.adoptSequentialRunningLocked(plan)
		d.reconcileQuotaOwnerLocked(plan, now)
	}
	for threadID, thread := range d.state.Threads {
		if thread.GoalHeld && !goalLimitRestartPending(thread) && !heldConversationStillAllowed(thread, thread.GoalStatus, thread.GoalUpdatedAt) {
			if thread.Pending != nil || thread.Awaiting != nil {
				thread.Pending = nil
				thread.Awaiting = nil
				thread.RecoveryAttempts = 0
				thread.ConsecutiveRetries = 0
				thread.CurrentTurnProgress = false
				thread.Stopped = nil
				d.state.Threads[threadID] = thread
			}
			continue
		}
		if thread.Pending == nil || thread.Pending.DueAt.After(now) || thread.Awaiting != nil {
			continue
		}
		if thread.Pending.Class == classRateLimit && (planErr != nil || (plan.Configured && !plan.IsSelected(threadID))) {
			continue
		}
		if thread.Pending.Class == classRateLimit && thread.ObservedTurnRunning {
			// An observed active turn must finish or abort before this exact
			// thread can receive another automatic continuation.
			continue
		}
		if _, active := d.active[threadID]; active {
			continue
		}
		candidates = append(candidates, candidate{threadID: threadID, pending: *thread.Pending})
	}
	if len(candidates) == 0 {
		return jobs
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].pending.Class == classRateLimit && candidates[j].pending.Class == classRateLimit && plan.Configured {
			left, right := plan.Order(candidates[i].threadID), plan.Order(candidates[j].threadID)
			if left != right {
				return left < right
			}
		}
		if candidates[i].pending.DueAt.Equal(candidates[j].pending.DueAt) {
			return candidates[i].threadID < candidates[j].threadID
		}
		return candidates[i].pending.DueAt.Before(candidates[j].pending.DueAt)
	})
	resumeInterval := time.Duration(d.config.ResumeIntervalSeconds) * time.Second
	if resumeInterval > 0 && !d.state.LastResumeDispatchAt.IsZero() && now.Before(d.state.LastResumeDispatchAt.Add(resumeInterval)) {
		return jobs
	}
	// Spacing limits only when requests start, not how long they may run. One
	// task can remain active while another starts on a later scheduler tick.
	if resumeInterval > 0 && available > 1 {
		available = 1
	}
	for _, item := range candidates {
		if available == 0 {
			break
		}
		if item.pending.Class == classRateLimit {
			if d.config.QuotaResumeMode == quotaModeSequentialCompletion &&
				d.state.QuotaSchedulerOwnerID != "" && d.state.QuotaSchedulerOwnerID != item.threadID {
				continue
			}
			if !d.quotaCapacityReadyLocked(now) {
				continue
			}
		}
		thread := d.state.Threads[item.threadID]
		thread.Pending = nil
		thread.LastAutoRetryAt = now
		thread.Awaiting = &AwaitingRetry{
			EventKey:            item.pending.EventKey,
			FailedTurnID:        item.pending.FailedTurnID,
			FailedAt:            item.pending.FailedAt,
			OriginTurnStartedAt: item.pending.OriginTurnStartedAt,
			Class:               item.pending.Class,
			Action:              actionDispatching,
			WaitReason:          item.pending.WaitReason,
			Attempt:             item.pending.Attempt,
			MaxAttempts:         item.pending.MaxAttempts,
			ConsecutiveRetry:    item.pending.ConsecutiveRetry,
			MaxConsecutive:      item.pending.MaxConsecutive,
			DispatchFailures:    item.pending.DispatchFailures,
			ParentNotified:      item.pending.ParentNotified,
			GoalLimitRestart:    item.pending.GoalLimitRestart,
			DispatchStartedAt:   now,
			StartDeadline:       now.Add(time.Duration(d.config.StartAckTimeoutSeconds) * time.Second),
			CodexHome:           item.pending.CodexHome,
			RolloutPath:         item.pending.RolloutPath,
		}
		d.state.Threads[item.threadID] = thread
		job := RetryJob{
			Kind:                jobRecovery,
			ThreadID:            item.threadID,
			FailedTurnID:        item.pending.FailedTurnID,
			FailedAt:            item.pending.FailedAt,
			OriginTurnStartedAt: item.pending.OriginTurnStartedAt,
			EventKey:            item.pending.EventKey,
			Class:               item.pending.Class,
			CodexHome:           item.pending.CodexHome,
			RolloutPath:         item.pending.RolloutPath,
			Attempt:             item.pending.Attempt,
			MaxAttempts:         item.pending.MaxAttempts,
			ConsecutiveRetry:    item.pending.ConsecutiveRetry,
			MaxConsecutive:      item.pending.MaxConsecutive,
			DispatchFailures:    item.pending.DispatchFailures,
			ParentNotified:      item.pending.ParentNotified,
			GoalLimitRestart:    item.pending.GoalLimitRestart,
			RecoveryEventID:     recoveryEventID(item.threadID, item.pending.EventKey),
			WaitReason:          item.pending.WaitReason,
		}
		d.active[item.threadID] = job
		jobs = append(jobs, job)
		d.state.LastResumeDispatchAt = now
		if item.pending.Class == classRateLimit {
			d.quotaDispatchStartedLocked(item.threadID)
		}
		available--
	}
	return jobs
}

func (d *daemon) runJob(ctx context.Context, job RetryJob) {
	defer d.wg.Done()
	if job.Kind == jobGoalBlock {
		d.runGoalBlockJob(ctx, job)
		return
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.mu.Lock()
	thread := d.state.Threads[job.ThreadID]
	if thread.Awaiting == nil || thread.Awaiting.EventKey != job.EventKey ||
		thread.Awaiting.Attempt != job.Attempt ||
		(thread.GoalHeld && !job.GoalLimitRestart && !heldConversationStillAllowed(thread, thread.GoalStatus, thread.GoalUpdatedAt)) {
		delete(d.active, job.ThreadID)
		d.mu.Unlock()
		d.logger.Printf("retry action ignored thread=%s reason=stale_job", shortThreadID(job.ThreadID))
		return
	}
	d.activeCtx[job.ThreadID] = cancel
	d.mu.Unlock()
	d.logger.Printf("retry action started thread=%s category=%s attempt=%d", shortThreadID(job.ThreadID), job.Class, job.Attempt)
	result, err := d.runner.Resume(jobCtx, job)
	finishedAt := time.Now().UTC()

	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.active, job.ThreadID)
	delete(d.activeCtx, job.ThreadID)
	thread = d.state.Threads[job.ThreadID]
	if thread.Awaiting == nil || thread.Awaiting.EventKey != job.EventKey || thread.Awaiting.Attempt != job.Attempt {
		d.logger.Printf("retry action ignored thread=%s reason=stale_result", shortThreadID(job.ThreadID))
		_ = d.writeStatusLocked(true, len(discoverSessionRoots(d.config)))
		return
	}

	if result.ParentNotified {
		thread.Awaiting.ParentNotified = true
	}
	if result.Outcome == outcomeRetryLater && isQuotaWaitReason(result.Reason) && err == nil {
		d.controllerState = "ready"
		d.rescheduleAwaitingAtLocked(job.ThreadID, thread, finishedAt, result.Reason, result.RetryAt, false)
	} else if err != nil || result.Outcome == outcomeRetryLater {
		reason := controllerFailureReason(result, err)
		d.controllerState = reason
		d.rescheduleAwaitingWithPolicyLocked(job.ThreadID, thread, finishedAt, reason, true)
	} else if result.Outcome == outcomeUserActive {
		// User or turn activity is temporary. A matching task_started event will
		// still cancel or acknowledge this retry on the next scan; until then the
		// failed task remains independently queued.
		d.controllerState = "ready"
		thread.Awaiting.DispatchFailures = 0
		d.rescheduleAwaitingWithPolicyLocked(job.ThreadID, thread, finishedAt, controllerFailureReason(result, nil), false)
	} else if result.Outcome == outcomeNotApplicable {
		d.controllerState = "ready"
		thread.LastTerminalNoRetryAt = finishedAt
		thread.RecoveryAttempts = 0
		thread.ConsecutiveRetries = 0
		thread.CurrentTurnProgress = false
		thread.Pending = nil
		thread.Awaiting = nil
		thread.Stopped = nil
		thread.GoalStop = nil
		if status, held := goalHoldStatusForReason(result.Reason); held {
			thread.GoalHeld = true
			if status != "" {
				thread.GoalStatus = status
			}
			if status == "completed" {
				// The controller observed the official goal result directly.
				thread.GoalObservedAt = finishedAt
			}
		}
		d.state.Threads[job.ThreadID] = thread
		d.logger.Printf("retry skipped thread=%s reason=%s", shortThreadID(job.ThreadID), result.Reason)
	} else {
		d.controllerState = "ready"
		if isQuotaWaitReason(job.WaitReason) {
			// A reset deadline is only a wake-up time. Reaching this branch means
			// a fresh quota read allowed the dispatch and Codex accepted it, so a
			// later quota cycle starts with a fresh recovery budget.
			thread.RecoveryAttempts = 1
			thread.ConsecutiveRetries = 1
			thread.RecoveryStartedAt = finishedAt
			thread.Awaiting.Attempt = 1
			thread.Awaiting.ConsecutiveRetry = 1
			thread.Awaiting.WaitReason = ""
		}
		thread.Awaiting.Action = result.Action
		thread.Awaiting.DispatchMode = ""
		if result.Reason == "cli_fallback_started" {
			thread.Awaiting.DispatchMode = "cli_fallback"
		}
		thread.Awaiting.DispatchFailures = 0
		thread.Awaiting.StartDeadline = finishedAt.Add(time.Duration(d.config.StartAckTimeoutSeconds) * time.Second)
		d.state.Threads[job.ThreadID] = thread
		d.logger.Printf("retry action accepted thread=%s action=%s attempt=%d", shortThreadID(job.ThreadID), result.Action, job.Attempt)
	}
	if stateErr := d.persistStateLocked(); stateErr != nil {
		d.logger.Printf("state save deferred category=state_write")
	}
	_ = d.writeStatusLocked(true, len(discoverSessionRoots(d.config)))
}

func (d *daemon) waitForJobs() {
	d.wg.Wait()
}

func (d *daemon) writeStatus(running bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeStatusLocked(running, len(discoverSessionRoots(d.config)))
}

func (d *daemon) writeStatusLocked(running bool, rootCount int) error {
	pending := 0
	active := 0
	for _, thread := range d.state.Threads {
		if thread.Pending != nil {
			pending++
		}
		if thread.Awaiting != nil {
			active++
		}
	}
	if !running {
		pending = 0
		active = 0
	}
	capability := capabilityForControllerState(d.controllerState, d.config.SharedAppServerEnabled, d.config.CLIResumeFallbackEnabled)
	if !running {
		capability = capabilityForControllerState("codex_not_running", false, d.config.CLIResumeFallbackEnabled)
	}
	status := StatusSnapshot{
		BuildSourceHash:                     buildSourceHash,
		DesktopLaunchMode:                   "process_scoped",
		DesktopTransport:                    capability.Transport,
		RecoveryMode:                        capability.RecoveryMode,
		AutomaticRecoverySupported:          capability.Automatic,
		RecoveryCapabilityReason:            capability.Reason,
		Version:                             appVersion,
		Build:                               appBuild,
		Running:                             running,
		PID:                                 os.Getpid(),
		StartedAt:                           d.startedAt,
		LastHeartbeatAt:                     d.lastHeartbeat,
		LastScanAt:                          d.lastScan,
		WatchedRoots:                        rootCount,
		PendingRetries:                      pending,
		ActiveRetries:                       active,
		Paused:                              d.paused,
		MonitoringPaused:                    d.monitoringPaused,
		SharedAppServerEnabled:              d.config.SharedAppServerEnabled,
		SharedAppServerRequested:            d.config.SharedAppServerRequested,
		ControllerState:                     d.controllerState,
		LastError:                           d.lastError,
		MemoryUsageMB:                       memoryBytesToMB(d.memoryUsageBytes),
		MemoryLimitMB:                       d.config.MemoryLimitMB,
		MemoryGuardTriggered:                d.memoryGuardTriggered,
		SharedAppServerMemoryUsageMB:        memoryBytesToMB(d.sharedAppServerMemoryBytes),
		SharedAppServerMemoryLimitMB:        d.config.SharedAppServerMemoryLimitMB,
		SharedAppServerMemoryGuardTriggered: d.sharedAppServerMemoryGuardTriggered,
		RetrySafetyWarning:                  d.config.retrySafetyWarning(),
		LogPath:                             d.logger.path,
	}
	// Publish a changed operational state immediately, but do not replace the
	// status file every scheduler tick solely to advance its heartbeat.
	comparable := status
	comparable.LastHeartbeatAt = time.Time{}
	// Memory samples are telemetry, not a state transition. They are included
	// in the next bounded heartbeat write rather than forcing disk churn.
	comparable.MemoryUsageMB = 0
	comparable.SharedAppServerMemoryUsageMB = 0
	if !d.statusWriteDeferred && !d.lastStatusPersist.IsZero() &&
		status.LastHeartbeatAt.Sub(d.lastStatusPersist) >= 0 &&
		status.LastHeartbeatAt.Sub(d.lastStatusPersist) < statusHeartbeatInterval &&
		comparable == d.lastPublishedStatus {
		return nil
	}
	writer := d.writeStatusFile
	if writer == nil {
		writer = writeJSONAtomic
	}
	if err := writer(d.statusPath, status); err != nil {
		d.statusWriteDeferred = true
		return err
	}
	d.lastStatusPersist = status.LastHeartbeatAt
	d.lastPublishedStatus = comparable
	if d.statusWriteDeferred {
		d.statusWriteDeferred = false
		d.logger.Printf("status save recovered category=status_write")
	}
	return nil
}
