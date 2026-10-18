//go:build windows

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed ui/settings.ps1
var settingsPowerShell string

const (
	wmDestroy          = 0x0002
	wmCommand          = 0x0111
	wmTimer            = 0x0113
	wmClose            = 0x0010
	wmNull             = 0x0000
	wmLButtonUp        = 0x0202
	wmLButtonDblClk    = 0x0203
	wmRButtonUp        = 0x0205
	wmAppTray          = 0x8001
	taskbarCreatedName = "TaskbarCreated"

	nimAdd     = 0
	nimModify  = 1
	nimDelete  = 2
	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10

	mfString       = 0x0000
	mfGrayed       = 0x0001
	mfSeparator    = 0x0800
	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	menuOpenSettings       = 1001
	menuTogglePause        = 1002
	menuExit               = 1003
	menuToggleMonitor      = 1005
	trayTimerID            = 1
	traySingleClickTimerID = 2
	traySnapshotInterval   = 5 * time.Second
)

type trayInputStamp struct {
	modTime int64
	size    int64
	exists  bool
}

type trayPoint struct{ X, Y int32 }
type trayMessage struct {
	HWnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       trayPoint
	LPrivate uint32
}
type windowClass struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSmall  uintptr
}
type notifyIconData struct {
	Size             uint32
	HWnd             uintptr
	ID               uint32
	Flags            uint32
	CallbackMessage  uint32
	Icon             uintptr
	Tip              [128]uint16
	State            uint32
	StateMask        uint32
	Info             [256]uint16
	TimeoutOrVersion uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	GUID             windows.GUID
	BalloonIcon      uintptr
}

type trayIconInfo struct {
	IsIcon   int32
	XHotspot uint32
	YHotspot uint32
	Mask     uintptr
	Color    uintptr
}

type trayClickState struct {
	pendingSingle      bool
	suppressNextLeftUp bool
}

func (s *trayClickState) onLeftButtonUp() bool {
	if s.suppressNextLeftUp {
		s.suppressNextLeftUp = false
		return false
	}
	s.pendingSingle = true
	return true
}

func (s *trayClickState) onDoubleClick() {
	s.pendingSingle = false
	s.suppressNextLeftUp = true
}

func (s *trayClickState) onSingleClickTimer() bool {
	s.suppressNextLeftUp = false
	if !s.pendingSingle {
		return false
	}
	s.pendingSingle = false
	return true
}

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	shell32                 = windows.NewLazySystemDLL("shell32.dll")
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procRegisterClassEx     = user32.NewProc("RegisterClassExW")
	procRegisterWindowMsg   = user32.NewProc("RegisterWindowMessageW")
	procCreateWindowEx      = user32.NewProc("CreateWindowExW")
	procDefWindowProc       = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procPostMessage         = user32.NewProc("PostMessageW")
	procGetMessage          = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procSetTimer            = user32.NewProc("SetTimer")
	procKillTimer           = user32.NewProc("KillTimer")
	procLoadIcon            = user32.NewProc("LoadIconW")
	procCreateIconIndirect  = user32.NewProc("CreateIconIndirect")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenu          = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procGetDoubleClickTime  = user32.NewProc("GetDoubleClickTime")
	procShellNotifyIcon     = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandle     = kernel32.NewProc("GetModuleHandleW")
	trayGDI32               = windows.NewLazySystemDLL("gdi32.dll")
	procCreateBitmap        = trayGDI32.NewProc("CreateBitmap")
	procDeleteObject        = trayGDI32.NewProc("DeleteObject")
	trayWindowProc          = syscall.NewCallback(trayWndProc)
	trayApps                sync.Map
)

var trayStatusColors = map[string]uint32{
	"running":      0x1570EF,
	"waiting":      0x12826B,
	"rate_limited": 0xE58A00,
	"paused":       0x697586,
	"active":       0x1570EF,
	"stopped":      0xD13A36,
}

type trayApp struct {
	hwnd                 uintptr
	dataDir              string
	logger               *safeLogger
	cancel               context.CancelFunc
	service              *managementService
	icons                map[string]uintptr
	ownedIcons           []uintptr
	lastTip              string
	lastIcon             string
	lastStopped          int
	lastGoalStopped      int
	lastGoalFailed       int
	lastRestartRequired  int
	lastCodexStopped     int
	lastSharedDisabled   int
	lastRateLimited      int
	lastUIState          string
	activeThreads        map[string]struct{}
	catalogQueued        map[string]time.Time
	initialized          bool
	settingsMu           sync.Mutex
	settingsOpen         bool
	lastDashboard        []byte
	dashboardWriteFailed bool
	taskbarCreated       uint32
	restoreIconFunc      func()
	clickState           trayClickState
	lastFullRefresh      time.Time
	lastSnapshot         ManagementSnapshot
	hasSnapshot          bool
	inputStamps          [5]trayInputStamp
}

func runTray(ctx context.Context, cancel context.CancelFunc, dataDir string, logger *safeLogger) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	taskbarCreated, err := registerTaskbarCreatedMessage()
	if err != nil {
		return err
	}
	className, _ := windows.UTF16PtrFromString(fmt.Sprintf("CodexAutoRetryTray-%d", os.Getpid()))
	instance, _, _ := procGetModuleHandle.Call(0)
	icon, _, _ := procLoadIcon.Call(0, 32512)
	wc := windowClass{Size: uint32(unsafe.Sizeof(windowClass{})), WndProc: trayWindowProc, Instance: instance, Icon: icon, IconSmall: icon, ClassName: className}
	if result, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); result == 0 {
		return fmt.Errorf("register tray window: %w", err)
	}
	hwnd, _, err := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if hwnd == 0 {
		return fmt.Errorf("create tray window: %w", err)
	}
	icons, ownedIcons := createTrayStatusIcons()
	app := &trayApp{
		hwnd: hwnd, dataDir: dataDir, logger: logger, cancel: cancel,
		taskbarCreated: taskbarCreated,
		service:        newManagementService(dataDir),
		icons:          icons,
		ownedIcons:     ownedIcons,
		activeThreads:  make(map[string]struct{}),
		catalogQueued:  make(map[string]time.Time),
	}
	trayApps.Store(hwnd, app)
	defer trayApps.Delete(hwnd)
	if !app.addIcon() {
		procDestroyWindow.Call(hwnd)
		return fmt.Errorf("add tray icon")
	}
	procSetTimer.Call(hwnd, trayTimerID, 1000, 0)
	app.refresh()
	go func() {
		<-ctx.Done()
		procPostMessage.Call(hwnd, wmClose, 0, 0)
	}()
	var message trayMessage
	for {
		result, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(result) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
	}
	return nil
}

func trayWndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	value, ok := trayApps.Load(hwnd)
	if !ok {
		result, _, _ := procDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
		return result
	}
	app := value.(*trayApp)
	if app.taskbarCreated != 0 && message == app.taskbarCreated {
		app.handleTaskbarCreated()
		return 0
	}
	switch message {
	case wmTimer:
		switch wParam {
		case trayTimerID:
			app.onTimer(time.Now().UTC())
		case traySingleClickTimerID:
			procKillTimer.Call(hwnd, traySingleClickTimerID)
			if app.clickState.onSingleClickTimer() {
				app.openSettings()
			}
		}
		return 0
	case wmAppTray:
		switch uint32(lParam) {
		case wmLButtonUp:
			if !app.clickState.onLeftButtonUp() {
				break
			}
			procKillTimer.Call(hwnd, traySingleClickTimerID)
			delay, _, _ := procGetDoubleClickTime.Call()
			if delay < 200 {
				delay = 500
			}
			procSetTimer.Call(hwnd, traySingleClickTimerID, delay, 0)
		case wmLButtonDblClk:
			procKillTimer.Call(hwnd, traySingleClickTimerID)
			app.clickState.onDoubleClick()
			delay, _, _ := procGetDoubleClickTime.Call()
			if delay < 200 {
				delay = 500
			}
			procSetTimer.Call(hwnd, traySingleClickTimerID, delay, 0)
			app.openSettings()
		case wmRButtonUp:
			app.showMenu()
		}
		return 0
	case wmCommand:
		app.handleCommand(uint16(wParam & 0xffff))
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procKillTimer.Call(hwnd, trayTimerID)
		procKillTimer.Call(hwnd, traySingleClickTimerID)
		app.removeIcon()
		app.destroyIcons()
		procPostQuitMessage.Call(0)
		return 0
	}
	result, _, _ := procDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func (a *trayApp) handleTaskbarCreated() {
	if a.restoreIconFunc != nil {
		a.restoreIconFunc()
		return
	}
	a.restoreIcon()
}

func registerTaskbarCreatedMessage() (uint32, error) {
	name, err := windows.UTF16PtrFromString(taskbarCreatedName)
	if err != nil {
		return 0, fmt.Errorf("encode %s message name: %w", taskbarCreatedName, err)
	}
	result, _, callErr := procRegisterWindowMsg.Call(uintptr(unsafe.Pointer(name)))
	if result == 0 {
		if callErr == nil {
			callErr = windows.GetLastError()
		}
		return 0, fmt.Errorf("register %s message: %w", taskbarCreatedName, callErr)
	}
	return uint32(result), nil
}

func (a *trayApp) addIcon() bool {
	data := notifyIconData{Size: uint32(unsafe.Sizeof(notifyIconData{})), HWnd: a.hwnd, ID: 1, Flags: nifMessage | nifIcon | nifTip, CallbackMessage: wmAppTray, Icon: a.icons["running"]}
	copyUTF16(data.Tip[:], "CodexWorkResumer")
	result, _, _ := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&data)))
	return result != 0
}

func (a *trayApp) removeIcon() {
	data := notifyIconData{Size: uint32(unsafe.Sizeof(notifyIconData{})), HWnd: a.hwnd, ID: 1}
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&data)))
}

func (a *trayApp) restoreIcon() {
	// Explorer owns the notification area. When it restarts, it forgets every
	// icon even though this process and its hidden window are still alive. Clear
	// the local cache so refresh cannot incorrectly skip the first NIM_MODIFY.
	a.lastTip = ""
	a.lastIcon = ""
	if !restoreTrayIcon(a.addIcon, a.removeIcon, a.refresh) {
		a.logger.Printf("tray icon restore failed category=tray_icon")
	}
}

// Keep the one-second Windows timer for countdowns and refresh requests. The
// full snapshot also reads configuration, task catalog, diagnostics and state,
// so it runs only when its inputs change or the five-second fallback expires.
func (a *trayApp) onTimer(now time.Time) {
	requested := a.consumeRefreshSignals()
	if traySnapshotDue(now, a.lastFullRefresh, requested || a.inputsChanged()) {
		a.refreshAt(now)
		return
	}
	a.refreshCountdownVisual(now)
}

func traySnapshotDue(now, previous time.Time, changed bool) bool {
	return changed || previous.IsZero() || now.Before(previous) || now.Sub(previous) >= traySnapshotInterval
}

func (a *trayApp) consumeRefreshSignals() bool {
	requested := false
	if consumeUsageRefreshRequest(a.dataDir) {
		a.service.usage.Invalidate()
		a.logger.Debugf("manual quota refresh requested")
		requested = true
	}
	if consumeTaskCatalogRefreshRequest(a.dataDir) {
		a.service.catalog.Invalidate()
		a.logger.Debugf("manual task catalog refresh requested")
		requested = true
	}
	return requested
}

func (a *trayApp) currentInputStamps() [5]trayInputStamp {
	files := [5]string{
		filepath.Join(a.dataDir, "state.json"),
		filepath.Join(a.dataDir, "status.json"),
		filepath.Join(a.dataDir, "control.json"),
		filepath.Join(a.dataDir, "config.json"),
		filepath.Join(a.dataDir, "task-plan.json"),
	}
	var stamps [5]trayInputStamp
	for index, path := range files {
		if info, err := os.Stat(path); err == nil {
			stamps[index] = trayInputStamp{modTime: info.ModTime().UnixNano(), size: info.Size(), exists: true}
		}
	}
	return stamps
}

func (a *trayApp) inputsChanged() bool {
	return a.currentInputStamps() != a.inputStamps
}

func (a *trayApp) refreshCountdownVisual(now time.Time) {
	if !a.hasSnapshot || a.lastSnapshot.PendingRetries == 0 {
		return
	}
	snapshot := traySnapshotWithCountdown(a.lastSnapshot, now)
	iconState, tip := trayVisual(snapshot)
	a.setVisual(iconState, tip)
}

func traySnapshotWithCountdown(snapshot ManagementSnapshot, now time.Time) ManagementSnapshot {
	snapshot.Retries = append([]ManagedRetry(nil), snapshot.Retries...)
	for index := range snapshot.Retries {
		retry := &snapshot.Retries[index]
		if retry.State != "pending" || retry.DueAt == "" {
			continue
		}
		if dueAt, err := time.Parse(time.RFC3339Nano, retry.DueAt); err == nil {
			retry.SecondsRemaining = max(0, int64((dueAt.Sub(now)+time.Second-1)/time.Second))
		}
	}
	return snapshot
}

func restoreTrayIcon(add func() bool, remove func(), refresh func()) bool {
	remove()
	if !add() {
		return false
	}
	refresh()
	return true
}

func (a *trayApp) refresh() {
	a.consumeRefreshSignals()
	a.refreshAt(time.Now().UTC())
}

func (a *trayApp) refreshAt(now time.Time) {
	// Capture before reading the snapshot. A file changed during the read will
	// then be noticed on the next timer tick instead of being silently skipped.
	a.inputStamps = a.currentInputStamps()
	a.lastFullRefresh = now
	snapshot, err := a.service.snapshotForTray(now)
	if err != nil {
		a.hasSnapshot = false
		a.setVisual("stopped", "CodexWorkResumer - Error · 狀態讀取失敗")
		return
	}
	a.lastSnapshot = snapshot
	a.hasSnapshot = true
	a.queueDiscoveredRateLimits(snapshot, now)
	a.persistDashboard(snapshot)
	iconState, tip := trayVisual(snapshot)
	a.setVisual(iconState, tip)
	goalStopped := goalEmptyResponseStoppedCount(snapshot.Retries)
	goalFailed := goalEmptyResponseBlockFailedCount(snapshot.Retries)
	restartRequired := stoppedReasonCount(snapshot.Retries, "codex_restart_required")
	codexStopped := stoppedReasonCount(snapshot.Retries, "codex_not_running")
	sharedDisabled := stoppedReasonCount(snapshot.Retries, "shared_app_server_disabled")
	limitStopped := retryLimitStoppedCount(snapshot.Retries)
	uiState := deriveUIState(snapshot)
	completedResumes, currentActive := completedActiveRetries(a.activeThreads, snapshot.Retries)
	if a.initialized && snapshot.ShowNotifications && restartRequired > a.lastRestartRequired {
		a.notify("需要重啟 Codex", "重啟一次 Codex 後，等待中的自動 Resume 會自行恢復。")
	} else if a.initialized && snapshot.ShowNotifications && codexStopped > a.lastCodexStopped {
		a.notify("Codex 已退出", "相關 Task 已停止自動 Resume。啟動 Codex 後可從主畫面重新開始。")
	} else if a.initialized && snapshot.ShowNotifications && sharedDisabled > a.lastSharedDisabled {
		a.notify("Codex自動續跑助手已關閉", "自動恢復未執行。請開啟 Codex自動續跑助手，再重新開始該 Task。")
	} else if a.initialized && snapshot.ShowNotifications && goalFailed > a.lastGoalFailed {
		a.notify("目標停止失敗", "目標恢復已停止，但自動設為受阻失敗。請從主畫面重新開始或檢查 Codex 狀態。")
	} else if a.initialized && snapshot.ShowNotifications && goalStopped > a.lastGoalStopped {
		a.notify("目標已自動停止", "目標連續空回覆達到上限，目標恢復已停止。")
	} else if a.initialized && snapshot.ShowNotifications && limitStopped > a.lastStopped {
		a.notify("自動 Resume 已停止", fmt.Sprintf("有 %d 個 Task 已達重試上限。", limitStopped))
	} else if a.initialized && snapshot.ShowNotifications && snapshot.RateLimitedRetries > 0 && a.lastRateLimited == 0 {
		detail := "已停止快速重試，等待額度 reset 後重新確認。"
		if seconds, ok := nextRetrySeconds(snapshot.Retries); ok {
			detail = fmt.Sprintf("已停止快速重試，約 %d 分鐘後重新確認額度。", max(1, int((seconds+59)/60)))
		}
		a.notify("Codex 額度受限", detail)
	} else if a.initialized && snapshot.ShowNotifications && snapshot.Running && uiState.Code != "error" && completedResumes > 0 {
		a.notify("Task Resume 完成", fmt.Sprintf("%d 個 Task 已完成本次恢復。", completedResumes))
	} else if a.initialized && snapshot.ShowNotifications && uiState.Code == "error" && a.lastUIState != "error" {
		a.notify("CodexWorkResumer 需要處理", uiState.Detail)
	}
	a.lastStopped = limitStopped
	a.lastGoalStopped = goalStopped
	a.lastGoalFailed = goalFailed
	a.lastRestartRequired = restartRequired
	a.lastCodexStopped = codexStopped
	a.lastSharedDisabled = sharedDisabled
	a.lastRateLimited = snapshot.RateLimitedRetries
	a.lastUIState = uiState.Code
	a.activeThreads = currentActive
	a.initialized = true
}

func (a *trayApp) queueDiscoveredRateLimits(snapshot ManagementSnapshot, now time.Time) {
	if snapshot.Paused || snapshot.MonitoringPaused || !snapshot.AutomaticRecoverySupported {
		return
	}
	recovering := make(map[string]struct{}, len(snapshot.Retries))
	for _, retry := range snapshot.Retries {
		recovering[strings.ToLower(retry.ThreadID)] = struct{}{}
	}
	for threadID := range a.catalogQueued {
		if now.Sub(a.catalogQueued[threadID]) > 2*time.Minute {
			delete(a.catalogQueued, threadID)
		}
	}
	// A positive aggregate percentage does not prove that the provider's
	// explicit quota rejection has cleared. Keep the daemon's persisted reset
	// deadline; otherwise this tray poll can undo the quota wait repeatedly.
	config, err := loadOrCreateConfig(a.service.configPath)
	if err != nil {
		return
	}
	for _, task := range snapshot.Tasks {
		threadID := strings.ToLower(task.ThreadID)
		if !shouldQueueDiscoveredRateLimit(task, snapshot.TaskPlan) {
			continue
		}
		if _, present := recovering[threadID]; present {
			continue
		}
		if queuedAt, present := a.catalogQueued[threadID]; present && now.Sub(queuedAt) < 2*time.Minute {
			continue
		}
		if _, err := validateCatalogResumePath(config, threadID, task.RolloutPath); err != nil {
			continue
		}
		if _, err := queueControlCommandWithTask(a.service.commandDir, commandResumeTask, threadID, task.RolloutPath, classRateLimit, now); err == nil {
			a.catalogQueued[threadID] = now
			a.logger.Printf("structured quota task queued thread=%s category=rate_limit", shortThreadID(threadID))
		}
	}
}

func shouldQueueDiscoveredRateLimit(task ManagedTask, plan TaskPlan) bool {
	if (task.Status != "rate_limited" && task.Status != "quota_recovered") || !task.CanResume || task.RolloutPath == "" {
		return false
	}
	// An explicit task plan is an opt-in quota recovery scope. A plan with no
	// selected tasks intentionally disables newly discovered quota resumes;
	// older installations without a plan retain their existing behaviour.
	return !plan.Configured || plan.IsSelected(task.ThreadID)
}

func completedActiveRetries(previous map[string]struct{}, retries []ManagedRetry) (int, map[string]struct{}) {
	currentActive := make(map[string]struct{})
	currentVisible := make(map[string]struct{})
	for _, retry := range retries {
		currentVisible[retry.ThreadID] = struct{}{}
		if retry.State == "starting" || retry.State == "running" {
			currentActive[retry.ThreadID] = struct{}{}
		}
	}
	completed := 0
	for threadID := range previous {
		if _, stillVisible := currentVisible[threadID]; !stillVisible {
			completed++
		}
	}
	return completed, currentActive
}

func trayVisual(snapshot ManagementSnapshot) (string, string) {
	state := deriveUIState(snapshot)
	if state.Code == "paused" {
		return "paused", "CodexWorkResumer - Paused · 監控已暫停"
	}
	tip := "CodexWorkResumer - Waiting · 等待工作"
	iconState := "waiting"
	if !snapshot.Running {
		return "stopped", "CodexWorkResumer - Error · 背景服務未執行"
	}
	cliFallbackReady := snapshot.CLIResumeFallbackEnabled &&
		(snapshot.ControllerState == "shared_app_server_disabled" || snapshot.ControllerState == "codex_restart_required" ||
			snapshot.ControllerState == "codex_not_running" || snapshot.ControllerState == "codex_app_not_ready")
	if cliFallbackReady {
		tip = "CodexWorkResumer - Waiting · 背景 CLI 備援待命"
		iconState = "waiting"
	} else if snapshot.ControllerState == "codex_restart_required" {
		tip = "CodexWorkResumer - Waiting · 請由安全入口啟動 Codex"
		iconState = "paused"
	} else if snapshot.ControllerState == "codex_not_running" && snapshot.StoppedRetries > 0 {
		tip = "CodexWorkResumer - Error · Codex 已退出"
		iconState = "stopped"
	} else if snapshot.ControllerState == "shared_app_server_disabled" {
		tip = "CodexWorkResumer - Waiting · Codex自動續跑助手已關閉"
		iconState = "paused"
	} else if snapshot.ControllerState == "shared_app_server_port_reserved" {
		tip = "CodexWorkResumer - Error · 本機連接埠被 Windows 保留"
		iconState = "stopped"
	} else if snapshot.ControllerState == "shared_app_server_port_conflict" {
		tip = fmt.Sprintf("CodexWorkResumer - Waiting · 連接埠 %d 正在遷移", snapshot.SharedAppServerPort)
		iconState = "paused"
	} else if snapshot.ControllerState == "shared_app_server_migration_deferred" {
		tip = "CodexWorkResumer - Waiting · 等待 Codex 關閉後遷移"
		iconState = "paused"
	} else if snapshot.Paused {
		tip = "CodexWorkResumer - Waiting · 自動 Resume 已停止"
		iconState = "paused"
	} else if snapshot.RateLimitedRetries > 0 {
		if seconds, ok := nextRetrySeconds(snapshot.Retries); ok {
			minutes := (seconds + 59) / 60
			if minutes < 1 {
				minutes = 1
			}
			tip = fmt.Sprintf("CodexWorkResumer - Rate Limited · %d 個 Task，約 %d 分鐘", snapshot.RateLimitedRetries, minutes)
		} else {
			tip = fmt.Sprintf("CodexWorkResumer - Rate Limited · %d 個 Task", snapshot.RateLimitedRetries)
		}
		iconState = "rate_limited"
	} else if snapshot.ActiveRetries > 0 {
		tip = fmt.Sprintf("CodexWorkResumer - Running · 正在 Resume %d 個 Task", snapshot.ActiveRetries)
		iconState = "active"
	} else if seconds, ok := nextRetrySeconds(snapshot.Retries); ok {
		tip = fmt.Sprintf("CodexWorkResumer - Waiting · %d 秒後 Resume", seconds)
		iconState = "waiting"
	} else if snapshot.StoppedRetries > 0 {
		tip = fmt.Sprintf("CodexWorkResumer - Error · %d 個 Task 已停止", snapshot.StoppedRetries)
		iconState = "stopped"
	}
	return iconState, tip
}

type trayDashboardSnapshot struct {
	CodexUsage                 UsageSnapshot       `json:"codex_usage"`
	Retries                    []ManagedRetry      `json:"retries"`
	Tasks                      []ManagedTask       `json:"tasks"`
	TaskPlan                   TaskPlan            `json:"task_plan"`
	TaskPlanError              string              `json:"task_plan_error,omitempty"`
	TaskCatalogAvailable       bool                `json:"task_catalog_available"`
	TaskCatalogRefreshing      bool                `json:"task_catalog_refreshing"`
	AutomaticRecoverySupported bool                `json:"automatic_recovery_supported"`
	RecoveryCapabilityReason   string              `json:"recovery_capability_reason,omitempty"`
	ResumeIntervalSeconds      int                 `json:"resume_interval_seconds"`
	UsageRefreshMinutes        int                 `json:"usage_refresh_minutes"`
	AutoScanEnabled            bool                `json:"auto_scan_enabled"`
	ScanIntervalSeconds        int                 `json:"scan_interval_seconds"`
	Paused                     bool                `json:"paused"`
	MonitoringPaused           bool                `json:"monitoring_paused"`
	Status                     UIState             `json:"status"`
	StatusCatalog              []UIStateDefinition `json:"status_catalog"`
	QuotaCatalog               []UIQuotaDefinition `json:"quota_catalog"`
	Diagnostics                DiagnosticSnapshot  `json:"diagnostics"`
}

func (a *trayApp) persistDashboard(snapshot ManagementSnapshot) {
	retries := append([]ManagedRetry(nil), snapshot.Retries...)
	for index := range retries {
		retries[index].SecondsRemaining = 0
	}
	diagnostics := snapshot.Diagnostics
	// The settings window derives uptime from StartedAt. Keeping this field
	// stable prevents a dashboard.json rewrite every second while idle.
	diagnostics.UptimeSeconds = 0
	value := trayDashboardSnapshot{
		CodexUsage:                 snapshot.CodexUsage,
		Retries:                    retries,
		Tasks:                      append([]ManagedTask(nil), snapshot.Tasks...),
		TaskPlan:                   snapshot.TaskPlan,
		TaskPlanError:              snapshot.TaskPlanError,
		TaskCatalogAvailable:       snapshot.TaskCatalogAvailable,
		TaskCatalogRefreshing:      snapshot.TaskCatalogRefreshing,
		AutomaticRecoverySupported: snapshot.AutomaticRecoverySupported,
		RecoveryCapabilityReason:   snapshot.RecoveryCapabilityReason,
		ResumeIntervalSeconds:      snapshot.ResumeIntervalSeconds,
		UsageRefreshMinutes:        snapshot.UsageRefreshMinutes,
		AutoScanEnabled:            snapshot.AutoScanEnabled,
		ScanIntervalSeconds:        snapshot.ScanIntervalSeconds,
		Paused:                     snapshot.Paused,
		MonitoringPaused:           snapshot.MonitoringPaused,
		Status:                     deriveUIState(snapshot),
		StatusCatalog:              append([]UIStateDefinition(nil), uiStateCatalog...),
		QuotaCatalog:               append([]UIQuotaDefinition(nil), uiQuotaCatalog...),
		Diagnostics:                diagnostics,
	}
	encoded, err := json.Marshal(value)
	if err != nil || bytes.Equal(encoded, a.lastDashboard) {
		return
	}
	if err := writeJSONAtomic(filepath.Join(a.dataDir, "dashboard.json"), value); err != nil {
		if !a.dashboardWriteFailed {
			a.logger.Printf("dashboard cache write failed category=dashboard_cache")
		}
		a.dashboardWriteFailed = true
		return
	}
	a.dashboardWriteFailed = false
	a.lastDashboard = encoded
}

func stoppedReasonCount(retries []ManagedRetry, reason string) int {
	count := 0
	for _, retry := range retries {
		if retry.State == "stopped" && retry.StopReason == reason {
			count++
		}
	}
	return count
}

func retryLimitStoppedCount(retries []ManagedRetry) int {
	count := 0
	for _, retry := range retries {
		if retry.State != "stopped" {
			continue
		}
		switch retry.StopReason {
		case "recovery_attempt_limit", "consecutive_retry_limit", "retry_limit":
			count++
		}
	}
	return count
}

func goalEmptyResponseStoppedCount(retries []ManagedRetry) int {
	count := 0
	for _, retry := range retries {
		if retry.State == "stopped" && retry.StopReason == goalEmptyResponseStopReason {
			count++
		}
	}
	return count
}

func goalEmptyResponseBlockFailedCount(retries []ManagedRetry) int {
	count := 0
	for _, retry := range retries {
		if retry.State == "stopped" && retry.StopReason == goalEmptyResponseBlockFailReason {
			count++
		}
	}
	return count
}

func nextRetrySeconds(retries []ManagedRetry) (int64, bool) {
	var seconds int64
	found := false
	for _, retry := range retries {
		if retry.State != "pending" {
			continue
		}
		if !found || retry.SecondsRemaining < seconds {
			seconds, found = retry.SecondsRemaining, true
		}
	}
	return seconds, found
}

func (a *trayApp) setTip(tip string) {
	a.setVisual(a.lastIcon, tip)
}

func (a *trayApp) setVisual(iconState, tip string) {
	if iconState == "" {
		iconState = "running"
	}
	if tip == a.lastTip {
		if iconState == a.lastIcon {
			return
		}
	}
	a.lastTip = tip
	a.lastIcon = iconState
	data := notifyIconData{Size: uint32(unsafe.Sizeof(notifyIconData{})), HWnd: a.hwnd, ID: 1, Flags: nifTip | nifIcon, Icon: a.icons[iconState]}
	copyUTF16(data.Tip[:], tip)
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&data)))
}

func loadSharedIcon(resourceID uintptr) uintptr {
	icon, _, _ := procLoadIcon.Call(0, resourceID)
	return icon
}

func createTrayStatusIcons() (map[string]uintptr, []uintptr) {
	icons := make(map[string]uintptr, len(trayStatusColors))
	owned := make([]uintptr, 0, len(trayStatusColors))
	for state, color := range trayStatusColors {
		icon := createTrayStatusIcon(color)
		if icon == 0 {
			fallback := uintptr(32515)
			if state == "stopped" {
				fallback = 32513
			} else if state == "running" || state == "active" {
				fallback = 32516
			}
			icons[state] = loadSharedIcon(fallback)
			continue
		}
		icons[state] = icon
		owned = append(owned, icon)
	}
	return icons, owned
}

func createTrayStatusIcon(rgb uint32) uintptr {
	const size = 32
	pixels := make([]uint32, size*size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)-15.5, float64(y)-15.5
			distance := math.Hypot(dx, dy)
			if distance <= 14.5 {
				pixels[y*size+x] = 0xFF000000 | rgb
			}
			angle := math.Atan2(dy, dx)
			if distance >= 8.0 && distance <= 10.8 && !(angle > -1.05 && angle < -0.10) {
				pixels[y*size+x] = 0xFFFFFFFF
			}
			// Arrowhead closes the circular resume mark at the upper-right.
			if x >= 20 && x <= 27 && y >= 5 && y <= 13 && (x+y <= 34) {
				pixels[y*size+x] = 0xFFFFFFFF
			}
		}
	}
	colorBitmap, _, _ := procCreateBitmap.Call(size, size, 1, 32, uintptr(unsafe.Pointer(&pixels[0])))
	if colorBitmap == 0 {
		return 0
	}
	defer procDeleteObject.Call(colorBitmap)
	maskBits := make([]byte, size*size/8)
	maskBitmap, _, _ := procCreateBitmap.Call(size, size, 1, 1, uintptr(unsafe.Pointer(&maskBits[0])))
	if maskBitmap == 0 {
		return 0
	}
	defer procDeleteObject.Call(maskBitmap)
	iconInfo := trayIconInfo{IsIcon: 1, Mask: maskBitmap, Color: colorBitmap}
	icon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&iconInfo)))
	return icon
}

func (a *trayApp) destroyIcons() {
	for _, icon := range a.ownedIcons {
		if icon != 0 {
			procDestroyIcon.Call(icon)
		}
	}
	a.ownedIcons = nil
}

func (a *trayApp) notify(title, text string) {
	data := notifyIconData{Size: uint32(unsafe.Sizeof(notifyIconData{})), HWnd: a.hwnd, ID: 1, Flags: nifInfo, InfoFlags: 0x1}
	copyUTF16(data.InfoTitle[:], title)
	copyUTF16(data.Info[:], text)
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&data)))
}

func (a *trayApp) showMenu() {
	snapshot, _ := a.service.snapshot(time.Now().UTC())
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	status := a.lastTip
	if strings.HasPrefix(status, "CodexWorkResumer - ") {
		status = strings.TrimPrefix(status, "CodexWorkResumer - ")
	}
	appendTrayMenu(menu, mfGrayed, 0, status)
	appendTrayMenu(menu, mfSeparator, 0, "")
	appendTrayMenu(menu, mfString, menuOpenSettings, "開啟主畫面…")
	monitorText := "暫停監控"
	if snapshot.MonitoringPaused {
		monitorText = "恢復監控"
	}
	appendTrayMenu(menu, mfString, menuToggleMonitor, monitorText)
	pauseText := "停止自動 Resume"
	if snapshot.Paused {
		pauseText = "恢復自動 Resume"
	}
	appendTrayMenu(menu, mfString, menuTogglePause, pauseText)
	appendTrayMenu(menu, mfSeparator, 0, "")
	appendTrayMenu(menu, mfString, menuExit, "結束程式")
	var point trayPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&point)))
	procSetForegroundWindow.Call(a.hwnd)
	command, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd, uintptr(point.X), uintptr(point.Y), 0, a.hwnd, 0)
	if command != 0 {
		a.handleCommand(uint16(command))
	}
	procPostMessage.Call(a.hwnd, wmNull, 0, 0)
}

func appendTrayMenu(menu uintptr, flags uint32, id uint16, label string) {
	var ptr *uint16
	if label != "" {
		ptr, _ = windows.UTF16PtrFromString(label)
	}
	procAppendMenu.Call(menu, uintptr(flags), uintptr(id), uintptr(unsafe.Pointer(ptr)))
}

func (a *trayApp) handleCommand(command uint16) {
	switch command {
	case menuOpenSettings:
		a.openSettings()
	case menuToggleMonitor:
		snapshot, err := a.service.snapshot(time.Now().UTC())
		if err == nil {
			_, _ = a.service.setMonitoringPaused(!snapshot.MonitoringPaused, time.Now().UTC())
		}
		a.refresh()
	case menuTogglePause:
		snapshot, err := a.service.snapshot(time.Now().UTC())
		if err == nil {
			_, _ = a.service.setPaused(!snapshot.Paused, time.Now().UTC())
		}
		a.refresh()
	case menuExit:
		a.cancel()
		procPostMessage.Call(a.hwnd, wmClose, 0, 0)
	}
}

func (a *trayApp) openSettings() {
	a.settingsMu.Lock()
	if a.settingsOpen {
		a.settingsMu.Unlock()
		return
	}
	a.settingsOpen = true
	a.settingsMu.Unlock()
	scriptPath, err := ensureSettingsScript(a.dataDir)
	if err != nil {
		a.logger.Printf("settings open failed category=settings_script")
		a.settingsFinished()
		return
	}
	config, err := loadOrCreateConfig(filepath.Join(a.dataDir, "config.json"))
	if err != nil {
		a.logger.Printf("settings open failed category=settings_config")
		a.settingsFinished()
		return
	}
	powerShell, err := resolvePowerShellExecutable(config.PowerShellExecutable)
	if err != nil {
		a.logger.Printf("settings open failed category=settings_shell")
		a.settingsFinished()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		a.logger.Printf("settings open failed category=settings_executable")
		a.settingsFinished()
		return
	}
	arguments := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-STA", "-File", scriptPath, "-DataDir", a.dataDir, "-Executable", executable}
	if os.Getenv("CODEX_AUTO_RETRY_SETTINGS_SMOKE") == "1" {
		arguments = append(arguments, "-SmokeTest")
	}
	command := exec.Command(powerShell, arguments...)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := command.Start(); err != nil {
		a.logger.Printf("settings open failed category=settings_start")
		a.settingsFinished()
		return
	}
	go func() {
		if err := command.Wait(); err != nil {
			a.logger.Printf("settings process stopped category=settings_process")
		}
		a.settingsFinished()
	}()
}

func (a *trayApp) settingsFinished() {
	a.settingsMu.Lock()
	a.settingsOpen = false
	a.settingsMu.Unlock()
}

func ensureSettingsScript(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "settings.ps1")
	content := ensureUTF8BOM([]byte(settingsPowerShell))
	current, _ := os.ReadFile(path)
	if string(current) == string(content) {
		return path, nil
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func ensureUTF8BOM(content []byte) []byte {
	bom := []byte{0xef, 0xbb, 0xbf}
	if bytes.HasPrefix(content, bom) {
		return content
	}
	return append(bom, content...)
}

func copyUTF16(destination []uint16, value string) {
	encoded, _ := windows.UTF16FromString(value)
	if len(encoded) > len(destination) {
		encoded = encoded[:len(destination)]
	}
	copy(destination, encoded)
	if len(destination) > 0 {
		destination[len(destination)-1] = 0
	}
}
