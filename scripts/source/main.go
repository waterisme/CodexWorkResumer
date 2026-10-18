package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

type localSettingsPayload struct {
	RetrySettings
	Paused                 bool  `json:"paused"`
	MonitoringPaused       *bool `json:"monitoring_paused,omitempty"`
	SharedAppServerEnabled *bool `json:"shared_app_server_enabled,omitempty"`
}

// startupFailOpenResult makes the process-boundary decision explicit. A
// persisted disabled setting is sufficient to keep the worker on Codex's
// official backend while cleanup is retried; if that setting cannot be read
// back, the worker must stop before constructing a recovery controller.
type startupFailOpenResult struct {
	Config      Config
	Reason      string
	CleanupErr  error
	CanContinue bool
}

func completeStartupFailOpen(ctx context.Context, configPath, dataDir string, config Config) startupFailOpenResult {
	result := startupFailOpenResult{Config: config}
	updated, failOpenErr := failOpenSharedAppServer(ctx, configPath, dataDir, config)
	result.Config = updated
	if failOpenErr == nil {
		result.CanContinue = true
		return result
	}
	if errors.Is(failOpenErr, errSharedAppServerEnvironmentConflict) {
		// The endpoint is still present but its ownership cannot be proven. Do
		// not continue and report a false official-backend recovery; a manual
		// cleanup decision is required before the worker can safely proceed.
		result.CleanupErr = failOpenErr
		return result
	}
	persisted, readErr := loadOrCreateConfig(configPath)
	if readErr != nil || persisted.SharedAppServerEnabled {
		result.CleanupErr = errors.Join(failOpenErr, readErr)
		return result
	}
	// The safety preference is durable. Cleanup failures are recoverable by the
	// disabled-mode reconciler and must not prevent official-backend operation.
	result.Config = persisted
	result.CleanupErr = failOpenErr
	result.CanContinue = true
	return result
}

// prepareSharedBackendAtStartup is the single startup gate before the
// controller can call Ensure. A failed owned backend is disabled first;
// first-time enablement with no stale endpoint evidence remains eligible for
// normal preparation.
func prepareSharedBackendAtStartup(ctx context.Context, configPath, dataDir string, config Config) startupFailOpenResult {
	result := startupFailOpenResult{Config: config, CanContinue: true}
	if !config.SharedAppServerEnabled {
		return result
	}
	manager := newSharedServerManager(config, dataDir, nil)
	if err := manager.checkSharedServerStartupState(ctx); err == nil {
		return result
	} else {
		result.Reason = controllerFailureReason(DispatchResult{}, err)
	}
	_ = recordSharedUnavailable(dataDir, result.Reason)
	failOpenResult := completeStartupFailOpen(ctx, configPath, dataDir, config)
	result.Config = failOpenResult.Config
	result.CleanupErr = failOpenResult.CleanupErr
	result.CanContinue = failOpenResult.CanContinue
	return result
}

var (
	enableSharedAppServerAtStartup  = enableSharedAppServer
	disableSharedAppServerAtStartup = disableSharedAppServer
)

// reconcileSharedBackendPreferenceAtStartup applies the saved user choice, not
// merely the last effective runtime state. A temporary health failure leaves
// Requested=true and Enabled=false; the next process start must make one safe,
// bounded attempt to enable it again. Disabling is persisted before cleanup so
// a deferred shutdown can never reactivate the route on a later start.
func reconcileSharedBackendPreferenceAtStartup(ctx context.Context, configPath, dataDir string, config Config) (Config, error) {
	desired := config.SharedAppServerRequested
	if desired == config.SharedAppServerEnabled {
		return config, nil
	}
	if desired {
		prepared, err := enableSharedAppServerAtStartup(ctx, dataDir, config)
		if err != nil {
			return config, err
		}
		persisted, err := updateConfigFile(configPath, func(current *Config) error {
			if !current.SharedAppServerRequested {
				return fmt.Errorf("shared app-server enable cancelled by user")
			}
			current.SharedAppServerEnabled = true
			current.SharedAppServerPort = prepared.SharedAppServerPort
			return nil
		})
		if err != nil {
			_ = disableSharedAppServerAtStartup(context.Background(), dataDir, prepared)
			return config, err
		}
		for _, marker := range []string{"shared-availability.json", sharedFailOpenMarkerName} {
			if err := os.Remove(filepath.Join(dataDir, marker)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return persisted, err
			}
		}
		return persisted, nil
	}

	persisted, err := updateConfigFile(configPath, func(current *Config) error {
		current.SharedAppServerRequested = false
		current.SharedAppServerEnabled = false
		return nil
	})
	if err != nil {
		return config, err
	}
	if err := disableSharedAppServerAtStartup(ctx, dataDir, persisted); err != nil {
		return persisted, err
	}
	return persisted, nil
}

const (
	localSettingsExitFailure      = 1
	localSettingsExitPortReserved = 2
	localSettingsExitPortConflict = 3
)

func main() {
	arguments := os.Args[1:]
	mode := "run"
	if len(arguments) > 0 && (arguments[0] == "run" || arguments[0] == "supervise" || arguments[0] == "mcp" || arguments[0] == "save-settings" || arguments[0] == "control" || arguments[0] == "diagnostics") {
		mode = arguments[0]
		arguments = arguments[1:]
	}
	flags := flag.NewFlagSet("codex-auto-retry", flag.ContinueOnError)
	dataDirFlag := flags.String("data-dir", "", "runtime data directory")
	settingsFileFlag := flags.String("settings-file", "", "settings payload path")
	actionFlag := flags.String("action", "", "retry control action")
	threadIDFlag := flags.String("thread-id", "", "Codex task identifier")
	noTrayFlag := flags.Bool("no-tray", false, "disable the Windows notification-area icon")
	supervisedFlag := flags.Bool("supervised", false, "run as a worker owned by the supervisor")
	_ = flags.Parse(arguments)

	dataDir := *dataDirFlag
	if dataDir == "" {
		dataDir = os.Getenv("CODEX_AUTO_RETRY_DATA_DIR")
	}
	if dataDir == "" {
		executable, err := os.Executable()
		if err != nil {
			return
		}
		dataDir = filepath.Dir(executable)
	}
	dataDir = expandPath(dataDir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return
	}
	if mode == "save-settings" {
		if err := saveLocalSettings(dataDir, *settingsFileFlag); err != nil {
			os.Exit(localSettingsExitCode(err))
		}
		return
	}
	if mode == "control" {
		if err := runLocalControl(dataDir, *actionFlag, *threadIDFlag); err != nil {
			os.Exit(1)
		}
		return
	}
	if mode == "diagnostics" {
		if err := runLocalDiagnostics(dataDir, *actionFlag, time.Now().UTC()); err != nil {
			os.Exit(1)
		}
		return
	}
	if mode == "mcp" {
		if err := runManagementMCP(dataDir); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "CodexWorkResumer MCP server stopped:", err)
		}
		return
	}
	if mode == "supervise" {
		if err := runSupervisor(dataDir, *noTrayFlag); err != nil {
			os.Exit(1)
		}
		return
	}

	lock, err := acquireInstanceLock(filepath.Join(dataDir, "daemon.lock"))
	if err != nil {
		return
	}
	defer lock.Close()

	logger, err := newSafeLogger(filepath.Join(dataDir, "logs", "daemon.log"))
	if err != nil {
		return
	}
	defer logger.Close()
	logger.ConfigureDebug(filepath.Join(dataDir, diagnosticSettingsFileName))
	logger.Printf("watchdog starting version=%s", appVersion)

	configPath := filepath.Join(dataDir, "config.json")
	config, err := loadOrCreateConfig(configPath)
	if err != nil {
		logger.Printf("startup failed category=config")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if cleanupErr := cleanupSharedBackend(cleanupCtx, dataDir); cleanupErr != nil {
			logger.Printf("shared app-server startup cleanup failed category=config_fallback")
		}
		cleanupCancel()
		return
	}
	if warning := config.retrySafetyWarning(); warning != "" {
		logger.Printf("retry policy warning category=aggressive_limits recovery=%d consecutive=%d", config.MaxRecoveryAttempts, config.MaxConsecutiveRetries)
	}
	// A previous fail-open may have been interrupted while config.json was
	// locked. Resolve that durable marker before any readiness or Ensure call so
	// an old enabled setting can never recreate a dead endpoint on this startup.
	startupFailOpenReason := ""
	if _, markerErr := os.Stat(filepath.Join(dataDir, sharedFailOpenMarkerName)); markerErr == nil {
		startupFailOpenReason = "shared_app_server_startup_recovery"
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		failOpenResult := completeStartupFailOpen(cleanupCtx, configPath, dataDir, config)
		cleanupCancel()
		config = failOpenResult.Config
		if !failOpenResult.CanContinue {
			logger.Printf("shared app-server startup fail-open remains incomplete category=%s", startupFailOpenReason)
			return
		}
		if failOpenResult.CleanupErr != nil {
			logger.Printf("shared app-server startup cleanup remains deferred category=%s", startupFailOpenReason)
		} else {
			_ = os.Remove(filepath.Join(dataDir, sharedFailOpenMarkerName))
		}
	}
	if _, markerErr := os.Stat(filepath.Join(dataDir, sharedFailOpenMarkerName)); errors.Is(markerErr, os.ErrNotExist) &&
		config.SharedAppServerRequested != config.SharedAppServerEnabled {
		desired := config.SharedAppServerRequested
		preferenceCtx, preferenceCancel := context.WithTimeout(context.Background(), 30*time.Second)
		reconciled, reconcileErr := reconcileSharedBackendPreferenceAtStartup(preferenceCtx, configPath, dataDir, config)
		preferenceCancel()
		config = reconciled
		if reconcileErr != nil {
			logger.Printf("saved shared app-server preference could not be applied at startup enabled=%t category=shared_preference", desired)
		} else {
			logger.Printf("saved shared app-server preference applied at startup enabled=%t", desired)
		}
	}
	manager := newSharedServerManager(config, dataDir, logger)
	if !config.SharedAppServerEnabled {
		// Fail-open startup cleans only plugin-owned artifacts. If Codex is still
		// using a live shared server, cleanup is deferred and retried by the
		// worker after Desktop exits instead of tearing down its route here.
		if cleanupErr := manager.CleanupIfUnused(context.Background()); cleanupErr != nil && !errors.Is(cleanupErr, errSharedServerMigrationDeferred) {
			logger.Printf("shared app-server cleanup failed category=boundary")
		}
	}
	// A healthy owned endpoint is intentionally left in place across worker
	// restarts. Removing it first creates a needless disconnect window for the
	// visible Codex Desktop; Prepare adopts or repairs it after the worker starts.
	var prepareErr error
	if config.SharedAppServerEnabled {
		startupCtx, startupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		startupResult := prepareSharedBackendAtStartup(startupCtx, configPath, dataDir, config)
		startupCancel()
		if startupResult.Reason != "" {
			startupFailOpenReason = startupResult.Reason
			config = startupResult.Config
			if !startupResult.CanContinue {
				logger.Printf("shared app-server startup fail-open remains incomplete category=%s", startupFailOpenReason)
				return
			}
			if startupResult.CleanupErr != nil {
				logger.Printf("shared app-server startup fail-open cleanup failed category=%s", startupFailOpenReason)
			}
			logger.Printf("shared app-server disabled after startup health failure category=%s", startupFailOpenReason)
		}
	}
	runner := newAppResumeRunner(config, dataDir, logger)
	if config.SharedAppServerEnabled || len(discoverSessionRoots(config)) > 0 {
		prepareCtx, prepareCancel := context.WithTimeout(context.Background(), 15*time.Second)
		prepareErr = runner.Prepare(prepareCtx)
		prepareCancel()
	}
	prepareReason := controllerFailureReason(DispatchResult{}, prepareErr)
	if config.SharedAppServerEnabled && prepareErr != nil && controllerFailureNeedsFailOpen(prepareReason) {
		startupFailOpenReason = prepareReason
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		failOpenResult := completeStartupFailOpen(cleanupCtx, configPath, dataDir, config)
		cleanupCancel()
		config = failOpenResult.Config
		if !failOpenResult.CanContinue {
			logger.Printf("shared app-server fail-open remains incomplete category=%s", prepareReason)
			return
		}
		if failOpenResult.CleanupErr != nil {
			logger.Printf("shared app-server fail-open cleanup failed category=%s", prepareReason)
		}
		logger.Printf("shared app-server disabled after startup failure category=%s", prepareReason)
	}
	daemon, err := newDaemon(config, dataDir, logger, runner)
	if err != nil {
		logger.Printf("startup failed category=state")
		return
	}
	if !config.SharedAppServerEnabled {
		if startupFailOpenReason != "" {
			daemon.controllerState = startupFailOpenReason
			daemon.lastError = startupFailOpenReason
			logger.Printf("shared app-server disabled; Codex remains on its official backend category=%s", startupFailOpenReason)
		} else {
			daemon.controllerState = "shared_app_server_disabled"
			logger.Printf("shared app-server disabled; Codex remains on its official backend")
		}
	} else if prepareErr != nil {
		daemon.lastError = prepareReason
		daemon.controllerState = daemon.lastError
		logger.Printf("controller preparation failed category=%s", daemon.lastError)
	} else {
		daemon.controllerState = "ready"
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	memoryTriggered := make(chan memorySample, 1)
	memoryGuard := newProcessMemoryGuard(config.MemoryLimitMB, memoryCheckInterval, nil,
		daemon.recordMemorySample,
		func(sample memorySample) {
			daemon.handleMemoryLimit(sample)
			select {
			case memoryTriggered <- sample:
			default:
			}
			cancel()
		},
	)
	go memoryGuard.Run(ctx)
	trayDone := make(chan struct{})
	if *noTrayFlag {
		close(trayDone)
	} else {
		go func() {
			defer close(trayDone)
			if trayErr := runTray(ctx, cancel, dataDir, logger); trayErr != nil {
				logger.Printf("tray stopped category=tray_error")
			}
		}()
	}
	_ = daemon.writeStatus(true)
	err = daemon.run(ctx)
	cancel()
	daemon.waitForJobs()
	select {
	case <-trayDone:
	case <-time.After(3 * time.Second):
	}
	if err != nil && err != errStopRequested {
		logger.Printf("watchdog stopped category=runtime_error")
	}
	_ = daemon.writeStatus(false)
	select {
	case sample := <-memoryTriggered:
		showMemoryLimitAlert(sample, config.MemoryLimitMB)
	default:
	}
	if *supervisedFlag && (err == nil || errors.Is(err, errStopRequested)) {
		// A clean worker shutdown is intentional. Tell the supervisor not to
		// bring it back; crashes and startup errors remain restartable.
		_ = os.WriteFile(filepath.Join(dataDir, "supervisor.stop"), []byte("stop\n"), 0o600)
	}
	logger.Printf("watchdog stopped")
}

func localSettingsExitCode(err error) int {
	switch {
	case errors.Is(err, errSharedServerPortReserved):
		return localSettingsExitPortReserved
	case errors.Is(err, errSharedServerPortConflict):
		return localSettingsExitPortConflict
	default:
		return localSettingsExitFailure
	}
}

func saveLocalSettings(dataDir, settingsPath string) error {
	if settingsPath == "" {
		settingsPath = os.Getenv("CODEX_AUTO_RETRY_SETTINGS_FILE")
	}
	if settingsPath == "" {
		return fmt.Errorf("settings file is required")
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return err
	}
	var payload localSettingsPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	service := newManagementService(dataDir)
	now := time.Now().UTC()
	if payload.SharedAppServerEnabled != nil {
		if _, err := service.setSharedAppServerEnabled(*payload.SharedAppServerEnabled, now); err != nil {
			return err
		}
	}
	return service.setLocalSettings(payload.RetrySettings, payload.Paused, payload.MonitoringPaused, now)
}

func runLocalControl(dataDir, action, threadID string) error {
	if action == "" {
		action = os.Getenv("CODEX_AUTO_RETRY_ACTION")
	}
	if threadID == "" {
		threadID = os.Getenv("CODEX_AUTO_RETRY_THREAD_ID")
	}
	service := newManagementService(dataDir)
	now := time.Now().UTC()
	switch action {
	case localSaveTaskPlanAction:
		_, err := saveTaskPlanJSON(filepath.Join(dataDir, "task-plan.json"), os.Getenv("CODEX_AUTO_RETRY_TASK_PLAN"), now)
		return err
	case "set_paused":
		value := os.Getenv("CODEX_AUTO_RETRY_PAUSED")
		if value != "true" && value != "false" {
			return fmt.Errorf("CODEX_AUTO_RETRY_PAUSED must be true or false")
		}
		_, err := saveControlState(filepath.Join(dataDir, "control.json"), value == "true", now)
		return err
	case "set_scheduler_settings":
		mode := os.Getenv("CODEX_AUTO_RETRY_QUOTA_RESUME_MODE")
		interval, err := strconv.Atoi(os.Getenv("CODEX_AUTO_RETRY_RESUME_INTERVAL_SECONDS"))
		if err != nil {
			return fmt.Errorf("invalid scheduler resume interval: %w", err)
		}
		parallel, err := strconv.Atoi(os.Getenv("CODEX_AUTO_RETRY_MAX_PARALLEL_SESSIONS"))
		if err != nil {
			return fmt.Errorf("invalid scheduler parallel limit: %w", err)
		}
		refresh, err := strconv.Atoi(os.Getenv("CODEX_AUTO_RETRY_USAGE_REFRESH_MINUTES"))
		if err != nil {
			return fmt.Errorf("invalid scheduler quota refresh: %w", err)
		}
		_, err = updateConfigFile(filepath.Join(dataDir, "config.json"), func(config *Config) error {
			config.QuotaResumeMode = mode
			config.ResumeIntervalSeconds = interval
			config.MaxParallelSessions = parallel
			config.UsageRefreshMinutes = refresh
			return nil
		})
		return err
	case string(commandConfirmTaskComplete):
		_, err := queueControlCommand(filepath.Join(dataDir, "commands"), commandConfirmTaskComplete, threadID, now)
		return err
	case string(commandRetryNow):
		_, err := service.retryNow(threadID, now)
		return err
	case string(commandCancelRetry):
		_, err := service.cancelRetry(threadID, now)
		return err
	case string(commandRestartRetry):
		_, err := service.restartRetry(threadID, now)
		return err
	case string(commandResumeTask):
		rolloutPath := os.Getenv("CODEX_AUTO_RETRY_ROLLOUT_PATH")
		class := FailureClass(os.Getenv("CODEX_AUTO_RETRY_FAILURE_CLASS"))
		_, err := service.resumeTask(threadID, rolloutPath, class, now)
		return err
	default:
		return fmt.Errorf("unsupported control action")
	}
}
