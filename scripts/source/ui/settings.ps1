param(
    [Parameter(Mandatory = $true)][string]$DataDir,
    [Parameter(Mandatory = $true)][string]$Executable,
    [switch]$SmokeTest
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
[System.Windows.Forms.Application]::EnableVisualStyles()

# A shared-backend health check can involve starting Codex and waiting for a
# WebSocket handshake. Keep the settings window responsive while it runs, and
# never allow a broken child process to hold the window open indefinitely.
$localCommandTimeoutMilliseconds = 35000
$localCommandExitPortReserved = 2
$localCommandExitPortConflict = 3
$script:localCommandProcess = $null
$script:localCommandTimedOut = $false
$script:localCommandInProgress = $false
$script:safeLaunchInProgress = $false
$script:memoryGuardTriggered = $false
$script:darkTheme = $null
$script:lastThemeCheck = [DateTimeOffset]::MinValue
$script:debugEnabled = $false
$script:lastDiagnostics = $null
$script:taskSortColumn = 4
$script:taskSortDescending = $true
$script:taskSortWasUserChosen = $false
$script:taskPlan = $null
$script:taskPlanSaveScheduled = $false
$script:taskPlanSaveInProgress = $false
$script:taskListHydrating = $false
$script:taskDragInProgress = $false
$script:taskDragHoverItem = $null
$script:taskDragHoverBackColor = $null
$script:taskDragHoverAfter = $false
$taskPlanMaxIDs = 100
$script:resumeRouteAvailable = $false

$configPath = Join-Path $DataDir 'config.json'
$controlPath = Join-Path $DataDir 'control.json'
$statusPath = Join-Path $DataDir 'status.json'
$statePath = Join-Path $DataDir 'state.json'
$dashboardPath = Join-Path $DataDir 'dashboard.json'
$taskPlanPath = Join-Path $DataDir 'task-plan.json'
$diagnosticSettingsPath = Join-Path $DataDir 'diagnostics.json'
$smokeClosePath = Join-Path $DataDir 'settings-smoke-close.signal'
$uiLangPath = Join-Path $DataDir 'ui-language.json'
$safeLauncherPath = Join-Path $env:USERPROFILE 'plugins\codex-auto-retry\scripts\launch-codex.ps1'

function Get-SharedModeRequested($Config) {
    if (-not $Config) { return $false }
    if ($Config.PSObject.Properties['shared_app_server_requested']) { return [bool]$Config.shared_app_server_requested }
    return [bool]$Config.shared_app_server_enabled
}

function Read-JsonFile {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return $null }
    $stream = $null
    $reader = $null
    try {
        $share = [System.IO.FileShare]([System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete)
        $stream = [System.IO.FileStream]::new(
            $Path,
            [System.IO.FileMode]::Open,
            [System.IO.FileAccess]::Read,
            $share
        )
        $reader = [System.IO.StreamReader]::new(
            $stream,
            [System.Text.UTF8Encoding]::new($false),
            $true,
            1024,
            $true
        )
        return $reader.ReadToEnd() | ConvertFrom-Json
    } catch {
        return $null
    } finally {
        if ($reader) { $reader.Dispose() }
        if ($stream) { $stream.Dispose() }
    }
}

$script:currentLanguage = 'zh'
$script:refreshFrame = 0
if (Test-Path -LiteralPath $uiLangPath) {
    try {
        $savedLangRecord = Read-JsonFile $uiLangPath
        if ($savedLangRecord -and $savedLangRecord.language -eq 'en') {
            $script:currentLanguage = 'en'
        }
    } catch { }
}

$script:i18n = @{
    'form_title'              = @{ zh = 'CodexWorkResumer'; en = 'CodexWorkResumer' }
    'lang_button'             = @{ zh = 'English'; en = '中文' }
    'status_group'            = @{ zh = '目前狀態'; en = 'Current Status' }
    'status_loading'          = @{ zh = '正在讀取…'; en = 'Loading...' }
    'status_not_running'      = @{ zh = '背景服務：未執行'; en = 'Background Service: Not Running' }
    'status_disconnected'     = @{ zh = '續跑通道：Codex 尚未接入'; en = 'Resume Channel: Codex Not Connected' }
    'status_disconnected_hint' = @{ zh = 'Codex自動續跑助手已啟動；請使用安全啟動入口重新啟動 Codex。'; en = 'Codex Auto-Resume Assistant is running; relaunch Codex with the safe launcher.' }
    'startup_approval_enabled' = @{ zh = '登入啟動已啟用'; en = 'Sign-in startup enabled' }
    'startup_approval_disabled' = @{ zh = '登入啟動已停用'; en = 'Sign-in startup disabled' }
    'startup_approval_unknown' = @{ zh = '登入啟動狀態未知'; en = 'Sign-in startup unknown' }
    'status_exited'           = @{ zh = '續跑通道：Codex 已退出'; en = 'Resume Channel: Codex Exited' }
    'status_shared_temp_unavail' = @{ zh = '續跑通道：暫時無法使用'; en = 'Resume Channel: Temporarily Unavailable' }
    'status_shared_disabled'  = @{ zh = '續跑通道：已停用'; en = 'Resume Channel: Disabled' }
    'status_cli_fallback_ready' = @{ zh = '續跑通道：背景 CLI 備援待命'; en = 'Resume Channel: CLI Fallback Ready' }
    'status_port_reserved'    = @{ zh = '本機連接埠被 Windows 保留，重試未執行'; en = 'Port Reserved by Windows' }
    'status_port_conflict'    = @{ zh = '首選本機連接埠無法使用，啟用時將自動選擇安全連接埠'; en = 'Port Conflict (Auto-Selecting)' }
    'status_migration_deferred' = @{ zh = '等待 Codex 關閉後完成 Codex自動續跑助手遷移'; en = 'Waiting for Codex to Exit' }
    'status_config_invalid'   = @{ zh = 'Codex自動續跑助手設定不相容，已切回官方後台'; en = 'Auto-Resume Assistant Config Reverted' }
    'status_paused'           = @{ zh = '已暫停'; en = 'Paused' }
    'status_monitoring_paused' = @{ zh = '監控已暫停'; en = 'Monitoring Paused' }
    'status_rate_limited'     = @{ zh = 'Codex 回報額度受限（等待重設）'; en = 'Codex reported a rate limit (waiting for reset)' }
    'status_quota_recovered'  = @{ zh = '額度已恢復，等待續跑'; en = 'Quota recovered; ready to resume' }
    'status_channel_unavailable' = @{ zh = '續跑通道尚未就緒'; en = 'Resume channel is not ready' }
    'status_running'          = @{ zh = '續跑通道：可以使用'; en = 'Resume Channel: Ready' }
    'queue_summary'           = @{ zh = '佇列：{0} 等待 / {1} 執行 / {2} 停止'; en = 'Queue: {0} wait / {1} run / {2} stop' }
    'queue_empty'             = @{ zh = '待續跑工作：目前沒有'; en = 'Resumable Tasks: None' }
    'next_waiting_service'    = @{ zh = '下次重試：等待服務啟動'; en = 'Next Retry: Waiting for Service' }
    'next_waiting_resume'     = @{ zh = '下次重試：等待恢復'; en = 'Next Retry: Waiting for Resume' }
    'next_seconds'            = @{ zh = '下次重試：{0} 秒'; en = 'Next Retry: {0}s' }
    'next_running'            = @{ zh = '下次重試：正在執行'; en = 'Next Retry: Running' }
    'next_none'               = @{ zh = '下次重試：--'; en = 'Next Retry: --' }
    'last_scan'               = @{ zh = '最近掃描時間：'; en = 'Last Scan Time: ' }
    'usage_group'             = @{ zh = 'Codex 剩餘額度'; en = 'Codex Remaining Quota' }
    'usage_unavailable'       = @{ zh = 'N/A'; en = 'N/A' }
    'usage_remaining'         = @{ zh = '剩餘 {0}%'; en = '{0}% left' }
    'usage_reset'             = @{ zh = 'Reset：{0}'; en = 'Reset: {0}' }
    'usage_rechecking'        = @{ zh = '重新確認中…'; en = 'Refreshing...' }
    'usage_source_official'   = @{ zh = '官方即時查詢'; en = 'Official live query' }
    'usage_source_rollout'    = @{ zh = '近期本機快照'; en = 'Recent local snapshot' }
    'usage_observed_at'       = @{ zh = '資料時間：'; en = 'Observed: ' }
    'queue_group'             = @{ zh = 'Session 清單與執行順序'; en = 'Sessions and Queue Order' }
    'section_queue'           = @{ zh = '02  Session 清單與執行順序'; en = '02  Sessions and Queue Order' }
    'section_scheduler'       = @{ zh = '03  排程模式與參數'; en = '03  Scheduling Mode and Parameters' }
    'section_monitor'         = @{ zh = '01  監控與操作'; en = '01  Monitoring and Controls' }
    'section_history'         = @{ zh = '04  通知與執行紀錄'; en = '04  Notifications and History' }
    'scheduler_intro'         = @{ zh = '勾選只加入監控名單；模式只決定本程式下一次如何發起合格的續跑。'; en = 'Selection watches a task; the mode controls only the next eligible resume.' }
    'mode_sequential'         = @{ zh = '循序完成'; en = 'Sequential completion' }
    'mode_staggered'          = @{ zh = '間隔啟動'; en = 'Staggered starts' }
    'parallel_limit'          = @{ zh = '可觀測執行上限'; en = 'Observable running limit' }
    'scheduler_scope'         = @{ zh = '僅計入本程式可觀測的工作；外部手動啟動可能超過上限。'; en = 'Best effort for observable work only; external starts may exceed the limit.' }
    'scheduler_scope_sequential' = @{ zh = '循序：正向完成訊號、安靜期、下次掃描無活動才交棒；不保證整項工作完成。'; en = ('Sequential: positive turn completion + quiet time + clear next scan.' + [Environment]::NewLine + 'This hands off scheduling; whole-task completion is not guaranteed.') }
    'scheduler_save'          = @{ zh = '儲存排程設定'; en = 'Save scheduling' }
    'monitor_control_note'    = @{ zh = '暫停只阻止新的自動續跑，不會停止已在 Codex 執行的工作。'; en = 'Pause stops new automatic resumes, not work already running in Codex.' }
    'pause_dispatch'          = @{ zh = '暫停自動續跑'; en = 'Pause auto-resume' }
    'resume_dispatch'         = @{ zh = '恢復自動續跑'; en = 'Resume auto-resume' }
    'history_summary'        = @{ zh = '重要事件才顯示 Windows 通知；詳細紀錄留在 Log 資料夾。'; en = 'Windows alerts are reserved for important events; details remain in the Log folder.' }
    'history_expand'         = @{ zh = '展開'; en = 'Expand' }
    'history_collapse'       = @{ zh = '收合'; en = 'Collapse' }
    'history_latest_error'   = @{ zh = '最近錯誤：'; en = 'Latest error: ' }
    'history_none'           = @{ zh = '目前無錯誤摘要'; en = 'No recent error summary' }
    'task_selection_hint'     = @{ zh = '勾選＝監控額度；反白＝操作對象；拖曳 ☰ 改順序。暫時故障仍自動救援。'; en = 'Check = quota watch; highlight = action; drag ☰ to reorder.' }
    'task_drag_hint'          = @{ zh = '拖曳 ☰ 調整監控順序；點欄位標題可暫時排序。'; en = 'Drag ☰ to change watch order; click a column heading to sort the view.' }
    'task_drag_before'        = @{ zh = '↑ 放開後插在「{0}」之前'; en = '↑ Release to insert before "{0}"' }
    'task_drag_after'         = @{ zh = '↓ 放開後插在「{0}」之後'; en = '↓ Release to insert after "{0}"' }
    'task_selection_legacy'   = @{ zh = '未設定名單：舊版仍自動救援全部額度故障；勾選後僅監控勾選項。'; en = 'No list: quota recovery watches all. Check to narrow it; no instant Resume.' }
    'task_plan_conflict'      = @{ zh = '預選或順序未儲存，可能其他視窗已更新。清單將載入最新設定，請再操作一次。'; en = 'Selection or order was not saved. Another window may have changed it. The latest settings will load; please try again.' }
    'task_plan_invalid'       = @{ zh = '預選設定檔無法讀取；已停止修改預選，請先修復檔案。'; en = 'The task selection file cannot be read. Changes are disabled until it is repaired.' }
    'task_plan_full'          = @{ zh = '最多只能儲存 100 個工作；已勾選及目前顯示的工作不會被悄悄移除。請先取消部分勾選後再試。'; en = 'At most 100 tasks can be saved. Checked and visible tasks will not be removed silently; uncheck some tasks and try again.' }
    'task_info_unavailable'   = @{ zh = '資訊無法取得'; en = 'Info unavailable' }
    'task_stopped_watched'    = @{ zh = '停止重試｜監控勾選'; en = 'Retry stopped (watch)' }
    'task_stopped_unwatched'  = @{ zh = '停止重試'; en = 'Retry stopped' }
    'task_stopped_catalog'    = @{ zh = 'Codex 工作已停止'; en = 'Codex task stopped' }
    'task_stopped_catalog_watched' = @{ zh = '工作停止｜監控勾選'; en = 'Task stopped (watch)' }
    'task_quota_wait'         = @{ zh = '等待額度重設'; en = 'Waiting for quota' }
    'task_quota_check'        = @{ zh = '等待確認額度'; en = 'Checking quota' }
    'task_retry_pending'      = @{ zh = '等待自動續跑'; en = 'Waiting to resume' }
    'task_retry_running'      = @{ zh = '正在續跑'; en = 'Resuming' }
    'task_recovery_success'   = @{ zh = '續跑成功'; en = 'Resume succeeded' }
    'task_recovery_handoff'   = @{ zh = '續跑成功・待交棒'; en = 'Resume OK · handoff' }
    'task_stopped_detail'     = @{ zh = '本次自動重試已停止。額度監控勾選不會重啟這次已停止的重試。原因：{0}'; en = 'This retry has stopped. A quota-watch checkmark does not restart this stopped retry. Reason: {0}' }
    'task_success_detail'     = @{ zh = '已確認同一工作自動續跑的回合無錯誤結束；這不代表整個專案已完成。'; en = 'The automatic resume turn for this same task ended without an error; this does not prove the whole project is complete.' }
    'btn_refresh_tasks'       = @{ zh = '重新整理'; en = 'Refresh' }
    'task_refreshing'         = @{ zh = '更新中'; en = 'Refreshing' }
    'check_auto_scan'         = @{ zh = '自動掃描'; en = 'Auto scan' }
    'label_every'             = @{ zh = '每'; en = 'Every' }
    'label_seconds'           = @{ zh = '秒'; en = 'seconds' }
    'col_task'                = @{ zh = 'Task / Session'; en = 'Task / Session' }
    'col_project'             = @{ zh = '專案路徑'; en = 'Project Path' }
    'col_task_type'           = @{ zh = '類型'; en = 'Type' }
    'col_status'              = @{ zh = '狀態'; en = 'Status' }
    'col_last_activity'       = @{ zh = '最後活動'; en = 'Last Activity' }
    'col_countdown'           = @{ zh = '倒數'; en = 'Countdown' }
    'col_recovery'            = @{ zh = '本次恢復'; en = 'Recoveries' }
    'col_consecutive'         = @{ zh = '連續重試'; en = 'Repeats' }
    'col_class'               = @{ zh = '故障類型'; en = 'Type' }
    'btn_retry_now'           = @{ zh = '立即 Resume 反白列'; en = 'Resume Highlighted' }
    'btn_cancel_retry'        = @{ zh = '取消反白等待'; en = 'Cancel Highlighted' }
    'btn_restart_retry'       = @{ zh = '全選監控'; en = 'Watch All' }
    'action_hint_select'      = @{ zh = '先反白選取工作才能操作；打勾只設定額度監控。'; en = 'Highlight a task to use these actions; checking it only sets quota monitoring.' }
    'action_hint_busy'        = @{ zh = '正在處理操作，請稍候。'; en = 'An action is in progress; please wait.' }
    'action_hint_unavailable' = @{ zh = '反白工作目前沒有可執行的 Resume 或取消等待動作。'; en = 'The highlighted task has no Resume or cancel-wait action right now.' }
    'action_hint_route'       = @{ zh = '反白工作可續跑，但續跑通道尚未就緒。'; en = 'The highlighted task can resume, but the resume channel is not ready.' }
    'action_hint_resume'      = @{ zh = '反白工作可立即續跑；打勾狀態不影響這顆按鈕。'; en = 'The highlighted task can resume now; its checkbox does not affect this button.' }
    'action_hint_cancel'      = @{ zh = '反白工作正在等待，可取消等待；打勾狀態不影響這顆按鈕。'; en = 'The highlighted task is waiting; cancel it here. Its checkbox does not affect this button.' }
    'action_hint_both'        = @{ zh = '反白工作有可用動作；立即續跑與取消等待只作用於符合條件的項目。'; en = 'Actions are available for highlighted tasks; each button applies only to eligible items.' }
    'btn_safe_launch'         = @{ zh = '安全啟動 Codex'; en = 'Safe Launch Codex' }
    'safe_launch_running'     = @{ zh = '正在等待 Codex 關閉並透過安全入口啟動…'; en = 'Waiting for Codex to close, then launching safely...' }
    'safe_launch_done'        = @{ zh = '已要求安全啟動 Codex，請稍候重新整理狀態。'; en = 'Safe Codex launch requested; status will refresh shortly.' }
    'safe_launch_failed'      = @{ zh = '安全啟動失敗，請確認 Codex 已完全退出。'; en = 'Safe launch failed; confirm that Codex is fully closed.' }
    'safe_launch_missing'     = @{ zh = '找不到安全啟動指令碼，請重新安裝外掛。'; en = 'Safe launcher is missing; reinstall the plugin.' }
    'safe_launch_disabled'    = @{ zh = '請先啟用 Codex自動續跑助手。'; en = 'Enable Codex Auto-Resume Assistant first.' }
    'settings_group'          = @{ zh = '自動重試設定'; en = 'Auto Retry Settings' }
    'check_enabled'           = @{ zh = '啟用自動重試'; en = 'Enable Auto Retry' }
    'check_monitoring'        = @{ zh = '啟用 Task 與額度監控'; en = 'Enable task and quota monitoring' }
    'guide_tab'               = @{ zh = '狀態說明'; en = 'Status Guide' }
    'guide_title'             = @{ zh = '狀態、顏色與行為說明'; en = 'Status, Color, and Behavior Guide' }
    'guide_state'             = @{ zh = '狀態'; en = 'State' }
    'guide_color'             = @{ zh = '顏色'; en = 'Color' }
    'guide_meaning'           = @{ zh = '代表意義'; en = 'Meaning' }
    'guide_behavior'          = @{ zh = '程式行為'; en = 'Behavior' }
    'guide_notice'            = @{ zh = '通知'; en = 'Notification' }
    'guide_source'            = @{ zh = '未設名單沿用舊版全工作額度救援；勾選後僅監控勾選項。'; en = 'Legacy quota recovery watches all until a selection is saved.' }
    'guide_scheduler'         = @{ zh = ('循序交棒須正向完成訊號、安靜期與下次掃描無活動；不代表整項工作已完成。' + [Environment]::NewLine + '暫停只阻止新的自動續跑，不會中止既有工作。'); en = ('Sequential handoff needs positive completion, quiet time, and a clear next scan.' + [Environment]::NewLine + 'This does not prove whole-task completion; pause will not stop running work.') }
    'guide_task_outcomes'     = @{ zh = ('清單「停止重試｜監控勾選」：勾選保留，但本次已停止的重試不會自行重啟。' + [Environment]::NewLine + '「續跑成功」：同一工作續跑回合無錯誤結束，不代表整項工作完成。'); en = ('Task list: Retry stopped (watch) keeps the checkmark, but a stopped retry will not restart by itself.' + [Environment]::NewLine + 'Resume succeeded means same-task recovery ended without error, not that the project is complete.') }
    'guide_waiting_data'      = @{ zh = '等待背景服務提供狀態定義。'; en = 'Waiting for status definitions from the service.' }
    'diagnostics_tab'         = @{ zh = '診斷'; en = 'Diagnostics' }
    'diagnostics_title'       = @{ zh = '執行狀態與安全診斷'; en = 'Runtime and Safe Diagnostics' }
    'diagnostics_privacy'     = @{ zh = '可複製資訊不包含 Task 內容、Session ID、專案路徑、登入資料或 Token。'; en = 'Copied diagnostics omit task content, session IDs, project paths, sign-in data, and tokens.' }
    'diag_version'            = @{ zh = '版本'; en = 'Version' }
    'diag_uptime'             = @{ zh = '已執行時間'; en = 'Uptime' }
    'diag_service'            = @{ zh = '背景服務'; en = 'Service' }
    'diag_connection'         = @{ zh = 'Codex app-server'; en = 'Codex app-server' }
    'diag_monitoring'         = @{ zh = 'Task／額度監控'; en = 'Task / quota monitoring' }
    'diag_auto_resume'        = @{ zh = '自動 Resume'; en = 'Auto Resume' }
    'diag_last_scan'          = @{ zh = '最近 Task 掃描'; en = 'Last task scan' }
    'diag_task_count'         = @{ zh = '目前恢復 Task'; en = 'Recovery tasks' }
    'diag_usage_source'       = @{ zh = '額度資料來源'; en = 'Quota source' }
    'diag_usage_refresh'      = @{ zh = '最近有效額度'; en = 'Last valid quota' }
    'diag_usage_attempt'      = @{ zh = '最近額度嘗試'; en = 'Last quota attempt' }
    'diag_next_refresh'       = @{ zh = '下次額度更新'; en = 'Next quota refresh' }
    'diag_next_resume'        = @{ zh = '下次 Resume'; en = 'Next Resume' }
    'diag_logs'               = @{ zh = 'Log 限制'; en = 'Log limit' }
    'diag_recent_error'       = @{ zh = '最近錯誤摘要'; en = 'Recent error summary' }
    'diag_debug'              = @{ zh = 'Debug 模式'; en = 'Debug mode' }
    'diag_none'               = @{ zh = '無'; en = 'None' }
    'diag_enabled'            = @{ zh = '啟用'; en = 'Enabled' }
    'diag_disabled'           = @{ zh = '關閉'; en = 'Off' }
    'diag_connected'          = @{ zh = '已連線'; en = 'Connected' }
    'diag_waiting_codex'      = @{ zh = '等待 Codex 接入'; en = 'Waiting for Codex' }
    'diag_codex_stopped'      = @{ zh = 'Codex 未執行'; en = 'Codex not running' }
    'diag_running'            = @{ zh = '執行中'; en = 'Running' }
    'diag_paused'             = @{ zh = '已暫停'; en = 'Paused' }
    'diag_unknown'            = @{ zh = '未知'; en = 'Unknown' }
    'diag_log_policy'         = @{ zh = '每檔 5 MB，共 3 檔（約 15 MB）'; en = '5 MB each, 3 files total (~15 MB)' }
    'btn_diag_refresh'        = @{ zh = '立即更新'; en = 'Refresh Now' }
    'btn_open_logs'           = @{ zh = '開啟 Log 資料夾'; en = 'Open Log Folder' }
    'btn_copy_diagnostics'    = @{ zh = '複製診斷資訊'; en = 'Copy Diagnostics' }
    'btn_enable_debug'        = @{ zh = '啟用 Debug 30 分鐘'; en = 'Enable Debug for 30 Minutes' }
    'btn_disable_debug'       = @{ zh = '關閉 Debug'; en = 'Disable Debug' }
    'diag_refresh_requested'  = @{ zh = '已要求立即更新額度；畫面會在背景回報後刷新。'; en = 'Quota refresh requested; the page will update after the background response.' }
    'diag_copied'             = @{ zh = '已複製隱私安全的診斷資訊。'; en = 'Privacy-safe diagnostics copied.' }
    'diag_debug_enabled_msg'  = @{ zh = 'Debug 已啟用，30 分鐘後自動關閉。'; en = 'Debug enabled and will turn off automatically after 30 minutes.' }
    'diag_debug_disabled_msg' = @{ zh = 'Debug 已關閉。'; en = 'Debug disabled.' }
    'diag_action_failed'      = @{ zh = '診斷操作失敗。'; en = 'Diagnostic action failed.' }
    'check_shared'            = @{ zh = '啟用 Codex自動續跑助手'; en = 'Enable Codex Auto-Resume Assistant' }
    'shared_description'      = @{ zh = '關閉時仍可查看額度；啟用後會嘗試讓中斷的原 Task 自動繼續。'; en = 'Quota remains available when off; when enabled, interrupted tasks can auto-resume.' }
    'shared_port_prefix'      = @{ zh = '目前本機連接埠：'; en = 'Local Port: ' }
    'check_cli_fallback'      = @{ zh = '允許背景 CLI 備援續跑'; en = 'Allow Background CLI Fallback' }
    'cli_fallback_description'= @{ zh = '僅在原地 IPC／後台無法使用時啟動；會在背景 CLI 繼續同一 Session，不會回到原 Codex 視窗。'; en = 'Used only when the in-place route is unavailable. It resumes the same session in a background CLI, not the original Codex window.' }
    'check_notifications'     = @{ zh = '顯示 Rate Limit、恢復與錯誤等重要通知'; en = 'Show important rate-limit, recovery, and error notifications' }
    'label_prompt'            = @{ zh = '備用重試文字'; en = 'Fallback Retry Prompt' }
    'label_recovery'          = @{ zh = '本次故障恢復上限'; en = 'Outage Recovery Limit' }
    'label_consecutive'       = @{ zh = '連續無進展重試上限'; en = 'No-Progress Limit' }
    'label_memory'            = @{ zh = '記憶體保護上限（MB）'; en = 'Memory Limit (MB)' }
    'label_resume_interval'   = @{ zh = '啟動間隔（秒）'; en = 'Start gap (s)' }
    'label_usage_refresh'     = @{ zh = '額度更新（分鐘）'; en = 'Quota Refresh (min)' }
    'label_strategy'          = @{ zh = '等待策略'; en = 'Wait Strategy' }
    'strategy_exponential'    = @{ zh = '倍增等待'; en = 'Exponential' }
    'strategy_linear'         = @{ zh = '等差遞增'; en = 'Linear' }
    'strategy_fixed'          = @{ zh = '固定間隔'; en = 'Fixed Interval' }
    'label_initial_delay'     = @{ zh = '首次等待（秒）'; en = 'Initial Wait (s)' }
    'label_fixed_interval'    = @{ zh = '固定間隔（秒）'; en = 'Fixed Interval (s)' }
    'label_max_delay'         = @{ zh = '最大等待（秒）'; en = 'Max Wait (s)' }
    'label_increment'         = @{ zh = '每次增加（秒）'; en = 'Increment (s)' }
    'wait_seq_prefix'         = @{ zh = '等待序列：'; en = 'Wait Sequence: ' }
    'unit_second'             = @{ zh = ' 秒'; en = 's' }
    'unit_minute'             = @{ zh = ' 分鐘'; en = 'm' }
    'unit_hour'               = @{ zh = ' 小時'; en = 'h' }
    'btn_save'                = @{ zh = '儲存設定'; en = 'Save Settings' }
    'btn_close'               = @{ zh = '關閉'; en = 'Close' }
    'busy_checking'           = @{ zh = '檢查中…'; en = 'Checking...' }
    'busy_notice'             = @{ zh = '正在執行設定檢查，請稍候…'; en = 'Checking settings, please wait...' }
    'save_saved'              = @{ zh = '設定已儲存，將在下一次掃描時生效。'; en = 'Settings saved; will take effect on next scan.' }
    'save_checking_health'    = @{ zh = '正在執行 Codex自動續跑助手健康檢查，Codex 仍維持官方後台…'; en = 'Checking Codex Auto-Resume Assistant; Codex remains on its official backend...' }
    'save_closing_shared'     = @{ zh = '正在關閉 Codex自動續跑助手並恢復官方後台…'; en = 'Disabling Codex Auto-Resume Assistant and reverting to the official backend...' }
    'save_saving'             = @{ zh = '正在儲存設定…'; en = 'Saving settings...' }
    'save_timeout'            = @{ zh = '設定檢查逾時，Codex 後台未切換，設定未儲存。'; en = 'Settings check timed out; Codex backend not changed, settings not saved.' }
    'save_validation_failed'  = @{ zh = '設定驗證失敗'; en = 'Settings validation failed' }
    'save_fail_reserved'      = @{ zh = '儲存失敗：連接埠 {0} 被 Windows 保留，Codex自動續跑助手未啟用。'; en = 'Save failed: Port {0} is reserved; Codex Auto-Resume Assistant was not enabled.' }
    'save_fail_conflict'      = @{ zh = '儲存失敗：連接埠 {0} 被其他程式占用，Codex自動續跑助手未啟用。'; en = 'Save failed: Port {0} is in use; Codex Auto-Resume Assistant was not enabled.' }
    'save_fail_timeout'       = @{ zh = '儲存逾時：Codex自動續跑助手健康檢查未完成，Codex 仍使用官方後台。'; en = 'Save timed out: Health check did not finish; Codex remains on its official backend.' }
    'save_fail_health'        = @{ zh = '儲存失敗：Codex自動續跑助手健康檢查未通過，Codex 仍使用官方後台。'; en = 'Save failed: Health check did not pass; Codex remains on its official backend.' }
    'save_fail_close'         = @{ zh = '儲存失敗：Codex自動續跑助手未能關閉，設定未完成。'; en = 'Save failed: Codex Auto-Resume Assistant could not be disabled.' }
    'save_fail_range'         = @{ zh = '儲存失敗，請檢查設定範圍。'; en = 'Save failed: please verify settings range.' }
    'msg_prompt_empty'        = @{ zh = '備用重試文字不能為空白。'; en = 'Fallback retry prompt cannot be empty.' }
    'msg_max_less_initial'    = @{ zh = '最大等待時間不能小於首次等待時間。'; en = 'Maximum wait time cannot be less than initial wait time.' }
    'msg_action_failed'       = @{ zh = '續跑指令未送出。請重新整理清單，並確認續跑通道已就緒。'; en = 'Resume request was not sent. Refresh tasks and check the resume channel.' }
    'msg_resume_channel_unavailable' = @{ zh = '續跑通道尚未就緒。請檢查設定與診斷，再重新整理清單。'; en = 'The resume channel is not ready. Check Settings and Diagnostics, then refresh the task list.' }
    'memory_guard_msg'        = @{ zh = '設定視窗私有記憶體已達到 {0} MB，超過上限 {1} MB。視窗將關閉，Codex Task 資料不會刪除。'; en = 'Settings window memory reached {0} MB, exceeding limit of {1} MB. Window will close; Codex task data is preserved.' }
    'memory_guard_title'      = @{ zh = 'CodexWorkResumer 記憶體保護'; en = 'CodexWorkResumer Memory Guard' }
    'layout_overlap_err'      = @{ zh = '設定版面發生遮擋：'; en = 'Layout overlap detected: ' }
    'layout_bounds_err'       = @{ zh = '設定輸入欄位超出可見區域。'; en = 'Control bounds exceed visible area.' }
    'config_read_err'         = @{ zh = '無法讀取自動重試設定。'; en = 'Cannot read auto-retry settings.' }
}

function T($Key) {
    $item = $script:i18n[$Key]
    if ($item) {
        $val = $item[$script:currentLanguage]
        if ($val) { return $val }
    }
    return $Key
}

function Stop-LocalCommandProcess {
    param($Process)
    if ($null -eq $Process) { return }
    try {
        if ($Process.HasExited) { return }
    } catch { return }
    try {
        $killer = Start-Process -FilePath 'taskkill.exe' -ArgumentList @('/PID', [string]$Process.Id, '/T', '/F') -WindowStyle Hidden -Wait -PassThru
        if ($killer) { $killer.Dispose() }
    } catch { }
    try {
        if (-not $Process.HasExited) { $Process.Kill() }
    } catch { }
}

function Start-LocalCommand {
    param(
        [string]$Mode,
        [hashtable]$Environment,
        [int]$TimeoutMilliseconds = $localCommandTimeoutMilliseconds
    )
    $script:localCommandTimedOut = $false
    $info = [System.Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $Executable
    $info.Arguments = $Mode
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.EnvironmentVariables['CODEX_AUTO_RETRY_DATA_DIR'] = $DataDir
    foreach ($entry in $Environment.GetEnumerator()) {
        $info.EnvironmentVariables[[string]$entry.Key] = [string]$entry.Value
    }
    $process = $null
    try {
        $process = [System.Diagnostics.Process]::Start($info)
        $script:localCommandProcess = $process
        $deadline = [DateTimeOffset]::UtcNow.AddMilliseconds($TimeoutMilliseconds)
        while (-not $process.HasExited) {
            [System.Windows.Forms.Application]::DoEvents()
            Start-Sleep -Milliseconds 50
            if ([DateTimeOffset]::UtcNow -ge $deadline) {
                $script:localCommandTimedOut = $true
                Stop-LocalCommandProcess $process
                return -2
            }
        }
        return $process.ExitCode
    } catch {
        return -1
    } finally {
        $script:localCommandProcess = $null
        if ($process) { $process.Dispose() }
    }
}

function Get-StartupApprovalStatus {
    $key = $null
    try {
        $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey(
            'Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\Run',
            $false
        )
        if ($null -eq $key) { return 'unknown' }
        $bytes = $key.GetValue('CodexAutoRetry', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        if ($bytes -isnot [byte[]] -or $bytes.Length -lt 4 -or $bytes[1] -ne 0 -or $bytes[2] -ne 0 -or $bytes[3] -ne 0) { return 'unknown' }
        if ($bytes[0] -eq 2) { return 'enabled' }
        if ($bytes[0] -eq 3) { return 'disabled' }
        return 'unknown'
    } catch {
        return 'unknown'
    } finally {
        if ($key) { $key.Close() }
    }
}

function ConvertTo-ProcessArgument {
    param([string]$Value)
    if ($Value -notmatch '[\s"]') { return $Value }
    return '"' + $Value.Replace('"', '\"') + '"'
}

function Start-SafeCodexLaunch {
    if ($script:localCommandInProgress) { return }
    $currentConfig = Read-JsonFile $configPath
    if (-not (Get-SharedModeRequested $currentConfig)) {
        $noticeLabel.Text = T 'safe_launch_disabled'
        $noticeLabel.ForeColor = [System.Drawing.Color]::DarkOrange
        return
    }
    if (-not (Test-Path -LiteralPath $safeLauncherPath -PathType Leaf)) {
        $noticeLabel.Text = T 'safe_launch_missing'
        $noticeLabel.ForeColor = [System.Drawing.Color]::Firebrick
        return
    }
    $powershellPath = Join-Path $env:WINDIR 'System32\WindowsPowerShell\v1.0\powershell.exe'
    if (-not (Test-Path -LiteralPath $powershellPath -PathType Leaf)) { $powershellPath = 'powershell.exe' }
    $arguments = @(
        '-NoLogo', '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass',
        '-File', (ConvertTo-ProcessArgument $safeLauncherPath),
        '-DataDir', (ConvertTo-ProcessArgument $DataDir),
        '-WaitForExitSeconds', '120'
    ) -join ' '
    $script:localCommandInProgress = $true
    $script:safeLaunchInProgress = $true
    Set-SettingsBusy $true
    $noticeLabel.Text = T 'safe_launch_running'
    $noticeLabel.ForeColor = [System.Drawing.Color]::DarkOrange
    try {
        $script:localCommandProcess = Start-Process -FilePath $powershellPath -ArgumentList $arguments -WindowStyle Hidden -PassThru
    } catch {
        $script:localCommandProcess = $null
        $script:safeLaunchInProgress = $false
        $script:localCommandInProgress = $false
        Set-SettingsBusy $false
        $noticeLabel.Text = T 'safe_launch_failed'
        $noticeLabel.ForeColor = [System.Drawing.Color]::Firebrick
    }
}

function Complete-SafeCodexLaunch {
    if (-not $script:safeLaunchInProgress -or -not $script:localCommandProcess) { return }
    $process = $script:localCommandProcess
    try {
        if (-not $process.HasExited) { return }
        $exitCode = $process.ExitCode
    } catch {
        $exitCode = 1
    }
    $process.Dispose()
    $script:localCommandProcess = $null
    $script:safeLaunchInProgress = $false
    $script:localCommandInProgress = $false
    Set-SettingsBusy $false
    if ($exitCode -eq 0) {
        $noticeLabel.Text = T 'safe_launch_done'
        $noticeLabel.ForeColor = [System.Drawing.Color]::SeaGreen
    } else {
        $noticeLabel.Text = T 'safe_launch_failed'
        $noticeLabel.ForeColor = [System.Drawing.Color]::Firebrick
    }
    Update-RuntimeView
}

function New-Label {
    param([string]$Text, [int]$X, [int]$Y, [int]$Width, [int]$Height)
    $label = [System.Windows.Forms.Label]::new()
    $label.Text = $Text
    $label.Location = [System.Drawing.Point]::new($X, $Y)
    $label.Size = [System.Drawing.Size]::new($Width, $Height)
    return $label
}

function New-NumberBox {
    param([int]$X, [int]$Y, [int]$Minimum, [int]$Maximum, [int]$Value, [int]$Width = 100)
    $box = [System.Windows.Forms.NumericUpDown]::new()
    $box.Location = [System.Drawing.Point]::new($X, $Y)
    $box.Size = [System.Drawing.Size]::new($Width, 24)
    $box.Minimum = $Minimum
    $box.Maximum = $Maximum
    $box.Value = [Math]::Min($Maximum, [Math]::Max($Minimum, $Value))
    return $box
}

function New-QuotaBar {
    param([int]$Y)
    # Windows visual styles animate a travelling highlight on native
    # ProgressBar controls. Plain panels keep the quota indicator static.
    $track = [System.Windows.Forms.Panel]::new()
    $track.Location = [System.Drawing.Point]::new(60, $Y)
    $track.Size = [System.Drawing.Size]::new(420, 18)
    $track.BorderStyle = 'FixedSingle'
    $track.BackColor = [System.Drawing.ColorTranslator]::FromHtml('#DEE5ED')
    $fill = [System.Windows.Forms.Panel]::new()
    $fill.Location = [System.Drawing.Point]::Empty
    $fill.Size = [System.Drawing.Size]::new(0, $track.ClientSize.Height)
    $fill.BackColor = [System.Drawing.ColorTranslator]::FromHtml('#9AA0A6')
    $track.Controls.Add($fill)
    return $track
}

$config = Read-JsonFile $configPath
$control = Read-JsonFile $controlPath

function Test-WindowsDarkTheme {
    $key = $null
    try {
        $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Software\Microsoft\Windows\CurrentVersion\Themes\Personalize', $false)
        if (-not $key) { return $false }
        return [int]$key.GetValue('AppsUseLightTheme', 1) -eq 0
    } catch {
        return $false
    } finally {
        if ($key) { $key.Dispose() }
    }
}

function Apply-AppTheme {
    $dark = Test-WindowsDarkTheme
    $script:lastThemeCheck = [DateTimeOffset]::UtcNow
    if ($null -ne $script:darkTheme -and $script:darkTheme -eq $dark) { return }
    $script:darkTheme = $dark
    $background = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#111418') } else { [System.Drawing.ColorTranslator]::FromHtml('#F3F6FA') }
    $surface = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#1B2027') } else { [System.Drawing.Color]::White }
    $input = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#0F1318') } else { [System.Drawing.Color]::White }
    $text = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#F3F6FA') } else { [System.Drawing.ColorTranslator]::FromHtml('#1D2733') }
    $muted = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#AAB5C2') } else { [System.Drawing.ColorTranslator]::FromHtml('#5F6B78') }
    $accent = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#70B7FF') } else { [System.Drawing.ColorTranslator]::FromHtml('#075EAA') }
    $primary = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#1676D2') } else { [System.Drawing.ColorTranslator]::FromHtml('#0969B8') }
    $secondary = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#303946') } else { [System.Drawing.ColorTranslator]::FromHtml('#E5EDF6') }
    $border = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#607083') } else { [System.Drawing.ColorTranslator]::FromHtml('#8091A3') }
    $form.BackColor = $background
    $form.ForeColor = $text
    $stack = [System.Collections.Generic.Stack[System.Windows.Forms.Control]]::new()
    $stack.Push($form)
    while ($stack.Count -gt 0) {
        $parent = $stack.Pop()
        foreach ($child in $parent.Controls) {
            $stack.Push($child)
            if ($child -is [System.Windows.Forms.TabPage] -or $child -is [System.Windows.Forms.GroupBox]) {
                $child.BackColor = $surface
                $child.ForeColor = $text
            } elseif ($child -is [System.Windows.Forms.TextBox] -or $child -is [System.Windows.Forms.NumericUpDown] -or
                $child -is [System.Windows.Forms.ComboBox] -or $child -is [System.Windows.Forms.ListView]) {
                $child.BackColor = $input
                $child.ForeColor = $text
            } elseif ($child -is [System.Windows.Forms.Button]) {
                $child.UseVisualStyleBackColor = $false
                $child.FlatStyle = 'Flat'
                $child.FlatAppearance.BorderSize = 1
                $child.FlatAppearance.BorderColor = $border
                $child.Cursor = [System.Windows.Forms.Cursors]::Hand
                if ([string]$child.Tag -eq 'primary') {
                    $child.BackColor = $primary
                    $child.ForeColor = [System.Drawing.Color]::White
                    $child.FlatAppearance.BorderColor = $primary
                    $child.Font = [System.Drawing.Font]::new('Microsoft YaHei UI', 9, [System.Drawing.FontStyle]::Bold)
                } else {
                    $child.BackColor = $secondary
                    $child.ForeColor = $text
                }
            } elseif ($child -is [System.Windows.Forms.Label] -or $child -is [System.Windows.Forms.CheckBox]) {
                $child.BackColor = [System.Drawing.Color]::Transparent
                $child.ForeColor = $text
            }
        }
    }
    # Keep the watchlist visually primary without changing the existing
    # ListView's platform-native selection and checkbox behavior.
    $queueGroup.BackColor = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#1B292F') } else { [System.Drawing.ColorTranslator]::FromHtml('#EEF6F8') }
    $quotaTrack = if ($dark) { [System.Drawing.ColorTranslator]::FromHtml('#303946') } else { [System.Drawing.ColorTranslator]::FromHtml('#DEE5ED') }
    $usage5Progress.BackColor = $quotaTrack
    $usage7Progress.BackColor = $quotaTrack
    $scanValue.ForeColor = $muted
    $schedulerScope.ForeColor = $muted
    $monitorNote.ForeColor = $muted
    $historySummary.ForeColor = $muted
    $taskActionHint.ForeColor = $muted
    $sharedPortValue.ForeColor = $muted
    $sharedDescription.ForeColor = $muted
    $previewLabel.ForeColor = $muted
    $guideSource.ForeColor = $muted
    $guideScheduler.ForeColor = $muted
    $guideTaskOutcomes.ForeColor = $muted
    $diagnosticsPrivacy.ForeColor = $muted
    $title.ForeColor = $accent
    $guideTitle.ForeColor = $accent
    $diagnosticsTitle.ForeColor = $accent
    $tabControl.Invalidate()
    $guideList.Tag = $null
    Update-RuntimeView
}

$runtimeStatus = Read-JsonFile $statusPath
if (-not $config) {
    [System.Windows.Forms.MessageBox]::Show((T 'config_read_err'), 'CodexWorkResumer', 'OK', 'Error') | Out-Null
    exit 1
}

$form = [System.Windows.Forms.Form]::new()
$form.Text = T 'form_title'
$form.StartPosition = 'CenterScreen'
$form.FormBorderStyle = 'Sizable'
$form.MaximizeBox = $true
$form.MinimizeBox = $true
$form.ClientSize = [System.Drawing.Size]::new(1000, 720)
$form.MinimumSize = [System.Drawing.Size]::new(1016, 520)
$form.Font = [System.Drawing.Font]::new('Microsoft YaHei UI', 9)
$form.Icon = [System.Drawing.SystemIcons]::Information
if ($SmokeTest) {
    $form.Opacity = 0
    $form.ShowInTaskbar = $false
}

$title = New-Label 'CodexWorkResumer' 22 18 340 30
$title.Font = [System.Drawing.Font]::new('Microsoft YaHei UI', 15, [System.Drawing.FontStyle]::Bold)
$form.Controls.Add($title)

$versionText = ''
if ($runtimeStatus -and $runtimeStatus.version) {
    $versionText = 'v' + [string]$runtimeStatus.version
    if ($runtimeStatus.PSObject.Properties['build'] -and [int]$runtimeStatus.build -gt 0) { $versionText += '  b' + [int]$runtimeStatus.build }
}
$versionLabel = New-Label $versionText 745 22 130 22
$versionLabel.TextAlign = 'MiddleRight'
$versionLabel.ForeColor = [System.Drawing.Color]::DimGray
$versionLabel.Anchor = 'Top,Right'
$form.Controls.Add($versionLabel)

$langButton = [System.Windows.Forms.Button]::new()
$langButton.Location = [System.Drawing.Point]::new(885, 18)
$langButton.Size = [System.Drawing.Size]::new(95, 26)
$langButton.Text = T 'lang_button'
$langButton.Font = [System.Drawing.Font]::new('Microsoft YaHei UI', 8.5)
$langButton.FlatStyle = 'Standard'
$langButton.Cursor = [System.Windows.Forms.Cursors]::Hand
$langButton.Tag = 'secondary'
$langButton.Anchor = 'Top,Right'
$form.Controls.Add($langButton)

$tabControl = [System.Windows.Forms.TabControl]::new()
$tabControl.Location = [System.Drawing.Point]::new(20, 58)
$tabControl.Size = [System.Drawing.Size]::new(960, 640)
$tabControl.Anchor = 'Top,Bottom,Left,Right'
$overviewPage = [System.Windows.Forms.TabPage]::new('總覽')
$overviewPage.AutoScroll = $true
$overviewPage.AutoScrollMinSize = [System.Drawing.Size]::new(0, 930)
$settingsPage = [System.Windows.Forms.TabPage]::new('設定')
$settingsPage.AutoScroll = $true
$settingsPage.AutoScrollMinSize = [System.Drawing.Size]::new(0, 535)
$guidePage = [System.Windows.Forms.TabPage]::new((T 'guide_tab'))
$guidePage.AutoScroll = $true
$guidePage.AutoScrollMinSize = [System.Drawing.Size]::new(0, 640)
$diagnosticsPage = [System.Windows.Forms.TabPage]::new((T 'diagnostics_tab'))
$diagnosticsPage.AutoScroll = $true
$diagnosticsPage.AutoScrollMinSize = [System.Drawing.Size]::new(0, 550)
[void]$tabControl.TabPages.Add($overviewPage)
[void]$tabControl.TabPages.Add($settingsPage)
[void]$tabControl.TabPages.Add($guidePage)
[void]$tabControl.TabPages.Add($diagnosticsPage)
$form.Controls.Add($tabControl)

$queueGroup = [System.Windows.Forms.GroupBox]::new()
$queueGroup.Text = T 'section_queue'
$queueGroup.Location = [System.Drawing.Point]::new(14, 312)
$queueGroup.Size = [System.Drawing.Size]::new(920, 348)
$overviewPage.Controls.Add($queueGroup)

$schedulerGroup = [System.Windows.Forms.GroupBox]::new()
$schedulerGroup.Text = T 'section_scheduler'
$schedulerGroup.Location = [System.Drawing.Point]::new(14, 670)
$schedulerGroup.Size = [System.Drawing.Size]::new(920, 160)
$overviewPage.Controls.Add($schedulerGroup)

$monitorGroup = [System.Windows.Forms.GroupBox]::new()
$monitorGroup.Text = T 'section_monitor'
$monitorGroup.Location = [System.Drawing.Point]::new(14, 10)
$monitorGroup.Size = [System.Drawing.Size]::new(920, 292)
$overviewPage.Controls.Add($monitorGroup)

$historyGroup = [System.Windows.Forms.GroupBox]::new()
$historyGroup.Text = T 'section_history'
$historyGroup.Location = [System.Drawing.Point]::new(14, 840)
$historyGroup.Size = [System.Drawing.Size]::new(920, 72)
$overviewPage.Controls.Add($historyGroup)

$statusGroup = [System.Windows.Forms.GroupBox]::new()
$statusGroup.Text = T 'status_group'
$statusGroup.Location = [System.Drawing.Point]::new(12, 49)
$statusGroup.Size = [System.Drawing.Size]::new(894, 118)
$monitorGroup.Controls.Add($statusGroup)
$serviceValue = New-Label (T 'status_loading') 18 24 870 24
$serviceValue.Font = [System.Drawing.Font]::new('Microsoft YaHei UI', 10, [System.Drawing.FontStyle]::Bold)
$serviceValue.AutoEllipsis = $true
$queueValue = New-Label ([string]::Format((T 'queue_summary'), '--', '--', '--')) 18 52 410 24
$nextValue = New-Label (T 'next_none') 500 52 388 24
$scanValue = New-Label '' 18 82 870 22
$scanValue.ForeColor = [System.Drawing.Color]::DimGray
$statusGroup.Controls.AddRange(@($serviceValue, $queueValue, $nextValue, $scanValue))

$usageGroup = [System.Windows.Forms.GroupBox]::new()
$usageGroup.Text = T 'usage_group'
$usageGroup.Location = [System.Drawing.Point]::new(12, 168)
$usageGroup.Size = [System.Drawing.Size]::new(894, 112)
$monitorGroup.Controls.Add($usageGroup)
$usage5Label = New-Label '5H' 18 28 36 24
$usage5Progress = New-QuotaBar 30
$usage5Remaining = New-Label (T 'usage_unavailable') 495 27 150 24
$usage5Reset = New-Label (T 'usage_unavailable') 660 27 210 24
$usage7Label = New-Label '7D' 18 68 36 24
$usage7Progress = New-QuotaBar 70
$usage7Remaining = New-Label (T 'usage_unavailable') 495 67 150 24
$usage7Reset = New-Label (T 'usage_unavailable') 660 67 210 24
$usageToolTip = [System.Windows.Forms.ToolTip]::new()
$usageRefreshingLabel = New-Label '' 810 0 65 20
$usageRefreshingLabel.TextAlign = 'MiddleRight'
$usageRefreshingLabel.ForeColor = [System.Drawing.Color]::DimGray
$usageGroup.Controls.AddRange(@($usage5Label, $usage5Progress, $usage5Remaining, $usage5Reset, $usage7Label, $usage7Progress, $usage7Remaining, $usage7Reset, $usageRefreshingLabel))

$taskList = [System.Windows.Forms.ListView]::new()
$taskList.Location = [System.Drawing.Point]::new(14, 25)
$taskList.Size = [System.Drawing.Size]::new(890, 218)
$taskList.View = 'Details'
$taskList.FullRowSelect = $true
$taskList.GridLines = $true
$taskList.HideSelection = $false
$taskList.CheckBoxes = $true
$taskList.MultiSelect = $true
$taskList.AllowDrop = $true
$taskList.ShowItemToolTips = $true
[void]$taskList.Columns.Add((T 'col_task'), 150)
[void]$taskList.Columns.Add((T 'col_project'), 270)
[void]$taskList.Columns.Add((T 'col_task_type'), 60)
[void]$taskList.Columns.Add((T 'col_status'), 280)
[void]$taskList.Columns.Add((T 'col_last_activity'), 120)
[void]$taskList.Columns.Add((T 'col_countdown'), 80)
[void]$taskList.Columns.Add((T 'col_class'), 100)
$queueGroup.Controls.Add($taskList)
$taskSelectionHint = New-Label (T 'task_selection_hint') 14 247 890 22
$taskSelectionHint.ForeColor = [System.Drawing.Color]::DimGray
$queueGroup.Controls.Add($taskSelectionHint)
$retryNowButton = [System.Windows.Forms.Button]::new()
$retryNowButton.Text = T 'btn_retry_now'
$refreshTasksButton = [System.Windows.Forms.Button]::new()
$refreshTasksButton.Text = T 'btn_refresh_tasks'
$refreshTasksButton.Location = [System.Drawing.Point]::new(14, 274)
$refreshTasksButton.Size = [System.Drawing.Size]::new(100, 29)
$refreshTasksButton.Tag = 'secondary'
$autoScanCheck = [System.Windows.Forms.CheckBox]::new()
$autoScanCheck.Text = T 'check_auto_scan'
$autoScanCheck.Location = [System.Drawing.Point]::new(126, 277)
$autoScanCheck.Size = [System.Drawing.Size]::new(105, 24)
$autoScanCheck.Checked = if ($config.PSObject.Properties['auto_scan_enabled']) { [bool]$config.auto_scan_enabled } else { $true }
$scanEveryLabel = New-Label (T 'label_every') 235 278 28 22
$scanIntervalBox = New-NumberBox 266 275 1 3600 $(if ($config.PSObject.Properties['scan_interval_seconds']) { [int]$config.scan_interval_seconds } else { 60 }) 58
$scanSecondsLabel = New-Label (T 'label_seconds') 330 278 50 22
$retryNowButton.Location = [System.Drawing.Point]::new(492, 274)
$retryNowButton.Size = [System.Drawing.Size]::new(160, 29)
$retryNowButton.Tag = 'primary'
$cancelRetryButton = [System.Windows.Forms.Button]::new()
$cancelRetryButton.Text = T 'btn_cancel_retry'
$cancelRetryButton.Location = [System.Drawing.Point]::new(659, 274)
$cancelRetryButton.Size = [System.Drawing.Size]::new(126, 29)
$cancelRetryButton.Tag = 'secondary'
$restartRetryButton = [System.Windows.Forms.Button]::new()
$restartRetryButton.Text = T 'btn_restart_retry'
$restartRetryButton.Location = [System.Drawing.Point]::new(792, 274)
$restartRetryButton.Size = [System.Drawing.Size]::new(112, 29)
$restartRetryButton.Tag = 'secondary'
$queueGroup.Controls.AddRange(@($refreshTasksButton, $autoScanCheck, $scanEveryLabel, $scanIntervalBox, $scanSecondsLabel, $retryNowButton, $cancelRetryButton, $restartRetryButton))
$taskActionHint = New-Label (T 'action_hint_select') 14 311 890 22
$taskActionHint.ForeColor = [System.Drawing.Color]::DimGray
$queueGroup.Controls.Add($taskActionHint)
$taskActionToolTip = [System.Windows.Forms.ToolTip]::new()
$taskActionToolTip.SetToolTip($retryNowButton, (T 'action_hint_select'))
$taskActionToolTip.SetToolTip($cancelRetryButton, (T 'action_hint_select'))

$schedulerIntro = New-Label (T 'scheduler_intro') 18 23 875 22
$schedulerSequential = [System.Windows.Forms.RadioButton]::new()
$schedulerSequential.Text = T 'mode_sequential'
$schedulerSequential.Location = [System.Drawing.Point]::new(18, 49)
$schedulerSequential.Size = [System.Drawing.Size]::new(148, 26)
$schedulerStaggered = [System.Windows.Forms.RadioButton]::new()
$schedulerStaggered.Text = T 'mode_staggered'
$schedulerStaggered.Location = [System.Drawing.Point]::new(177, 49)
$schedulerStaggered.Size = [System.Drawing.Size]::new(148, 26)
$schedulerMode = if ($config.PSObject.Properties['quota_resume_mode']) { [string]$config.quota_resume_mode } else { 'staggered_start' }
$schedulerSequential.Checked = $schedulerMode -eq 'sequential_completion'
$schedulerStaggered.Checked = -not $schedulerSequential.Checked
$schedulerSaveButton = [System.Windows.Forms.Button]::new()
$schedulerSaveButton.Text = T 'scheduler_save'
$schedulerSaveButton.Location = [System.Drawing.Point]::new(750, 48)
$schedulerSaveButton.Size = [System.Drawing.Size]::new(145, 29)
$schedulerSaveButton.Tag = 'primary'
$schedulerSaveNotice = New-Label '' 341 52 398 24
$schedulerSaveNotice.AutoEllipsis = $true
$parallelLimitLabel = New-Label (T 'parallel_limit') 320 92 151 22
$parallelLimitBox = New-NumberBox 475 89 1 16 $(if ($config.PSObject.Properties['max_parallel_sessions'] -and [int]$config.max_parallel_sessions -gt 0) { [int]$config.max_parallel_sessions } else { 2 }) 65
$schedulerScope = New-Label (T 'scheduler_scope') 18 115 875 40
$schedulerScope.ForeColor = [System.Drawing.Color]::DimGray
$schedulerGroup.Controls.AddRange(@($schedulerIntro, $schedulerSequential, $schedulerStaggered, $schedulerSaveButton, $schedulerSaveNotice, $parallelLimitLabel, $parallelLimitBox, $schedulerScope))

$monitorNote = New-Label (T 'monitor_control_note') 18 23 685 23
$monitorNote.ForeColor = [System.Drawing.Color]::DimGray
$autoDispatchButton = [System.Windows.Forms.Button]::new()
$autoDispatchButton.Text = T 'pause_dispatch'
$autoDispatchButton.Location = [System.Drawing.Point]::new(746, 18)
$autoDispatchButton.Size = [System.Drawing.Size]::new(150, 27)
$autoDispatchButton.Tag = 'secondary'
$monitorGroup.Controls.AddRange(@($monitorNote, $autoDispatchButton))

$historySummary = New-Label (T 'history_summary') 18 25 745 25
$historySummary.ForeColor = [System.Drawing.Color]::DimGray
$historyExpandButton = [System.Windows.Forms.Button]::new()
$historyExpandButton.Text = T 'history_expand'
$historyExpandButton.Location = [System.Drawing.Point]::new(797, 21)
$historyExpandButton.Size = [System.Drawing.Size]::new(98, 27)
$historyExpandButton.Tag = 'secondary'
$historyError = New-Label (T 'history_none') 18 64 875 38
$historyError.AutoEllipsis = $true
$historyError.Visible = $false
$historyErrorToolTip = [System.Windows.Forms.ToolTip]::new()
$historyOpenLogs = [System.Windows.Forms.Button]::new()
$historyOpenLogs.Text = T 'btn_open_logs'
$historyOpenLogs.Location = [System.Drawing.Point]::new(18, 113)
$historyOpenLogs.Size = [System.Drawing.Size]::new(165, 30)
$historyOpenLogs.Tag = 'secondary'
$historyOpenLogs.Visible = $false
$historyGroup.Controls.AddRange(@($historySummary, $historyExpandButton, $historyError, $historyOpenLogs))

$settingsGroup = [System.Windows.Forms.GroupBox]::new()
$settingsGroup.Text = T 'settings_group'
$settingsGroup.Location = [System.Drawing.Point]::new(14, 10)
$settingsGroup.Size = [System.Drawing.Size]::new(920, 455)
$settingsPage.Controls.Add($settingsGroup)

$enabledCheck = [System.Windows.Forms.CheckBox]::new()
$enabledCheck.Text = T 'check_enabled'
$enabledCheck.Location = [System.Drawing.Point]::new(18, 25)
$enabledCheck.Size = [System.Drawing.Size]::new(160, 24)
$enabledCheck.Checked = -not [bool]$control.paused
$monitoringCheck = [System.Windows.Forms.CheckBox]::new()
$monitoringCheck.Text = T 'check_monitoring'
$monitoringCheck.Location = [System.Drawing.Point]::new(600, 108)
$monitoringCheck.Size = [System.Drawing.Size]::new(295, 24)
$monitoringCheck.Checked = -not [bool]$control.monitoring_paused
$sharedCheck = [System.Windows.Forms.CheckBox]::new()
$sharedCheck.Text = T 'check_shared'
$sharedCheck.Location = [System.Drawing.Point]::new(18, 52)
$sharedCheck.Size = [System.Drawing.Size]::new(260, 24)
$sharedCheck.Checked = Get-SharedModeRequested $config
$sharedDescription = New-Label (T 'shared_description') 18 78 260 42
$sharedDescription.ForeColor = [System.Drawing.Color]::DimGray
$sharedPortValue = New-Label ((T 'shared_port_prefix') + [int]$config.shared_app_server_port) 300 25 255 24
$sharedPortValue.ForeColor = [System.Drawing.Color]::DimGray
$safeLaunchButton = [System.Windows.Forms.Button]::new()
$safeLaunchButton.Text = T 'btn_safe_launch'
$safeLaunchButton.Location = [System.Drawing.Point]::new(300, 82)
$safeLaunchButton.Size = [System.Drawing.Size]::new(255, 25)
$safeLaunchButton.FlatStyle = 'Standard'
$safeLaunchButton.Cursor = [System.Windows.Forms.Cursors]::Hand
$safeLaunchButton.Tag = 'secondary'
$notificationsCheck = [System.Windows.Forms.CheckBox]::new()
$notificationsCheck.Text = T 'check_notifications'
$notificationsCheck.Location = [System.Drawing.Point]::new(300, 52)
$notificationsCheck.Size = [System.Drawing.Size]::new(255, 24)
$notificationsCheck.Checked = [bool]$config.show_notifications
$cliFallbackCheck = [System.Windows.Forms.CheckBox]::new()
$cliFallbackCheck.Text = T 'check_cli_fallback'
$cliFallbackCheck.Location = [System.Drawing.Point]::new(600, 145)
$cliFallbackCheck.Size = [System.Drawing.Size]::new(295, 24)
$cliFallbackCheck.Checked = [bool]$config.cli_resume_fallback_enabled
$cliFallbackDescription = New-Label (T 'cli_fallback_description') 600 171 295 66
$cliFallbackDescription.ForeColor = [System.Drawing.Color]::DimGray
$settingsGroup.Controls.AddRange(@($enabledCheck, $monitoringCheck, $sharedCheck, $sharedDescription, $sharedPortValue, $safeLaunchButton, $notificationsCheck, $cliFallbackCheck, $cliFallbackDescription))

$guideTitle = New-Label (T 'guide_title') 18 18 500 30
$guideTitle.Font = [System.Drawing.Font]::new('Microsoft YaHei UI', 13, [System.Drawing.FontStyle]::Bold)
$guideSource = New-Label (T 'guide_source') 18 50 870 24
$guideSource.ForeColor = [System.Drawing.Color]::DimGray
$guideScheduler = New-Label (T 'guide_scheduler') 18 74 870 42
$guideScheduler.ForeColor = [System.Drawing.Color]::DimGray
$guideList = [System.Windows.Forms.ListView]::new()
$guideList.Location = [System.Drawing.Point]::new(18, 122)
$guideList.Size = [System.Drawing.Size]::new(900, 430)
$guideList.View = 'Details'
$guideList.FullRowSelect = $true
$guideList.GridLines = $true
$guideList.HideSelection = $false
$guideList.ShowItemToolTips = $true
[void]$guideList.Columns.Add((T 'guide_state'), 100)
[void]$guideList.Columns.Add((T 'guide_color'), 95)
[void]$guideList.Columns.Add((T 'guide_meaning'), 260)
[void]$guideList.Columns.Add((T 'guide_behavior'), 325)
[void]$guideList.Columns.Add((T 'guide_notice'), 110)
$guideTaskOutcomes = New-Label (T 'guide_task_outcomes') 18 558 870 62
$guideTaskOutcomes.ForeColor = [System.Drawing.Color]::DimGray
$guidePage.Controls.AddRange(@($guideTitle, $guideSource, $guideScheduler, $guideList, $guideTaskOutcomes))

$diagnosticsTitle = New-Label (T 'diagnostics_title') 18 18 500 30
$diagnosticsTitle.Font = [System.Drawing.Font]::new('Microsoft YaHei UI', 13, [System.Drawing.FontStyle]::Bold)
$diagnosticsPrivacy = New-Label (T 'diagnostics_privacy') 18 50 870 24
$diagnosticsPrivacy.ForeColor = [System.Drawing.Color]::DimGray
$diagnosticsGroup = [System.Windows.Forms.GroupBox]::new()
$diagnosticsGroup.Location = [System.Drawing.Point]::new(18, 78)
$diagnosticsGroup.Size = [System.Drawing.Size]::new(900, 408)
$diagnosticsPage.Controls.AddRange(@($diagnosticsTitle, $diagnosticsPrivacy, $diagnosticsGroup))

$diagVersionLabel = New-Label (T 'diag_version') 18 24 145 22
$diagVersionValue = New-Label '--' 170 24 250 22
$diagUptimeLabel = New-Label (T 'diag_uptime') 450 24 155 22
$diagUptimeValue = New-Label '--' 612 24 260 22
$diagServiceLabel = New-Label (T 'diag_service') 18 62 145 22
$diagServiceValue = New-Label '--' 170 62 250 22
$diagConnectionLabel = New-Label (T 'diag_connection') 450 62 155 22
$diagConnectionValue = New-Label '--' 612 62 260 22
$diagMonitoringLabel = New-Label (T 'diag_monitoring') 18 100 145 22
$diagMonitoringValue = New-Label '--' 170 100 250 22
$diagAutoResumeLabel = New-Label (T 'diag_auto_resume') 450 100 155 22
$diagAutoResumeValue = New-Label '--' 612 100 260 22
$diagLastScanLabel = New-Label (T 'diag_last_scan') 18 138 145 22
$diagLastScanValue = New-Label '--' 170 138 250 22
$diagTaskCountLabel = New-Label (T 'diag_task_count') 450 138 155 22
$diagTaskCountValue = New-Label '--' 612 138 260 22
$diagUsageSourceLabel = New-Label (T 'diag_usage_source') 18 176 145 22
$diagUsageSourceValue = New-Label '--' 170 176 250 22
$diagUsageRefreshLabel = New-Label (T 'diag_usage_refresh') 450 176 155 22
$diagUsageRefreshValue = New-Label '--' 612 176 260 22
$diagUsageAttemptLabel = New-Label (T 'diag_usage_attempt') 18 214 145 22
$diagUsageAttemptValue = New-Label '--' 170 214 250 22
$diagNextRefreshLabel = New-Label (T 'diag_next_refresh') 450 214 155 22
$diagNextRefreshValue = New-Label '--' 612 214 260 22
$diagNextResumeLabel = New-Label (T 'diag_next_resume') 18 252 145 22
$diagNextResumeValue = New-Label '--' 170 252 250 22
$diagLogsLabel = New-Label (T 'diag_logs') 450 252 155 22
$diagLogsValue = New-Label (T 'diag_log_policy') 612 252 260 44
$diagRecentErrorLabel = New-Label (T 'diag_recent_error') 18 300 145 22
$diagRecentErrorValue = New-Label (T 'diag_none') 170 300 702 22
$diagDebugLabel = New-Label (T 'diag_debug') 18 346 145 22
$diagDebugValue = New-Label (T 'diag_disabled') 170 346 702 22
$diagnosticFieldLabels = @(
    $diagVersionLabel, $diagUptimeLabel, $diagServiceLabel, $diagConnectionLabel,
    $diagMonitoringLabel, $diagAutoResumeLabel, $diagLastScanLabel, $diagTaskCountLabel,
    $diagUsageSourceLabel, $diagUsageRefreshLabel, $diagUsageAttemptLabel, $diagNextRefreshLabel,
    $diagNextResumeLabel, $diagLogsLabel, $diagRecentErrorLabel, $diagDebugLabel
)
$diagnosticValueLabels = @(
    $diagVersionValue, $diagUptimeValue, $diagServiceValue, $diagConnectionValue,
    $diagMonitoringValue, $diagAutoResumeValue, $diagLastScanValue, $diagTaskCountValue,
    $diagUsageSourceValue, $diagUsageRefreshValue, $diagUsageAttemptValue, $diagNextRefreshValue,
    $diagNextResumeValue, $diagLogsValue, $diagRecentErrorValue, $diagDebugValue
)
$diagnosticsGroup.Controls.AddRange(@($diagnosticFieldLabels + $diagnosticValueLabels))

$diagRefreshButton = [System.Windows.Forms.Button]::new()
$diagRefreshButton.Text = T 'btn_diag_refresh'
$diagRefreshButton.Location = [System.Drawing.Point]::new(18, 505)
$diagRefreshButton.Size = [System.Drawing.Size]::new(115, 30)
$diagOpenLogsButton = [System.Windows.Forms.Button]::new()
$diagOpenLogsButton.Text = T 'btn_open_logs'
$diagOpenLogsButton.Location = [System.Drawing.Point]::new(143, 505)
$diagOpenLogsButton.Size = [System.Drawing.Size]::new(145, 30)
$diagCopyButton = [System.Windows.Forms.Button]::new()
$diagCopyButton.Text = T 'btn_copy_diagnostics'
$diagCopyButton.Location = [System.Drawing.Point]::new(298, 505)
$diagCopyButton.Size = [System.Drawing.Size]::new(145, 30)
$diagDebugButton = [System.Windows.Forms.Button]::new()
$diagDebugButton.Text = T 'btn_enable_debug'
$diagDebugButton.Location = [System.Drawing.Point]::new(453, 505)
$diagDebugButton.Size = [System.Drawing.Size]::new(175, 30)
$diagNotice = New-Label '' 18 550 870 40
$diagNotice.ForeColor = [System.Drawing.Color]::SeaGreen
$diagnosticsPage.Controls.AddRange(@($diagRefreshButton, $diagOpenLogsButton, $diagCopyButton, $diagDebugButton, $diagNotice))

$promptLabel = New-Label (T 'label_prompt') 18 130 180 22
$settingsGroup.Controls.Add($promptLabel)
$promptBox = [System.Windows.Forms.TextBox]::new()
$promptBox.Location = [System.Drawing.Point]::new(18, 153)
$promptBox.Size = [System.Drawing.Size]::new(540, 54)
$promptBox.Multiline = $true
$promptBox.MaxLength = 500
$promptBox.ScrollBars = 'Vertical'
$promptBox.Text = [string]$config.retry_prompt
$settingsGroup.Controls.Add($promptBox)

$recoveryLabel = New-Label (T 'label_recovery') 18 221 145 22
$recoveryBox = New-NumberBox 168 218 1 1000 ([int]$config.max_recovery_attempts) 120
$consecutiveLabel = New-Label (T 'label_consecutive') 310 221 128 22
$consecutiveBox = New-NumberBox 438 218 1 100 ([int]$config.max_consecutive_retries) 120
$memoryLabel = New-Label (T 'label_memory') 18 347 145 22
$memoryBox = New-NumberBox 168 344 128 65536 ([int]$config.memory_limit_mb) 120
$resumeIntervalLabel = New-Label (T 'label_resume_interval') 18 92 172 22
$resumeIntervalBox = New-NumberBox 192 89 0 3600 ([int]$config.resume_interval_seconds) 75
$usageRefreshLabel = New-Label (T 'label_usage_refresh') 573 92 176 22
$usageRefreshBox = New-NumberBox 752 89 1 1440 ([int]$config.usage_refresh_minutes) 74
$schedulerGroup.Controls.AddRange(@($resumeIntervalLabel, $resumeIntervalBox, $usageRefreshLabel, $usageRefreshBox))
$settingsGroup.Controls.AddRange(@($recoveryLabel, $recoveryBox, $consecutiveLabel, $consecutiveBox, $memoryLabel, $memoryBox))

function Update-SchedulerModeView {
    $staggered = [bool]$schedulerStaggered.Checked
    foreach ($control in @($resumeIntervalLabel, $resumeIntervalBox, $parallelLimitLabel, $parallelLimitBox)) {
        $control.Visible = $staggered
    }
    $schedulerScope.Text = if ($staggered) { T 'scheduler_scope' } else { T 'scheduler_scope_sequential' }
}
Update-SchedulerModeView

$strategyLabel = New-Label (T 'label_strategy') 18 263 145 22
$strategyBox = [System.Windows.Forms.ComboBox]::new()
$strategyBox.Location = [System.Drawing.Point]::new(168, 260)
$strategyBox.Size = [System.Drawing.Size]::new(120, 24)
$strategyBox.DropDownStyle = 'DropDownList'
[void]$strategyBox.Items.Add((T 'strategy_exponential'))
[void]$strategyBox.Items.Add((T 'strategy_linear'))
[void]$strategyBox.Items.Add((T 'strategy_fixed'))
$strategyBox.SelectedIndex = switch ([string]$config.delay_strategy) {
    'linear' { 1 }
    'fixed' { 2 }
    default { 0 }
}

function Test-SettingsMemoryLimit {
    if ($script:memoryGuardTriggered) { return $true }
    $limitMB = 1024
    try {
        if ($config.memory_limit_mb) { $limitMB = [int]$config.memory_limit_mb }
        $process = Get-Process -Id $PID -ErrorAction Stop
        $usageMB = [int][Math]::Ceiling($process.PrivateMemorySize64 / 1MB)
        if ($limitMB -ge 128 -and $usageMB -ge $limitMB) {
            $script:memoryGuardTriggered = $true
            $logPath = Join-Path $DataDir 'logs\daemon.log'
            New-Item -ItemType Directory -Force -Path (Split-Path -Parent $logPath) | Out-Null
            Add-Content -LiteralPath $logPath -Value ("{0} memory guard triggered component=settings pid={1} private_memory_mb={2} limit_mb={3}" -f ([DateTime]::UtcNow.ToString('o')), $PID, $usageMB, $limitMB)
            [System.Windows.Forms.MessageBox]::Show(
                ([string]::Format((T 'memory_guard_msg'), $usageMB, $limitMB)),
                (T 'memory_guard_title'),
                [System.Windows.Forms.MessageBoxButtons]::OK,
                [System.Windows.Forms.MessageBoxIcon]::Warning
            ) | Out-Null
            $form.Close()
            return $true
        }
    }
    catch { }
    return $false
}
$initialDelayLabel = New-Label (T 'label_initial_delay') 310 263 118 22
$initialDelayBox = New-NumberBox 438 260 1 3600 ([int]$config.initial_delay_seconds) 120
$settingsGroup.Controls.AddRange(@($strategyLabel, $strategyBox, $initialDelayLabel, $initialDelayBox))

$maxDelayLabel = New-Label (T 'label_max_delay') 18 305 145 22
$maxDelayBox = New-NumberBox 168 302 1 86400 ([int]$config.max_delay_seconds) 120
$incrementLabel = New-Label (T 'label_increment') 310 305 128 22
$incrementBox = New-NumberBox 438 302 1 3600 ([int]$config.delay_increment_seconds) 120
$previewLabel = New-Label '' 18 380 540 38
$previewLabel.ForeColor = [System.Drawing.Color]::DimGray
$settingsGroup.Controls.AddRange(@($maxDelayLabel, $maxDelayBox, $incrementLabel, $incrementBox, $previewLabel))

function Assert-SettingsLayout {
    foreach ($pair in @(
        @($recoveryLabel, $recoveryBox, (T 'label_recovery')),
        @($consecutiveLabel, $consecutiveBox, (T 'label_consecutive')),
        @($memoryLabel, $memoryBox, (T 'label_memory')),
        @($strategyLabel, $strategyBox, (T 'label_strategy')),
        @($initialDelayLabel, $initialDelayBox, (T 'label_initial_delay')),
        @($maxDelayLabel, $maxDelayBox, (T 'label_max_delay')),
        @($incrementLabel, $incrementBox, (T 'label_increment')),
        @($resumeIntervalLabel, $resumeIntervalBox, (T 'label_resume_interval')),
        @($parallelLimitLabel, $parallelLimitBox, (T 'parallel_limit')),
        @($usageRefreshLabel, $usageRefreshBox, (T 'label_usage_refresh'))
    )) {
        if ($pair[0].Right -gt $pair[1].Left) {
            throw ((T 'layout_overlap_err') + [string]$pair[2])
        }
    }
    foreach ($box in @($recoveryBox, $consecutiveBox, $strategyBox, $initialDelayBox, $maxDelayBox, $incrementBox, $resumeIntervalBox, $parallelLimitBox, $usageRefreshBox)) {
        if ($box.Left -lt 0 -or $box.Right -gt $box.Parent.ClientSize.Width) {
            throw (T 'layout_bounds_err')
        }
    }
}
Assert-SettingsLayout

function Get-DelayStrategy {
    switch ($strategyBox.SelectedIndex) {
        1 { return 'linear' }
        2 { return 'fixed' }
        default { return 'exponential' }
    }
}

function Format-PreviewDelay {
    param([long]$Seconds)
    if ($Seconds -lt 60) { return ([string]$Seconds + (T 'unit_second')) }
    if ($Seconds % 3600 -eq 0) { return ([string]($Seconds / 3600) + (T 'unit_hour')) }
    if ($Seconds % 60 -eq 0) { return ([string]($Seconds / 60) + (T 'unit_minute')) }
    return ([string]$Seconds + (T 'unit_second'))
}

function Update-DelayPreview {
    $strategy = Get-DelayStrategy
    $initial = [long]$initialDelayBox.Value
    $maximum = [long]$maxDelayBox.Value
    $increment = [long]$incrementBox.Value
    $count = [Math]::Min([int]$consecutiveBox.Value, 6)
    $values = @()
    $delay = $initial
    for ($index = 0; $index -lt $count; $index++) {
        $value = if ($strategy -eq 'fixed') { $initial } else { [Math]::Min($delay, $maximum) }
        $values += (Format-PreviewDelay $value)
        if ($strategy -eq 'exponential') { $delay = [Math]::Min($delay * 2, $maximum) }
        if ($strategy -eq 'linear') { $delay = [Math]::Min($delay + $increment, $maximum) }
    }
    $sep = if ($script:currentLanguage -eq 'en') { ', ' } else { '，' }
    $suffix = ''
    if ([int]$consecutiveBox.Value -gt $count) {
        $suffix = if ($script:currentLanguage -eq 'en') { ', ...' } else { '，…' }
    }
    $previewLabel.Text = (T 'wait_seq_prefix') + ($values -join $sep) + $suffix
    $maxDelayBox.Enabled = $strategy -ne 'fixed'
    $incrementBox.Enabled = $strategy -eq 'linear'
    $initialDelayLabel.Text = if ($strategy -eq 'fixed') { T 'label_fixed_interval' } else { T 'label_initial_delay' }
}

Update-DelayPreview

$noticeLabel = New-Label '' 20 480 595 28
$noticeLabel.ForeColor = [System.Drawing.Color]::SeaGreen
$settingsPage.Controls.Add($noticeLabel)
$saveButton = [System.Windows.Forms.Button]::new()
$saveButton.Text = T 'btn_save'
$saveButton.Location = [System.Drawing.Point]::new(704, 478)
$saveButton.Size = [System.Drawing.Size]::new(100, 30)
$saveButton.BackColor = [System.Drawing.Color]::FromArgb(35, 39, 37)
$saveButton.ForeColor = [System.Drawing.Color]::White
$saveButton.FlatStyle = 'Flat'
$saveButton.Tag = 'primary'
$closeButton = [System.Windows.Forms.Button]::new()
$closeButton.Text = T 'btn_close'
$closeButton.Location = [System.Drawing.Point]::new(814, 478)
$closeButton.Size = [System.Drawing.Size]::new(85, 30)
$closeButton.Tag = 'secondary'
$settingsPage.Controls.AddRange(@($saveButton, $closeButton))
$form.CancelButton = $closeButton

$settingsInputControls = @(
    $enabledCheck, $monitoringCheck, $sharedCheck, $safeLaunchButton, $notificationsCheck, $cliFallbackCheck, $promptBox,
    $recoveryBox, $consecutiveBox, $strategyBox, $initialDelayBox,
    $maxDelayBox, $incrementBox, $memoryBox,
    $resumeIntervalBox, $usageRefreshBox, $parallelLimitBox, $schedulerSequential, $schedulerStaggered
)

function Set-SettingsBusy {
    param([bool]$Busy)
    $taskList.Enabled = -not $Busy
    $retryNowButton.Enabled = -not $Busy
    $cancelRetryButton.Enabled = -not $Busy
    $restartRetryButton.Enabled = -not $Busy
    $refreshTasksButton.Enabled = -not $Busy
    $autoDispatchButton.Enabled = -not $Busy
    $schedulerSaveButton.Enabled = -not $Busy
    $autoScanCheck.Enabled = -not $Busy
    $scanIntervalBox.Enabled = -not $Busy
    foreach ($control in $settingsInputControls) {
        $control.Enabled = -not $Busy
    }
    $saveButton.Enabled = -not $Busy
    $closeButton.Enabled = -not $Busy
    if ($Busy) {
        $saveButton.Text = T 'busy_checking'
        $noticeLabel.Text = T 'busy_notice'
        $noticeLabel.ForeColor = [System.Drawing.Color]::DarkOrange
    } else {
        $saveButton.Text = T 'btn_save'
        Update-DelayPreview
        Update-ActionButtons
    }
}

function Get-StateText {
    param([string]$State)
    $lang = $script:currentLanguage
    switch ($State) {
        'pending'  { if ($lang -eq 'en') { return 'Pending' } else { return '等待中' } }
        'starting' { if ($lang -eq 'en') { return 'Starting' } else { return '啟動中' } }
        'running'  { if ($lang -eq 'en') { return 'Running' } else { return '執行中' } }
        'stopped'  { if ($lang -eq 'en') { return 'Limit Reached' } else { return '達到上限' } }
        'rate_limited' { if ($lang -eq 'en') { return 'Quota paused' } else { return '額度暫停' } }
        'quota_recovered' { if ($lang -eq 'en') { return 'Quota recovered' } else { return '額度已恢復' } }
        'awaiting_confirmation' { if ($lang -eq 'en') { return 'Awaiting auto-check' } else { return '等待自動判定' } }
        'paused' { if ($lang -eq 'en') { return 'Paused' } else { return '已暫停' } }
        'completed' { if ($lang -eq 'en') { return 'Completed' } else { return '已完成' } }
        'idle' { if ($lang -eq 'en') { return 'Idle' } else { return '閒置' } }
        'notLoaded' { if ($lang -eq 'en') { return 'Not loaded' } else { return '未載入' } }
        'active' { if ($lang -eq 'en') { return 'Active' } else { return '執行中' } }
        'unknown' { if ($lang -eq 'en') { return 'Unknown' } else { return '未知' } }
        default { return $State }
    }
}

function Get-StoppedStateText {
    param([string]$Reason)
    $lang = $script:currentLanguage
    if ($Reason -eq 'codex_not_running') {
        if ($lang -eq 'en') { return 'Codex Exited' } else { return 'Codex 已退出' }
    }
    if ($Reason -eq 'shared_app_server_disabled') {
        if ($lang -eq 'en') { return 'Codex Auto-Resume Assistant Disabled' } else { return 'Codex自動續跑助手已關閉' }
    }
    if ($Reason -eq 'codex_restart_required') {
        if ($lang -eq 'en') { return 'Codex Not Connected' } else { return 'Codex 未接入 Codex自動續跑助手' }
    }
    if ($Reason -eq 'codex_home_not_shared') {
        if ($lang -eq 'en') { return 'Task Dir Not Connected' } else { return 'Task 目錄未接入' }
    }
    if ($Reason -eq 'shared_app_server_port_conflict') {
        if ($lang -eq 'en') { return 'Port Conflict' } else { return '恢復連接埠衝突' }
    }
    if ($Reason -eq 'shared_app_server_port_reserved') {
        if ($lang -eq 'en') { return 'Port Reserved by Windows' } else { return '連接埠被 Windows 保留' }
    }
    if ($Reason -eq 'shared_app_server_config_invalid') {
        if ($lang -eq 'en') { return 'Config Incompatible; Reverted' } else { return 'Codex自動續跑助手設定不相容，已切回官方後台' }
    }
    if ($Reason -like 'controller_*' -or $Reason -like 'codex_background_*' -or $Reason -eq 'app_server_request_failed') {
        if ($lang -eq 'en') { return 'Recovery Channel Failed' } else { return '恢復通道失敗' }
    }
    if ($Reason -eq 'goal_empty_response_limit_block_failed') {
        if ($lang -eq 'en') { return 'Goal Stop Failed' } else { return '目標停止失敗' }
    }
    if ($Reason -eq 'goal_empty_response_limit') {
        if ($lang -eq 'en') { return 'Goal Stopped (Empty Replies)' } else { return '目標空白回覆已停止' }
    }
    if ($lang -eq 'en') { return 'Limit Reached' } else { return '達到上限' }
}

function Get-ClassText {
    param([string]$Class)
    $lang = $script:currentLanguage
    switch ($Class) {
        'transient'      { if ($lang -eq 'en') { return 'Connection Dropped' } else { return '連線中斷' } }
        'rate_limit'     { if ($lang -eq 'en') { return 'Rate Limited' } else { return '要求遭限流' } }
        'server'         { if ($lang -eq 'en') { return 'Provider Failure' } else { return '服務供應商故障' } }
        'auth_transient' { if ($lang -eq 'en') { return 'Auth Service Error' } else { return '登入服務異常' } }
        'auth_limited'   { if ($lang -eq 'en') { return 'Auth Error' } else { return '登入異常' } }
        'empty_response' { if ($lang -eq 'en') { return 'Empty Model Reply' } else { return '模型空白回覆' } }
        'unknown'        { if ($lang -eq 'en') { return 'Unknown Fault' } else { return '未知故障' } }
        default          { if ($lang -eq 'en') { return 'Unclassified' } else { return '未分類' } }
    }
}

$stoppedRetryDisplayWindow = [TimeSpan]::FromHours(1)

function Test-StoppedRetryVisible {
    param($Stopped)
    if (-not $Stopped -or [bool]$Stopped.historical) { return $false }
    $now = [DateTimeOffset]::UtcNow
    foreach ($timestamp in @([string]$Stopped.failed_at, [string]$Stopped.stopped_at)) {
        if ([string]::IsNullOrWhiteSpace($timestamp)) { continue }
        try {
            $age = $now - [DateTimeOffset]::Parse($timestamp)
            if ($age -ge [TimeSpan]::Zero -and $age -gt $stoppedRetryDisplayWindow) { return $false }
        } catch { }
    }
    return $true
}

function Get-TaskStatusText {
    param($Task)
    $state = [string]$Task.status
    if ($state -eq 'stopped') {
        if ([string]$Task.stop_reason) {
            if ([bool]$Task.selected_for_monitoring) { return T 'task_stopped_watched' }
            return T 'task_stopped_unwatched'
        }
        if ([bool]$Task.selected_for_monitoring) { return T 'task_stopped_catalog_watched' }
        return T 'task_stopped_catalog'
    }
    if ($state -eq 'pending') {
        if ([string]$Task.wait_reason -eq 'quota_limited') { return T 'task_quota_wait' }
        if ([string]$Task.wait_reason -eq 'quota_status_unavailable') { return T 'task_quota_check' }
        return T 'task_retry_pending'
    }
    if ($state -in @('starting', 'running') -and [bool]$Task.is_recovery) { return T 'task_retry_running' }
    if ([string]$Task.last_recovery_succeeded_at) {
        if ($state -eq 'awaiting_confirmation') { return T 'task_recovery_handoff' }
        return T 'task_recovery_success'
    }
    if ([bool]$Task.info_unavailable) { return T 'task_info_unavailable' }
    return Get-StateText $state
}

function Get-TaskStatusDetail {
    param($Task)
    if ([string]$Task.status -eq 'stopped' -and [string]$Task.stop_reason) {
        return [string]::Format((T 'task_stopped_detail'), (Get-StoppedStateText ([string]$Task.stop_reason)))
    }
    if ([string]$Task.last_recovery_succeeded_at) { return T 'task_success_detail' }
    return Get-TaskStatusText $Task
}

function Format-QuotaCountdown {
    param([long]$ResetsAt)
    if ($ResetsAt -le 0) { return T 'usage_unavailable' }
    $reset = [DateTimeOffset]::FromUnixTimeSeconds($ResetsAt)
    $seconds = [Math]::Ceiling(($reset - [DateTimeOffset]::UtcNow).TotalSeconds)
    if ($seconds -le 0) { return T 'usage_rechecking' }
    $minutes = [Math]::Max(1, [Math]::Ceiling($seconds / 60))
    if ($minutes -lt 60) { return ([string][int]$minutes + (T 'unit_minute')) }
    $hours = [Math]::Ceiling($minutes / 60)
    if ($hours -lt 24) { return ([string][int]$hours + (T 'unit_hour')) }
    return ([string][int][Math]::Ceiling($hours / 24) + $(if ($script:currentLanguage -eq 'en') { 'd' } else { ' 天' }))
}

function Set-QuotaProgressColor {
    param($Progress, [double]$Percent, [bool]$Available)
    $color = if (-not $Available) {
        [System.Drawing.ColorTranslator]::FromHtml('#9AA0A6')
    } elseif ($Percent -le 5) {
        [System.Drawing.ColorTranslator]::FromHtml('#C62828')
    } elseif ($Percent -le 20) {
        [System.Drawing.ColorTranslator]::FromHtml('#EF6C00')
    } else {
        [System.Drawing.ColorTranslator]::FromHtml('#1565C0')
    }
    $fill = [System.Windows.Forms.Panel]$Progress.Controls[0]
    $fill.BackColor = $color
    $width = if ($Available) { [int][Math]::Round($Progress.ClientSize.Width * [Math]::Max(0, [Math]::Min(100, $Percent)) / 100) } else { 0 }
    if ($fill.Width -ne $width -or $fill.Height -ne $Progress.ClientSize.Height) {
        $fill.Size = [System.Drawing.Size]::new($width, $Progress.ClientSize.Height)
    }
}

function Update-UsageRow {
    param($Window, $Progress, $Remaining, $Reset)
    if (-not $Window -or [long]$Window.resets_at -le 0) {
        Set-QuotaProgressColor $Progress 0 $false
        $Remaining.Text = T 'usage_unavailable'
        $Reset.Text = T 'usage_unavailable'
        $usageToolTip.SetToolTip($Reset, '')
        return
    }
    $percent = [Math]::Max(0, [Math]::Min(100, [double]$Window.remaining_percent))
    Set-QuotaProgressColor $Progress $percent $true
    $displayPercent = if ([Math]::Abs($percent - [Math]::Round($percent)) -lt 0.05) { [string][int][Math]::Round($percent) } else { $percent.ToString('0.0') }
    $Remaining.Text = [string]::Format((T 'usage_remaining'), $displayPercent)
    $Reset.Text = [string]::Format((T 'usage_reset'), (Format-QuotaCountdown ([long]$Window.resets_at)))
    $exact = [DateTimeOffset]::FromUnixTimeSeconds([long]$Window.resets_at).ToLocalTime().ToString('yyyy/MM/dd HH:mm:ss')
    $usageToolTip.SetToolTip($Reset, $exact)
}

function Update-UsageView {
    param($Dashboard)
    $usage = if ($Dashboard) { $Dashboard.codex_usage } else { $null }
    $usageRefreshingLabel.Text = ''
    if ($usage -and [bool]$usage.refreshing) {
        $frames = @('◐', '◓', '◑', '◒')
        $usageRefreshingLabel.Text = $frames[$script:refreshFrame % $frames.Count] + ' ' + (T 'task_refreshing')
    }
    if (-not $usage -or -not [bool]$usage.available) {
        Update-UsageRow $null $usage5Progress $usage5Remaining $usage5Reset
        Update-UsageRow $null $usage7Progress $usage7Remaining $usage7Reset
        $usageToolTip.SetToolTip($usageGroup, '')
        return
    }
    Update-UsageRow $usage.five_hour $usage5Progress $usage5Remaining $usage5Reset
    Update-UsageRow $usage.weekly $usage7Progress $usage7Remaining $usage7Reset
    $source = switch ([string]$usage.source) {
        'account/rateLimits/read' { T 'usage_source_official'; break }
        'rollout_snapshot' { T 'usage_source_rollout'; break }
        default { T 'usage_unavailable' }
    }
    $observed = if ([string]$usage.observed_at) { Format-DiagnosticTime ([string]$usage.observed_at) } else { Format-DiagnosticTime ([string]$usage.fetched_at) }
    $usageToolTip.SetToolTip($usageGroup, ($source + [Environment]::NewLine + (T 'usage_observed_at') + $observed))
}

function Update-GuideView {
    param($Dashboard)
    $catalog = if ($Dashboard) { @($Dashboard.status_catalog) } else { @() }
    $quotaCatalog = if ($Dashboard) { @($Dashboard.quota_catalog) } else { @() }
    $signature = (($catalog | ConvertTo-Json -Depth 5 -Compress) + ($quotaCatalog | ConvertTo-Json -Depth 5 -Compress) + $script:currentLanguage)
    if ([string]$guideList.Tag -eq $signature) { return }
    $guideList.BeginUpdate()
    $guideList.Items.Clear()
    if ($catalog.Count -eq 0) {
        [void]$guideList.Items.Add((T 'guide_waiting_data'))
    } else {
        foreach ($definition in $catalog) {
            $label = if ($script:currentLanguage -eq 'en') { [string]$definition.label_en } else { [string]$definition.label_zh }
            $meaning = if ($script:currentLanguage -eq 'en') { [string]$definition.meaning_en } else { [string]$definition.meaning_zh }
            $behavior = if ($script:currentLanguage -eq 'en') { [string]$definition.behavior_en } else { [string]$definition.behavior_zh }
            $notification = if ($script:currentLanguage -eq 'en') { [string]$definition.notification_en } else { [string]$definition.notification_zh }
            $item = [System.Windows.Forms.ListViewItem]::new($label)
            $item.ToolTipText = $behavior
            $item.ForeColor = $guideList.ForeColor
            $item.UseItemStyleForSubItems = $false
            $colorCell = $item.SubItems.Add('● ' + [string]$definition.color)
            $meaningCell = $item.SubItems.Add($meaning)
            $behaviorCell = $item.SubItems.Add($behavior)
            $notificationCell = $item.SubItems.Add($notification)
            foreach ($cell in @($meaningCell, $behaviorCell, $notificationCell)) { $cell.ForeColor = $guideList.ForeColor }
            try { $colorCell.ForeColor = [System.Drawing.ColorTranslator]::FromHtml([string]$definition.color) } catch { }
            [void]$guideList.Items.Add($item)
        }
        foreach ($definition in $quotaCatalog) {
            $meaning = if ($script:currentLanguage -eq 'en') { [string]$definition.meaning_en } else { [string]$definition.meaning_zh }
            $item = [System.Windows.Forms.ListViewItem]::new('Quota ' + [string]$definition.code)
            $item.ForeColor = $guideList.ForeColor
            $item.UseItemStyleForSubItems = $false
            $colorCell = $item.SubItems.Add('● ' + [string]$definition.color)
            $meaningCell = $item.SubItems.Add($meaning)
            $behaviorCell = $item.SubItems.Add($(if ($script:currentLanguage -eq 'en') { 'Changes the quota bar color only.' } else { '只改變額度條顏色。' }))
            $notificationCell = $item.SubItems.Add($(if ($script:currentLanguage -eq 'en') { 'None' } else { '不通知' }))
            foreach ($cell in @($meaningCell, $behaviorCell, $notificationCell)) { $cell.ForeColor = $guideList.ForeColor }
            try { $colorCell.ForeColor = [System.Drawing.ColorTranslator]::FromHtml([string]$definition.color) } catch { }
            [void]$guideList.Items.Add($item)
        }
    }
    $guideList.EndUpdate()
    $guideList.Tag = $signature
}

function Format-DiagnosticTime {
    param([string]$Value)
    if ([string]::IsNullOrWhiteSpace($Value)) { return '--' }
    try { return [DateTimeOffset]::Parse($Value).ToLocalTime().ToString('yyyy/MM/dd HH:mm:ss') } catch { return '--' }
}

function Format-DiagnosticUptime {
    param([long]$Seconds)
    if ($Seconds -lt 0) { $Seconds = 0 }
    $span = [TimeSpan]::FromSeconds($Seconds)
    if ($span.Days -gt 0) { return ('{0}d {1:00}:{2:00}:{3:00}' -f $span.Days, $span.Hours, $span.Minutes, $span.Seconds) }
    return ('{0:00}:{1:00}:{2:00}' -f [int]$span.TotalHours, $span.Minutes, $span.Seconds)
}

function Get-DiagnosticStateText {
    param([string]$State)
    switch ($State) {
        'running'           { return T 'diag_running' }
        'paused'            { return T 'diag_paused' }
        'connected'         { return T 'diag_connected' }
        'waiting_for_codex' { return T 'diag_waiting_codex' }
        'codex_not_running' { return T 'diag_codex_stopped' }
        'disabled'          { return T 'diag_disabled' }
        'stopped'           { return T 'status_not_running' }
        'unavailable'       { return T 'usage_unavailable' }
        'unknown'           { return T 'diag_unknown' }
        default {
            if ([string]::IsNullOrWhiteSpace($State)) { return '--' }
            return $State
        }
    }
}

function Update-DiagnosticsView {
    param($Dashboard, $Status)
    $diagnostics = if ($Dashboard) { $Dashboard.diagnostics } else { $null }
    $script:lastDiagnostics = $diagnostics
    if (-not $diagnostics) {
        foreach ($valueLabel in $diagnosticValueLabels) { $valueLabel.Text = '--' }
        $diagLogsValue.Text = T 'diag_log_policy'
        $diagRecentErrorValue.Text = T 'diag_none'
        $diagDebugValue.Text = T 'diag_disabled'
        $script:debugEnabled = $false
        $diagDebugButton.Text = T 'btn_enable_debug'
        return
    }
    $diagVersionValue.Text = if ([string]$diagnostics.version) {
        'v' + [string]$diagnostics.version + $(if ($diagnostics.PSObject.Properties['build'] -and [int]$diagnostics.build -gt 0) { ' (Build ' + [int]$diagnostics.build + ')' } else { '' })
    } else { '--' }
    $uptimeSeconds = [long]$diagnostics.uptime_seconds
    if ([string]$diagnostics.started_at -and [string]$diagnostics.service_state -eq 'running') {
        try { $uptimeSeconds = [Math]::Max(0, [Math]::Floor(([DateTimeOffset]::UtcNow - [DateTimeOffset]::Parse([string]$diagnostics.started_at)).TotalSeconds)) } catch { }
    }
    $diagUptimeValue.Text = Format-DiagnosticUptime $uptimeSeconds
    $diagServiceValue.Text = Get-DiagnosticStateText ([string]$diagnostics.service_state)
    $diagConnectionValue.Text = Get-DiagnosticStateText ([string]$diagnostics.app_server_connection)
    $diagMonitoringValue.Text = Get-DiagnosticStateText ([string]$diagnostics.monitoring_state)
    $diagAutoResumeValue.Text = Get-DiagnosticStateText ([string]$diagnostics.auto_resume_state)
    $diagLastScanValue.Text = Format-DiagnosticTime ([string]$diagnostics.last_task_scan_at)
    $diagTaskCountValue.Text = [string][int]$diagnostics.recovery_task_count
    $diagUsageSourceValue.Text = switch ([string]$diagnostics.usage_source) {
        'account/rateLimits/read' { 'account/rateLimits/read'; break }
        'rollout_snapshot' { T 'usage_source_rollout'; break }
        default { T 'usage_unavailable' }
    }
    $diagUsageRefreshValue.Text = Format-DiagnosticTime ([string]$diagnostics.last_usage_refresh_at)
    $diagUsageAttemptValue.Text = Format-DiagnosticTime ([string]$diagnostics.last_usage_attempt_at)
    $diagNextRefreshValue.Text = Format-DiagnosticTime ([string]$diagnostics.next_usage_refresh_at)
    $diagNextResumeValue.Text = Format-DiagnosticTime ([string]$diagnostics.next_resume_at)
    $diagLogsValue.Text = T 'diag_log_policy'
    $diagRecentErrorValue.Text = if ([string]$diagnostics.recent_error) { [string]$diagnostics.recent_error } else { T 'diag_none' }
    $script:debugEnabled = [bool]$diagnostics.debug_enabled
    if ($script:debugEnabled) {
        $until = Format-DiagnosticTime ([string]$diagnostics.debug_until)
        $diagDebugValue.Text = if ($script:currentLanguage -eq 'en') { 'Enabled until ' + $until } else { '啟用至 ' + $until + '（到期自動關閉）' }
        $diagDebugButton.Text = T 'btn_disable_debug'
    } else {
        $diagDebugValue.Text = T 'diag_disabled'
        $diagDebugButton.Text = T 'btn_enable_debug'
    }
}

function Copy-DiagnosticsToClipboard {
    $lines = @(
        'CodexWorkResumer Diagnostics',
        ((T 'diag_version') + ': ' + $diagVersionValue.Text),
        ((T 'diag_uptime') + ': ' + $diagUptimeValue.Text),
        ((T 'diag_service') + ': ' + $diagServiceValue.Text),
        ((T 'diag_connection') + ': ' + $diagConnectionValue.Text),
        ((T 'diag_monitoring') + ': ' + $diagMonitoringValue.Text),
        ((T 'diag_auto_resume') + ': ' + $diagAutoResumeValue.Text),
        ((T 'diag_last_scan') + ': ' + $diagLastScanValue.Text),
        ((T 'diag_task_count') + ': ' + $diagTaskCountValue.Text),
        ((T 'diag_usage_source') + ': ' + $diagUsageSourceValue.Text),
        ((T 'diag_usage_refresh') + ': ' + $diagUsageRefreshValue.Text),
        ((T 'diag_usage_attempt') + ': ' + $diagUsageAttemptValue.Text),
        ((T 'diag_next_refresh') + ': ' + $diagNextRefreshValue.Text),
        ((T 'diag_next_resume') + ': ' + $diagNextResumeValue.Text),
        ((T 'diag_logs') + ': ' + $diagLogsValue.Text),
        ((T 'diag_recent_error') + ': ' + $diagRecentErrorValue.Text),
        ((T 'diag_debug') + ': ' + $diagDebugValue.Text)
    )
    [System.Windows.Forms.Clipboard]::SetText(($lines -join [Environment]::NewLine))
    $diagNotice.Text = T 'diag_copied'
    $diagNotice.ForeColor = [System.Drawing.Color]::SeaGreen
}

function Get-UniqueTaskIDs {
    param([object[]]$Values)
    $ids = [System.Collections.Generic.List[string]]::new()
    $seen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($value in @($Values)) {
        $id = [string]$value
        if ($id) { $id = $id.Trim().ToLowerInvariant() }
        if ($id -and $seen.Add($id)) { $ids.Add($id) }
    }
    return ,$ids.ToArray()
}

function Get-CurrentTaskPlan {
    param($Dashboard)
    # The plan file is authoritative. dashboard.json can lag behind a control
    # command by one worker tick and must not briefly undo a user's checkbox.
    $planExists = Test-Path -LiteralPath $taskPlanPath
    $plan = if ($planExists) { Read-JsonFile $taskPlanPath } else { $null }
    $invalid = $false
    if ($planExists) {
        try {
            $invalid = -not $plan -or -not $plan.PSObject.Properties['version'] -or
                -not $plan.PSObject.Properties['revision'] -or
                -not $plan.PSObject.Properties['configured'] -or
                -not $plan.PSObject.Properties['ordered_thread_ids'] -or
                -not $plan.PSObject.Properties['selected_thread_ids'] -or
                [int]$plan.version -ne 1 -or [int64]$plan.revision -lt 1 -or
                -not [bool]$plan.configured
            if (-not $invalid) {
                $ordered = @($plan.ordered_thread_ids)
                $selected = @($plan.selected_thread_ids)
                $known = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
                $chosen = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
                $canonicalID = '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                $invalid = $plan.ordered_thread_ids -isnot [array] -or
                    $plan.selected_thread_ids -isnot [array] -or
                    $ordered.Count -gt $taskPlanMaxIDs
                foreach ($id in $ordered) {
                    if ([string]$id -notmatch $canonicalID -or -not $known.Add([string]$id)) { $invalid = $true }
                }
                foreach ($id in $selected) {
                    if (-not $known.Contains([string]$id) -or -not $chosen.Add([string]$id)) { $invalid = $true }
                }
            }
        } catch { $invalid = $true }
    }
    if ($planExists -and $invalid) {
        return [pscustomobject]@{
            version = 1; revision = 0; configured = $true; invalid = $true
            ordered_thread_ids = @(); selected_thread_ids = @()
        }
    }
    if (-not $planExists -and $Dashboard -and $Dashboard.task_plan -and [bool]$Dashboard.task_plan.configured) {
        # A previously saved file disappearing is not the unconfigured legacy
        # case. Never fall back to a stale dashboard and silently rewrite it.
        return [pscustomobject]@{
            version = 1; revision = 0; configured = $true; invalid = $true
            ordered_thread_ids = @(); selected_thread_ids = @()
        }
    }
    if ($plan) { return $plan }
    return [pscustomobject]@{
        version = 1; revision = 0; configured = $false; invalid = $false
        ordered_thread_ids = @(); selected_thread_ids = @()
    }
}

function Save-TaskPlan {
    param([string[]]$OrderedIDs, [string[]]$SelectedIDs)
    if ($script:taskPlanSaveInProgress -or -not $script:taskPlan -or [bool]$script:taskPlan.invalid) { return $false }
    $ordered = Get-UniqueTaskIDs $OrderedIDs
    $selected = Get-UniqueTaskIDs $SelectedIDs
    $orderedSet = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($id in $ordered) { [void]$orderedSet.Add($id) }
    foreach ($id in $selected) {
        if ($orderedSet.Add($id)) { $ordered += $id }
    }
    $protected = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($id in $selected) { [void]$protected.Add($id) }
    foreach ($item in $taskList.Items) { [void]$protected.Add([string]$item.Tag.ThreadID) }
    if ($protected.Count -gt $taskPlanMaxIDs) {
        $script:taskPlanSaveScheduled = $false
        [System.Windows.Forms.MessageBox]::Show((T 'task_plan_full'), 'CodexWorkResumer', 'OK', 'Warning') | Out-Null
        Update-RuntimeView
        return $false
    }
    $trimmed = [System.Collections.Generic.List[string]]::new()
    foreach ($id in $ordered) { $trimmed.Add($id) }
    for ($index = $trimmed.Count - 1; $index -ge 0 -and $trimmed.Count -gt $taskPlanMaxIDs; $index--) {
        if (-not $protected.Contains($trimmed[$index])) { $trimmed.RemoveAt($index) }
    }
    if ($trimmed.Count -gt $taskPlanMaxIDs) {
        $script:taskPlanSaveScheduled = $false
        [System.Windows.Forms.MessageBox]::Show((T 'task_plan_full'), 'CodexWorkResumer', 'OK', 'Warning') | Out-Null
        Update-RuntimeView
        return $false
    }
    $ordered = $trimmed.ToArray()
    $payload = [ordered]@{
        expected_revision = [int64]$script:taskPlan.revision
        ordered_thread_ids = @($ordered)
        selected_thread_ids = @($selected)
    }
    $script:taskPlanSaveInProgress = $true
    $saved = $false
    try {
        $exitCode = Start-LocalCommand 'control' @{
            CODEX_AUTO_RETRY_ACTION = 'save_task_plan'
            CODEX_AUTO_RETRY_TASK_PLAN = (ConvertTo-Json -InputObject $payload -Depth 4 -Compress)
        } 5000
        $saved = $exitCode -eq 0
    } finally {
        $script:taskPlanSaveInProgress = $false
        $script:taskPlanSaveScheduled = $false
    }
    if (-not $saved) {
        [System.Windows.Forms.MessageBox]::Show((T 'task_plan_conflict'), 'CodexWorkResumer', 'OK', 'Warning') | Out-Null
    }
    # Always reload after a CAS result. An older window must never silently
    # overwrite a newer selection or drag order from another window.
    Update-RuntimeView
    return $saved
}

function Save-VisibleTaskSelection {
    if (-not $script:taskPlan) {
        $script:taskPlanSaveScheduled = $false
        return
    }
    $ordered = [System.Collections.Generic.List[string]]::new()
    $orderedSet = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($id in (Get-UniqueTaskIDs @($script:taskPlan.ordered_thread_ids))) {
        if ($orderedSet.Add($id)) { $ordered.Add($id) }
    }
    $selectedSet = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($id in (Get-UniqueTaskIDs @($script:taskPlan.selected_thread_ids))) { [void]$selectedSet.Add($id) }
    foreach ($item in $taskList.Items) {
        $id = [string]$item.Tag.ThreadID
        if ($orderedSet.Add($id)) { $ordered.Add($id) }
        [void]$selectedSet.Remove($id)
        if ($item.Checked) { [void]$selectedSet.Add($id) }
    }
    $selected = [System.Collections.Generic.List[string]]::new()
    foreach ($id in $ordered) { if ($selectedSet.Contains($id)) { $selected.Add($id) } }
    if (Save-TaskPlan $ordered.ToArray() $selected.ToArray()) {
        if (-not $script:taskSortWasUserChosen) { $script:taskSortColumn = -1 }
    }
}

function Get-TaskDropSlotForRow {
    param([int]$RowIndex, [System.Drawing.Rectangle]$Bounds, [int]$CursorY)
    if ($RowIndex -lt 0 -or $Bounds.Height -le 0) { return -1 }
    $middle = $Bounds.Top + [Math]::Floor($Bounds.Height / 2)
    return $RowIndex + [int]($CursorY -ge $middle)
}

function Get-TaskMoveIndex {
    param([int]$SourceIndex, [int]$DropSlot, [int]$Count)
    if ($Count -lt 2 -or $SourceIndex -lt 0 -or $SourceIndex -ge $Count -or
        $DropSlot -lt 0 -or $DropSlot -gt $Count) { return -1 }
    $moveIndex = $DropSlot
    if ($SourceIndex -lt $DropSlot) { $moveIndex-- }
    if ($moveIndex -eq $SourceIndex) { return -1 }
    return $moveIndex
}

function Get-TaskDropCandidate {
    param([string]$SourceID, [System.Drawing.Point]$ClientPoint)
    if (-not $SourceID -or $taskList.Items.Count -lt 2 -or
        -not $taskList.ClientRectangle.Contains($ClientPoint)) { return $null }
    $sourceIndex = -1
    foreach ($item in $taskList.Items) {
        if ([string]$item.Tag.ThreadID -eq $SourceID) { $sourceIndex = $item.Index; break }
    }
    if ($sourceIndex -lt 0) { return $null }
    $target = $taskList.GetItemAt($ClientPoint.X, $ClientPoint.Y)
    if (-not $target) { $target = $taskList.GetItemAt(2, $ClientPoint.Y) }
    if (-not $target) {
        $last = $taskList.Items[$taskList.Items.Count - 1]
        if ($last.Bounds.Bottom -le $ClientPoint.Y -and
            $last.Bounds.Top -lt $taskList.ClientRectangle.Bottom) {
            $target = $last
        }
    }
    if (-not $target) { return $null }
    $slot = Get-TaskDropSlotForRow $target.Index $target.Bounds $ClientPoint.Y
    if ($ClientPoint.Y -ge $target.Bounds.Bottom) { $slot = $target.Index + 1 }
    $moveIndex = Get-TaskMoveIndex $sourceIndex $slot $taskList.Items.Count
    if ($moveIndex -lt 0) { return $null }
    return [pscustomobject]@{
        Target = $target
        After = ($slot -gt $target.Index)
        MoveIndex = $moveIndex
    }
}

function Clear-TaskDragCue {
    if ($script:taskDragHoverItem) {
        $script:taskDragHoverItem.BackColor = $script:taskDragHoverBackColor
    }
    $script:taskDragHoverItem = $null
    $script:taskDragHoverBackColor = $null
    $script:taskDragHoverAfter = $false
    $taskSelectionHint.Text = if ([bool]$script:taskPlan.invalid) { T 'task_plan_invalid' } elseif (-not [bool]$script:taskPlan.configured) { T 'task_selection_legacy' } else { T 'task_selection_hint' }
    $taskSelectionHint.ForeColor = if ([bool]$script:taskPlan.invalid) { [System.Drawing.Color]::Firebrick } else { [System.Drawing.Color]::DimGray }
}

function Set-TaskDragCue {
    param($Candidate)
    if (-not $Candidate) { Clear-TaskDragCue; return }
    if ($script:taskDragHoverItem -ne $Candidate.Target) {
        if ($script:taskDragHoverItem) {
            $script:taskDragHoverItem.BackColor = $script:taskDragHoverBackColor
        }
        $script:taskDragHoverItem = $Candidate.Target
        $script:taskDragHoverBackColor = $Candidate.Target.BackColor
        $Candidate.Target.BackColor = [System.Drawing.Color]::FromArgb(216, 236, 252)
    }
    $script:taskDragHoverAfter = [bool]$Candidate.After
    $name = ([string]$Candidate.Target.ToolTipText).Split([char]10)[0].Trim([char]13)
    if (-not $name) { $name = [string]$Candidate.Target.Text }
    if ($name.Length -gt 32) { $name = $name.Substring(0, 31) + '…' }
    $taskSelectionHint.Text = if ($Candidate.After) { (T 'task_drag_after') -f $name } else { (T 'task_drag_before') -f $name }
    $taskSelectionHint.ForeColor = [System.Drawing.Color]::FromArgb(0, 90, 170)
}

function Save-ReorderedTaskList {
    param([string[]]$VisibleIDs)
    if (-not $script:taskPlan) { return }
    $visible = Get-UniqueTaskIDs $VisibleIDs
    $visibleSet = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($id in $visible) { [void]$visibleSet.Add($id) }
    $oldOrder = [System.Collections.Generic.List[string]]::new()
    $oldSet = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($id in (Get-UniqueTaskIDs @($script:taskPlan.ordered_thread_ids))) {
        if ($oldSet.Add($id)) { $oldOrder.Add($id) }
    }
    foreach ($id in $visible) { if ($oldSet.Add($id)) { $oldOrder.Add($id) } }
    $result = [System.Collections.Generic.List[string]]::new()
    $visibleIndex = 0
    foreach ($id in $oldOrder) {
        if ($visibleSet.Contains($id)) {
            $result.Add($visible[$visibleIndex])
            $visibleIndex++
        } else {
            $result.Add($id)
        }
    }
    if (Save-TaskPlan $result.ToArray() (Get-UniqueTaskIDs @($script:taskPlan.selected_thread_ids))) {
        $script:taskSortColumn = -1
        $script:taskSortWasUserChosen = $true
        Update-TaskSortHeaders
        Update-RuntimeView
    }
}

function Update-ActionButtons {
    $retryNowButton.Enabled = $false
    $cancelRetryButton.Enabled = $false
    $restartRetryButton.Enabled = $false
    if ($script:localCommandInProgress) {
        $taskActionHint.Text = T 'action_hint_busy'
        return
    }
    $resumable = 0
    $pending = 0
    foreach ($item in $taskList.SelectedItems) {
        if ([bool]$item.Tag.CanResume) { $resumable++ }
        if ([bool]$item.Tag.CanCancel) { $pending++ }
    }
    $retryNowButton.Enabled = $script:resumeRouteAvailable -and $resumable -gt 0
    $cancelRetryButton.Enabled = $pending -gt 0
    $restartRetryButton.Enabled = $taskList.Items.Count -gt 0 -and -not [bool]$script:taskPlan.invalid
    $hint = if ($taskList.SelectedItems.Count -eq 0) { 'action_hint_select' }
        elseif ($retryNowButton.Enabled -and $cancelRetryButton.Enabled) { 'action_hint_both' }
        elseif ($retryNowButton.Enabled) { 'action_hint_resume' }
        elseif ($cancelRetryButton.Enabled) { 'action_hint_cancel' }
        elseif ($resumable -gt 0) { 'action_hint_route' }
        else { 'action_hint_unavailable' }
    $taskActionHint.Text = T $hint
    $taskActionToolTip.SetToolTip($retryNowButton, (T $(if ($retryNowButton.Enabled) { 'action_hint_resume' } else { $hint })))
    $taskActionToolTip.SetToolTip($cancelRetryButton, (T $(if ($cancelRetryButton.Enabled) { 'action_hint_cancel' } else { $hint })))
    # A disabled action is neutral rather than blue; an available blue action
    # always has white text in both Windows light and dark themes.
    $retryNowButton.UseVisualStyleBackColor = $false
    $retryNowButton.FlatStyle = 'Flat'
    $retryNowButton.BackColor = if ($retryNowButton.Enabled) { [System.Drawing.ColorTranslator]::FromHtml('#0969B8') } else { [System.Drawing.ColorTranslator]::FromHtml('#67788A') }
    $retryNowButton.ForeColor = [System.Drawing.Color]::White
}

function Get-TaskSortValue {
    param($Task)
    switch ($script:taskSortColumn) {
        0 { return [string]$Task.label }
        1 { return [string]$Task.project_path }
        2 { return [string]$Task.task_type }
        3 { return Get-TaskStatusText $Task }
        4 {
            try { return [DateTimeOffset]::Parse([string]$Task.last_activity_at).ToUnixTimeMilliseconds() }
            catch { return [long]0 }
        }
        5 {
            try { return [DateTimeOffset]::Parse([string]$Task.due_at).ToUnixTimeMilliseconds() }
            catch { return [long]0 }
        }
        6 { return Get-ClassText ([string]$Task.class) }
    }
    return ''
}

function Update-TaskSortHeaders {
    $keys = @('col_task', 'col_project', 'col_task_type', 'col_status', 'col_last_activity', 'col_countdown', 'col_class')
    for ($index = 0; $index -lt $keys.Count; $index++) {
        $caption = T $keys[$index]
        if ($index -eq $script:taskSortColumn) {
            $caption += if ($script:taskSortDescending) { ' ▼' } else { ' ▲' }
        }
        $taskList.Columns[$index].Text = $caption
    }
}

function Get-ActiveQuotaWaitCount {
    param($Dashboard)
    if (-not $Dashboard -or -not $Dashboard.tasks) { return 0 }
    return @($Dashboard.tasks | Where-Object {
        [string]$_.status -eq 'pending' -and
        [string]$_.wait_reason -in @('quota_limited', 'quota_status_unavailable')
    }).Count
}

function Update-RuntimeView {
    $status = Read-JsonFile $statusPath
    $dashboard = Read-JsonFile $dashboardPath
    $taskPlan = Get-CurrentTaskPlan $dashboard
    if (-not $script:taskDragInProgress) {
        $taskSelectionHint.Text = if ([bool]$taskPlan.invalid) { T 'task_plan_invalid' } elseif (-not [bool]$taskPlan.configured) { T 'task_selection_legacy' } else { T 'task_selection_hint' }
        $taskSelectionHint.ForeColor = if ([bool]$taskPlan.invalid) { [System.Drawing.Color]::Firebrick } else { [System.Drawing.Color]::DimGray }
    }
    $currentControl = Read-JsonFile $controlPath
    $currentConfig = Read-JsonFile $configPath
    $refreshTasksButton.Text = if ($dashboard -and [bool]$dashboard.task_catalog_refreshing) { T 'task_refreshing' } else { T 'btn_refresh_tasks' }
    if ($currentConfig) {
        if ($autoScanCheck.Checked -ne [bool]$currentConfig.auto_scan_enabled) { $autoScanCheck.Checked = [bool]$currentConfig.auto_scan_enabled }
        if (-not $scanIntervalBox.Focused -and [int]$currentConfig.scan_interval_seconds -ge 1) { $scanIntervalBox.Value = [int]$currentConfig.scan_interval_seconds }
    }
    Update-UsageView $dashboard
    Update-GuideView $dashboard
    Update-DiagnosticsView $dashboard $status
    if ($currentConfig -and [int]$currentConfig.shared_app_server_port -gt 0) {
        $sharedPortValue.Text = (T 'shared_port_prefix') + [int]$currentConfig.shared_app_server_port
    }
    $running = $false
    if ($status -and [bool]$status.running -and [int]$status.pid -gt 0) {
        try {
            $process = Get-Process -Id ([int]$status.pid) -ErrorAction Stop
            $running = [string]::Equals($process.Path, $Executable, [System.StringComparison]::OrdinalIgnoreCase)
        } catch { $running = $false }
    }
    $script:resumeRouteAvailable = $running -and $dashboard -and [bool]$dashboard.automatic_recovery_supported
    $paused = if ($currentControl) { [bool]$currentControl.paused } else { $false }
    $autoDispatchButton.Text = if ($paused) { T 'resume_dispatch' } else { T 'pause_dispatch' }
    $historyError.Text = if ([string]$diagRecentErrorValue.Text -and [string]$diagRecentErrorValue.Text -ne (T 'diag_none')) {
        (T 'history_latest_error') + [string]$diagRecentErrorValue.Text
    } else { T 'history_none' }
    $historyErrorToolTip.SetToolTip($historyError, $historyError.Text)
    $monitoringPaused = if ($currentControl) { [bool]$currentControl.monitoring_paused } else { $false }
    $rateLimitedCount = Get-ActiveQuotaWaitCount $dashboard
    $cliFallbackReady = $currentConfig -and [bool]$currentConfig.cli_resume_fallback_enabled -and
        [string]$status.controller_state -in @('shared_app_server_disabled', 'codex_restart_required', 'codex_not_running', 'codex_app_not_ready')
    if (-not $running) {
        $serviceValue.Text = T 'status_not_running'
        $serviceValue.ForeColor = [System.Drawing.Color]::Firebrick
    } elseif ($monitoringPaused) {
        $serviceValue.Text = T 'status_monitoring_paused'
        $serviceValue.ForeColor = [System.Drawing.Color]::DimGray
    } elseif ($cliFallbackReady) {
        $serviceValue.Text = T 'status_cli_fallback_ready'
        $serviceValue.ForeColor = [System.Drawing.Color]::SeaGreen
    } elseif ([string]$status.controller_state -eq 'codex_restart_required') {
        $serviceValue.Text = T 'status_disconnected'
        $serviceValue.ForeColor = [System.Drawing.Color]::DarkOrange
    } elseif ([string]$status.controller_state -eq 'codex_not_running') {
        $serviceValue.Text = T 'status_exited'
        $serviceValue.ForeColor = [System.Drawing.Color]::Firebrick
    } elseif ([string]$status.controller_state -eq 'shared_app_server_disabled') {
        $serviceValue.Text = if (Get-SharedModeRequested (Read-JsonFile $configPath)) { T 'status_shared_temp_unavail' } else { T 'status_shared_disabled' }
        $serviceValue.ForeColor = [System.Drawing.Color]::DarkOrange
    } elseif ([string]$status.controller_state -eq 'shared_app_server_port_reserved') {
        $serviceValue.Text = T 'status_port_reserved'
        $serviceValue.ForeColor = [System.Drawing.Color]::Firebrick
    } elseif ([string]$status.controller_state -eq 'shared_app_server_port_conflict') {
        $serviceValue.Text = T 'status_port_conflict'
        $serviceValue.ForeColor = [System.Drawing.Color]::DarkOrange
    } elseif ([string]$status.controller_state -eq 'shared_app_server_migration_deferred') {
        $serviceValue.Text = T 'status_migration_deferred'
        $serviceValue.ForeColor = [System.Drawing.Color]::DarkOrange
    } elseif ([string]$status.controller_state -eq 'shared_app_server_config_invalid') {
        $serviceValue.Text = T 'status_config_invalid'
        $serviceValue.ForeColor = [System.Drawing.Color]::Firebrick
    } elseif ($paused) {
        $serviceValue.Text = T 'status_paused'
        $serviceValue.ForeColor = [System.Drawing.Color]::DarkOrange
    } elseif (-not $script:resumeRouteAvailable) {
        $serviceValue.Text = T 'status_channel_unavailable'
        $serviceValue.ForeColor = [System.Drawing.Color]::DarkOrange
    } elseif ($rateLimitedCount -gt 0) {
        $serviceValue.Text = T 'status_rate_limited'
        $serviceValue.ForeColor = [System.Drawing.Color]::DarkOrange
    } else {
        $serviceValue.Text = T 'status_running'
        $serviceValue.ForeColor = [System.Drawing.Color]::SeaGreen
    }

    if ($script:taskDragInProgress -or $script:taskPlanSaveScheduled -or $script:taskPlanSaveInProgress) { return }
    $selectedRows = @{}
    foreach ($selectedRow in $taskList.SelectedItems) { $selectedRows[[string]$selectedRow.Tag.ThreadID] = $true }
    $script:taskPlan = $taskPlan
    if ([bool]$taskPlan.configured -and -not $script:taskSortWasUserChosen) {
        $script:taskSortColumn = -1
        Update-TaskSortHeaders
    }
    $planOrder = @{}
    $planSelected = @{}
    $orderIndex = 0
    foreach ($id in (Get-UniqueTaskIDs @($taskPlan.ordered_thread_ids))) {
        $planOrder[$id] = $orderIndex
        $orderIndex++
    }
    foreach ($id in (Get-UniqueTaskIDs @($taskPlan.selected_thread_ids))) { $planSelected[$id] = $true }
    $orderedTasks = @()
    if ($dashboard -and $dashboard.tasks) {
        if ($script:taskSortColumn -eq -1) {
            $orderedTasks = @($dashboard.tasks | Sort-Object -Property @(
                @{ Expression = { if ($planOrder.ContainsKey([string]$_.thread_id)) { 0 } else { 1 } } },
                @{ Expression = { if ($planOrder.ContainsKey([string]$_.thread_id)) { [int]$planOrder[[string]$_.thread_id] } else { 0 } } },
                @{ Expression = { try { -[DateTimeOffset]::Parse([string]$_.last_activity_at).ToUnixTimeMilliseconds() } catch { [long]0 } } }
            ))
        } else {
            $orderedTasks = @($dashboard.tasks | Sort-Object -Property @{ Expression = { Get-TaskSortValue $_ }; Descending = $script:taskSortDescending })
        }
    }
    $sameRows = $taskList.Items.Count -eq $orderedTasks.Count
    if ($sameRows) {
        for ($index = 0; $index -lt $orderedTasks.Count; $index++) {
            if (-not [string]::Equals([string]$taskList.Items[$index].Tag.ThreadID,
                [string]$orderedTasks[$index].thread_id, [System.StringComparison]::OrdinalIgnoreCase)) {
                $sameRows = $false
                break
            }
        }
    }
    $topThreadID = ''
    $topIndex = 0
    if (-not $sameRows) {
        try {
            if ($taskList.TopItem) {
                $topThreadID = [string]$taskList.TopItem.Tag.ThreadID
                $topIndex = $taskList.TopItem.Index
            }
        } catch { }
        $taskList.BeginUpdate()
    }
    $script:taskListHydrating = $true
    try {
        if (-not $sameRows) { $taskList.Items.Clear() }
        $pendingCount = 0
        $activeCount = 0
        $stoppedCount = 0
        $nextSeconds = $null
        for ($rowIndex = 0; $rowIndex -lt $orderedTasks.Count; $rowIndex++) {
            $retry = $orderedTasks[$rowIndex]
            $threadID = [string]$retry.thread_id
            $rowState = [string]$retry.status
            $seconds = $null
            if ($rowState -eq 'pending') {
                $pendingCount++
                try {
                    $dueAt = [DateTimeOffset]::Parse([string]$retry.due_at)
                    $seconds = [Math]::Max(0, [Math]::Ceiling(($dueAt - [DateTimeOffset]::UtcNow).TotalSeconds))
                    if ($null -eq $nextSeconds -or $seconds -lt $nextSeconds) { $nextSeconds = $seconds }
                } catch { $seconds = 0 }
            } elseif ($rowState -in @('starting', 'running')) {
                $activeCount++
            } elseif ($rowState -eq 'stopped') {
                $stoppedCount++
            } elseif ($rowState -in @('rate_limited', 'quota_recovered')) {
                $stoppedCount++
            }
            $name = if ([string]$retry.label) { [string]$retry.label } else { 'Codex Task ' + $threadID.Substring(0, [Math]::Min(8, $threadID.Length)) }
            $project = if ([string]$retry.project_path) { [string]$retry.project_path } else { T 'usage_unavailable' }
            $taskType = if ([string]$retry.task_type) { [string]$retry.task_type } else { 'Codex' }
            $lastActivity = T 'usage_unavailable'
            if ([string]$retry.last_activity_at) {
                try { $lastActivity = [DateTimeOffset]::Parse([string]$retry.last_activity_at).ToLocalTime().ToString('MM/dd HH:mm') } catch { }
            }
            $countdown = if ($null -ne $seconds) { ([int]$seconds).ToString() + (T 'unit_second') } else { '--' }
            $stateText = Get-TaskStatusText $retry
            $displayName = if ($planOrder.ContainsKey($threadID)) { ('☰  {0:D2}  {1}' -f ([int]$planOrder[$threadID] + 1), $name) } else { '☰  ' + $name }
            $rowText = @($displayName, $project, $taskType, $stateText, $lastActivity,
                $countdown, (Get-ClassText ([string]$retry.class)))
            $item = if ($sameRows) { $taskList.Items[$rowIndex] } else { [System.Windows.Forms.ListViewItem]::new($displayName) }
            if ($sameRows) {
                for ($column = 0; $column -lt $rowText.Count; $column++) {
                    if ($item.SubItems[$column].Text -cne [string]$rowText[$column]) {
                        $item.SubItems[$column].Text = [string]$rowText[$column]
                    }
                }
            } else {
                for ($column = 1; $column -lt $rowText.Count; $column++) { [void]$item.SubItems.Add([string]$rowText[$column]) }
            }
            $tooltip = $name + [Environment]::NewLine + (Get-TaskStatusDetail $retry) + [Environment]::NewLine + (T 'task_drag_hint')
            if ($item.ToolTipText -cne $tooltip) { $item.ToolTipText = $tooltip }
            $item.Tag = [pscustomobject]@{
                ThreadID = $threadID
                State = $rowState
                CanResume = [bool]$retry.can_resume
                CanCancel = [bool]$retry.can_cancel
                ResumeAction = [string]$retry.resume_action
                RolloutPath = [string]$retry.rollout_path
                Class = [string]$retry.class
            }
            if (-not $sameRows) { [void]$taskList.Items.Add($item) }
            $shouldBeChecked = $planSelected.ContainsKey($threadID)
            if ($item.Checked -ne $shouldBeChecked) { $item.Checked = $shouldBeChecked }
            if (-not $sameRows -and $selectedRows.ContainsKey($threadID)) { $item.Selected = $true }
        }
    } finally {
        $script:taskListHydrating = $false
        if (-not $sameRows) { $taskList.EndUpdate() }
    }
    if (-not $sameRows -and $taskList.Items.Count -gt 0) {
        $restoreItem = $null
        if ($topThreadID) {
            foreach ($item in $taskList.Items) {
                if ([string]::Equals([string]$item.Tag.ThreadID, $topThreadID,
                    [System.StringComparison]::OrdinalIgnoreCase)) {
                    $restoreItem = $item
                    break
                }
            }
        }
        if (-not $restoreItem) { $restoreItem = $taskList.Items[[Math]::Min($topIndex, $taskList.Items.Count - 1)] }
        try { $taskList.TopItem = $restoreItem } catch { }
    }
    if (($pendingCount + $activeCount + $stoppedCount) -eq 0) {
        $queueValue.Text = T 'queue_empty'
    } else {
        $queueValue.Text = [string]::Format((T 'queue_summary'), $pendingCount, $activeCount, $stoppedCount)
    }
    if (-not $running) {
        $nextValue.Text = T 'next_waiting_service'
    } elseif ($paused -and $pendingCount -gt 0) {
        $nextValue.Text = T 'next_waiting_resume'
    } elseif ($null -ne $nextSeconds) {
        $nextValue.Text = [string]::Format((T 'next_seconds'), [int]$nextSeconds)
    } elseif ($activeCount -gt 0) {
        $nextValue.Text = T 'next_running'
    } else {
        $nextValue.Text = T 'next_none'
    }
    if ($status -and $status.last_scan_at) {
        try { $scanValue.Text = (T 'last_scan') + ([DateTimeOffset]::Parse([string]$status.last_scan_at).ToLocalTime().ToString('HH:mm:ss')) } catch { $scanValue.Text = '' }
    } else { $scanValue.Text = '' }
    if ($status -and [string]$status.controller_state -eq 'codex_restart_required') {
        if ($scanValue.Text) { $scanValue.Text += '  ·  ' }
        $scanValue.Text += T 'status_disconnected_hint'
    }
    $startupApproval = Get-StartupApprovalStatus
    if ($scanValue.Text) { $scanValue.Text += '  ·  ' }
    $scanValue.Text += switch ($startupApproval) {
        'enabled' { T 'startup_approval_enabled'; break }
        'disabled' { T 'startup_approval_disabled'; break }
        default { T 'startup_approval_unknown' }
    }
    $sharedRequested = Get-SharedModeRequested $currentConfig
    $needsSafeLaunch = $status -and [string]$status.controller_state -in @('codex_restart_required', 'codex_not_running', 'codex_app_not_ready')
    $safeLaunchButton.Enabled = -not $script:localCommandInProgress -and $sharedRequested -and $needsSafeLaunch
    Update-ActionButtons
}

function Invoke-TaskAction {
    param([string]$Action)
    if ($taskList.SelectedItems.Count -eq 0) { return }
    if ($Action -eq 'resume_selected' -and -not $script:resumeRouteAvailable) {
        [System.Windows.Forms.MessageBox]::Show((T 'msg_resume_channel_unavailable'), 'CodexWorkResumer', 'OK', 'Warning') | Out-Null
        return
    }
    $failed = $false
    foreach ($item in @($taskList.SelectedItems)) {
        $effectiveAction = $Action
        if ($Action -eq 'resume_selected') {
            if (-not [bool]$item.Tag.CanResume) { continue }
            $effectiveAction = [string]$item.Tag.ResumeAction
            if (-not $effectiveAction) { continue }
        } elseif ($Action -eq 'cancel_retry' -and -not [bool]$item.Tag.CanCancel) {
            continue
        }
        $environment = @{
            CODEX_AUTO_RETRY_ACTION = $effectiveAction
            CODEX_AUTO_RETRY_THREAD_ID = [string]$item.Tag.ThreadID
        }
        if ($effectiveAction -eq 'resume_task') {
            $environment.CODEX_AUTO_RETRY_ROLLOUT_PATH = [string]$item.Tag.RolloutPath
            $environment.CODEX_AUTO_RETRY_FAILURE_CLASS = [string]$item.Tag.Class
        }
        $exitCode = Start-LocalCommand 'control' $environment
        if ($exitCode -ne 0) { $failed = $true }
    }
    if ($failed) {
        [System.Windows.Forms.MessageBox]::Show((T 'msg_action_failed'), 'CodexWorkResumer', 'OK', 'Warning') | Out-Null
    }
    Start-Sleep -Milliseconds 250
    Update-RuntimeView
}

function Apply-Language {
    $form.Text = T 'form_title'
    $overviewPage.Text = if ($script:currentLanguage -eq 'en') { 'Overview' } else { '總覽' }
    $settingsPage.Text = if ($script:currentLanguage -eq 'en') { 'Settings' } else { '設定' }
    $guidePage.Text = T 'guide_tab'
    $diagnosticsPage.Text = T 'diagnostics_tab'
    $langButton.Text = T 'lang_button'
    $schedulerGroup.Text = T 'section_scheduler'
    $monitorGroup.Text = T 'section_monitor'
    $historyGroup.Text = T 'section_history'
    $schedulerIntro.Text = T 'scheduler_intro'
    $schedulerSequential.Text = T 'mode_sequential'
    $schedulerStaggered.Text = T 'mode_staggered'
    $parallelLimitLabel.Text = T 'parallel_limit'
    $schedulerSaveButton.Text = T 'scheduler_save'
    $monitorNote.Text = T 'monitor_control_note'
    $historySummary.Text = T 'history_summary'
    $historyExpandButton.Text = if ($historyError.Visible) { T 'history_collapse' } else { T 'history_expand' }
    $historyOpenLogs.Text = T 'btn_open_logs'
    $statusGroup.Text = T 'status_group'
    $usageGroup.Text = T 'usage_group'
    $queueGroup.Text = T 'section_queue'
    $taskSelectionHint.Text = T 'task_selection_hint'
    $refreshTasksButton.Text = T 'btn_refresh_tasks'
    $autoScanCheck.Text = T 'check_auto_scan'
    $scanEveryLabel.Text = T 'label_every'
    $scanSecondsLabel.Text = T 'label_seconds'
    $taskList.Columns[0].Text = T 'col_task'
    $taskList.Columns[0].Width = 150
    $taskList.Columns[1].Text = T 'col_project'
    $taskList.Columns[1].Width = 270
    $taskList.Columns[2].Text = T 'col_task_type'
    $taskList.Columns[2].Width = 60
    $taskList.Columns[3].Text = T 'col_status'
    $taskList.Columns[3].Width = 280
    $taskList.Columns[4].Text = T 'col_last_activity'
    $taskList.Columns[4].Width = 120
    $taskList.Columns[5].Text = T 'col_countdown'
    $taskList.Columns[5].Width = 80
    $taskList.Columns[6].Text = T 'col_class'
    $taskList.Columns[6].Width = 100
    Update-TaskSortHeaders
    $retryNowButton.Text = T 'btn_retry_now'
    $cancelRetryButton.Text = T 'btn_cancel_retry'
    $restartRetryButton.Text = T 'btn_restart_retry'
    $settingsGroup.Text = T 'settings_group'
    $enabledCheck.Text = T 'check_enabled'
    $monitoringCheck.Text = T 'check_monitoring'
    $sharedCheck.Text = T 'check_shared'
    $sharedDescription.Text = T 'shared_description'
    $cliFallbackCheck.Text = T 'check_cli_fallback'
    $cliFallbackDescription.Text = T 'cli_fallback_description'
    $safeLaunchButton.Text = T 'btn_safe_launch'
    $notificationsCheck.Text = T 'check_notifications'
    $promptLabel.Text = T 'label_prompt'
    $recoveryLabel.Text = T 'label_recovery'
    $consecutiveLabel.Text = T 'label_consecutive'
    $strategyLabel.Text = T 'label_strategy'
    $maxDelayLabel.Text = T 'label_max_delay'
    $incrementLabel.Text = T 'label_increment'
    $memoryLabel.Text = T 'label_memory'
    $resumeIntervalLabel.Text = T 'label_resume_interval'
    $usageRefreshLabel.Text = T 'label_usage_refresh'
    Update-SchedulerModeView
    $guideTitle.Text = T 'guide_title'
    $guideSource.Text = T 'guide_source'
    $guideScheduler.Text = T 'guide_scheduler'
    $guideTaskOutcomes.Text = T 'guide_task_outcomes'
    $guideList.Columns[0].Text = T 'guide_state'
    $guideList.Columns[1].Text = T 'guide_color'
    $guideList.Columns[2].Text = T 'guide_meaning'
    $guideList.Columns[3].Text = T 'guide_behavior'
    $guideList.Columns[4].Text = T 'guide_notice'
    $diagnosticsTitle.Text = T 'diagnostics_title'
    $diagnosticsPrivacy.Text = T 'diagnostics_privacy'
    $diagVersionLabel.Text = T 'diag_version'
    $diagUptimeLabel.Text = T 'diag_uptime'
    $diagServiceLabel.Text = T 'diag_service'
    $diagConnectionLabel.Text = T 'diag_connection'
    $diagMonitoringLabel.Text = T 'diag_monitoring'
    $diagAutoResumeLabel.Text = T 'diag_auto_resume'
    $diagLastScanLabel.Text = T 'diag_last_scan'
    $diagTaskCountLabel.Text = T 'diag_task_count'
    $diagUsageSourceLabel.Text = T 'diag_usage_source'
    $diagUsageRefreshLabel.Text = T 'diag_usage_refresh'
    $diagUsageAttemptLabel.Text = T 'diag_usage_attempt'
    $diagNextRefreshLabel.Text = T 'diag_next_refresh'
    $diagNextResumeLabel.Text = T 'diag_next_resume'
    $diagLogsLabel.Text = T 'diag_logs'
    $diagRecentErrorLabel.Text = T 'diag_recent_error'
    $diagDebugLabel.Text = T 'diag_debug'
    $diagRefreshButton.Text = T 'btn_diag_refresh'
    $diagOpenLogsButton.Text = T 'btn_open_logs'
    $diagCopyButton.Text = T 'btn_copy_diagnostics'
    $diagDebugButton.Text = if ($script:debugEnabled) { T 'btn_disable_debug' } else { T 'btn_enable_debug' }
    $saveButton.Text = if ($script:localCommandInProgress) { T 'busy_checking' } else { T 'btn_save' }
    $closeButton.Text = T 'btn_close'

    $prevStrategyIndex = $strategyBox.SelectedIndex
    $strategyBox.Items.Clear()
    [void]$strategyBox.Items.Add((T 'strategy_exponential'))
    [void]$strategyBox.Items.Add((T 'strategy_linear'))
    [void]$strategyBox.Items.Add((T 'strategy_fixed'))
    $strategyBox.SelectedIndex = if ($prevStrategyIndex -ge 0 -and $prevStrategyIndex -le 2) { $prevStrategyIndex } else { 0 }

    $guideList.Tag = $null

    Update-DelayPreview
    Update-RuntimeView
}

$langButton.add_Click({
    $script:currentLanguage = if ($script:currentLanguage -eq 'zh') { 'en' } else { 'zh' }
    try {
        $langConfig = [ordered]@{ language = $script:currentLanguage }
        [System.IO.File]::WriteAllText($uiLangPath, ($langConfig | ConvertTo-Json), [System.Text.UTF8Encoding]::new($false))
    } catch { }
    Apply-Language
})

$taskList.add_SelectedIndexChanged({ Update-ActionButtons })
$schedulerSequential.add_CheckedChanged({ Update-SchedulerModeView })
$schedulerStaggered.add_CheckedChanged({ Update-SchedulerModeView })
$schedulerSaveButton.add_Click({
    if ($script:localCommandInProgress) { return }
    $mode = if ($schedulerSequential.Checked) { 'sequential_completion' } else { 'staggered_start' }
    $schedulerSaveNotice.Text = T 'save_saving'
    $script:localCommandInProgress = $true
    $schedulerSaveButton.Enabled = $false
    try {
        $exitCode = Start-LocalCommand 'control' @{
            CODEX_AUTO_RETRY_ACTION = 'set_scheduler_settings'
            CODEX_AUTO_RETRY_QUOTA_RESUME_MODE = $mode
            CODEX_AUTO_RETRY_RESUME_INTERVAL_SECONDS = [string][int]$resumeIntervalBox.Value
            CODEX_AUTO_RETRY_MAX_PARALLEL_SESSIONS = [string][int]$parallelLimitBox.Value
            CODEX_AUTO_RETRY_USAGE_REFRESH_MINUTES = [string][int]$usageRefreshBox.Value
        } 5000
    } finally {
        $script:localCommandInProgress = $false
        $schedulerSaveButton.Enabled = $true
    }
    if ($exitCode -eq 0) {
        $schedulerSaveNotice.Text = T 'save_saved'
        $schedulerSaveNotice.ForeColor = [System.Drawing.Color]::SeaGreen
        Update-RuntimeView
    } else {
        $schedulerSaveNotice.Text = T 'save_fail_range'
        $schedulerSaveNotice.ForeColor = [System.Drawing.Color]::Firebrick
    }
})
$autoDispatchButton.add_Click({
    if ($script:localCommandInProgress) { return }
    $latestControl = Read-JsonFile $controlPath
    $pauseNext = -not $(if ($latestControl) { [bool]$latestControl.paused } else { $false })
    $pauseValue = if ($pauseNext) { 'true' } else { 'false' }
    $script:localCommandInProgress = $true
    $autoDispatchButton.Enabled = $false
    try {
        $exitCode = Start-LocalCommand 'control' @{
            CODEX_AUTO_RETRY_ACTION = 'set_paused'
            CODEX_AUTO_RETRY_PAUSED = $pauseValue
        } 5000
    } finally {
        $script:localCommandInProgress = $false
        $autoDispatchButton.Enabled = $true
    }
    if ($exitCode -eq 0) {
        # Keep the advanced Settings checkbox aligned with this direct control.
        $enabledCheck.Checked = -not $pauseNext
    } else {
        [System.Windows.Forms.MessageBox]::Show((T 'diag_action_failed'), 'CodexWorkResumer', 'OK', 'Warning') | Out-Null
    }
    Update-RuntimeView
})
$historyExpandButton.add_Click({
    $expanded = -not [bool]$historyError.Visible
    $historyError.Visible = $expanded
    $historyOpenLogs.Visible = $expanded
    $historyExpandButton.Text = if ($expanded) { T 'history_collapse' } else { T 'history_expand' }
    $historyGroup.Height = if ($expanded) { 158 } else { 72 }
    $overviewPage.AutoScrollMinSize = [System.Drawing.Size]::new(0, $(if ($expanded) { 1020 } else { 930 }))
})
$taskList.add_ColumnClick({
    param($sender, $eventArgs)
    if ($script:taskSortColumn -eq $eventArgs.Column) {
        $script:taskSortDescending = -not $script:taskSortDescending
    } else {
        $script:taskSortColumn = $eventArgs.Column
        $script:taskSortDescending = $eventArgs.Column -eq 4
    }
    $script:taskSortWasUserChosen = $true
    Update-TaskSortHeaders
    Update-RuntimeView
})
$taskList.add_ItemCheck({
    param($sender, $eventArgs)
    if ($script:taskListHydrating -or $script:taskPlanSaveInProgress -or $eventArgs.Index -lt 0) { return }
    if ([bool]$script:taskPlan.invalid) {
        $eventArgs.NewValue = $eventArgs.CurrentValue
        return
    }
    # ItemCheck runs before Checked changes. Defer one UI message so the
    # persisted selection reflects the final user-visible checkbox state.
    if (-not $script:taskPlanSaveScheduled) {
        $script:taskPlanSaveScheduled = $true
        [void]$form.BeginInvoke([Action]{ Save-VisibleTaskSelection })
    }
    $form.BeginInvoke([Action]{ Update-ActionButtons }) | Out-Null
})
$taskList.add_ItemDrag({
    param($sender, $eventArgs)
    if ($script:taskPlanSaveScheduled -or $script:taskPlanSaveInProgress -or [bool]$script:taskPlan.invalid) { return }
    $item = $eventArgs.Item
    if (-not $item -or -not $item.Tag -or -not [string]$item.Tag.ThreadID) { return }
    $script:taskDragInProgress = $true
    try {
        [void]$taskList.DoDragDrop([string]$item.Tag.ThreadID, [System.Windows.Forms.DragDropEffects]::Move)
    } finally {
        Clear-TaskDragCue
        $script:taskDragInProgress = $false
        Update-RuntimeView
    }
})
$taskList.add_DragEnter({
    param($sender, $eventArgs)
    $eventArgs.Effect = if ($eventArgs.Data.GetDataPresent([string])) {
        [System.Windows.Forms.DragDropEffects]::Move
    } else {
        [System.Windows.Forms.DragDropEffects]::None
    }
})
$taskList.add_DragOver({
    param($sender, $eventArgs)
    if (-not $eventArgs.Data.GetDataPresent([string])) {
        $eventArgs.Effect = [System.Windows.Forms.DragDropEffects]::None
        Clear-TaskDragCue
        return
    }
    $sourceID = [string]$eventArgs.Data.GetData([string])
    $point = $taskList.PointToClient([System.Drawing.Point]::new($eventArgs.X, $eventArgs.Y))
    $candidate = Get-TaskDropCandidate $sourceID $point
    $eventArgs.Effect = if ($candidate) {
        [System.Windows.Forms.DragDropEffects]::Move
    } else {
        [System.Windows.Forms.DragDropEffects]::None
    }
    Set-TaskDragCue $candidate
})
$taskList.add_DragLeave({ Clear-TaskDragCue })
$taskList.add_DragDrop({
    param($sender, $eventArgs)
    try {
        if (-not $eventArgs.Data.GetDataPresent([string])) { return }
        $sourceID = [string]$eventArgs.Data.GetData([string])
        $point = $taskList.PointToClient([System.Drawing.Point]::new($eventArgs.X, $eventArgs.Y))
        $candidate = Get-TaskDropCandidate $sourceID $point
        if (-not $candidate) { return }
        $visible = [System.Collections.Generic.List[string]]::new()
        foreach ($item in $taskList.Items) { $visible.Add([string]$item.Tag.ThreadID) }
        if (-not $visible.Remove($sourceID)) { return }
        $visible.Insert([int]$candidate.MoveIndex, $sourceID)
        Save-ReorderedTaskList ($visible.ToArray())
    } finally {
        Clear-TaskDragCue
    }
})
$retryNowButton.add_Click({ Invoke-TaskAction 'resume_selected' })
$cancelRetryButton.add_Click({ Invoke-TaskAction 'cancel_retry' })
$refreshTasksButton.add_Click({
    $exitCode = Start-LocalCommand 'diagnostics' @{ CODEX_AUTO_RETRY_ACTION = 'refresh_tasks' } 5000
    if ($exitCode -eq 0) {
        $refreshTasksButton.Text = T 'task_refreshing'
        Start-Sleep -Milliseconds 200
        Update-RuntimeView
    }
})
$saveTaskScanSettings = {
    if ($script:localCommandInProgress) { return }
    [void](Start-LocalCommand 'diagnostics' @{
        CODEX_AUTO_RETRY_ACTION = 'set_task_scan'
        CODEX_AUTO_RETRY_AUTO_SCAN_ENABLED = [string][bool]$autoScanCheck.Checked
        CODEX_AUTO_RETRY_SCAN_INTERVAL_SECONDS = [string][int]$scanIntervalBox.Value
    } 5000)
}.GetNewClosure()
$autoScanCheck.add_CheckedChanged({ & $saveTaskScanSettings })
$scanIntervalBox.add_Validated({ & $saveTaskScanSettings })
$restartRetryButton.add_Click({
    if ($script:taskPlanSaveScheduled -or $script:taskPlanSaveInProgress -or [bool]$script:taskPlan.invalid) { return }
    $allChecked = $taskList.Items.Count -gt 0 -and $taskList.CheckedItems.Count -eq $taskList.Items.Count
    $script:taskListHydrating = $true
    try {
        foreach ($item in $taskList.Items) { $item.Checked = -not $allChecked }
    } finally { $script:taskListHydrating = $false }
    $script:taskPlanSaveScheduled = $true
    Save-VisibleTaskSelection
})
$safeLaunchButton.add_Click({ Start-SafeCodexLaunch })
$diagRefreshButton.add_Click({
    $exitCode = Start-LocalCommand 'diagnostics' @{ CODEX_AUTO_RETRY_ACTION = 'refresh_usage' } 5000
    if ($exitCode -eq 0) {
        $diagNotice.Text = T 'diag_refresh_requested'
        $diagNotice.ForeColor = [System.Drawing.Color]::SeaGreen
        Start-Sleep -Milliseconds 250
        Update-RuntimeView
    } else {
        $diagNotice.Text = T 'diag_action_failed'
        $diagNotice.ForeColor = [System.Drawing.Color]::Firebrick
    }
})
$openLogsAction = {
    try {
        $logDirectory = Join-Path $DataDir 'logs'
        [void][System.IO.Directory]::CreateDirectory($logDirectory)
        Start-Process -FilePath 'explorer.exe' -ArgumentList @($logDirectory)
    } catch {
        $diagNotice.Text = T 'diag_action_failed'
        $diagNotice.ForeColor = [System.Drawing.Color]::Firebrick
    }
}
$diagOpenLogsButton.add_Click($openLogsAction)
$historyOpenLogs.add_Click($openLogsAction)
$diagCopyButton.add_Click({ Copy-DiagnosticsToClipboard })
$diagDebugButton.add_Click({
    $action = if ($script:debugEnabled) { 'disable_debug' } else { 'enable_debug' }
    $exitCode = Start-LocalCommand 'diagnostics' @{ CODEX_AUTO_RETRY_ACTION = $action } 5000
    if ($exitCode -eq 0) {
        $diagNotice.Text = if ($action -eq 'enable_debug') { T 'diag_debug_enabled_msg' } else { T 'diag_debug_disabled_msg' }
        $diagNotice.ForeColor = [System.Drawing.Color]::SeaGreen
        Start-Sleep -Milliseconds 250
        Update-RuntimeView
    } else {
        $diagNotice.Text = T 'diag_action_failed'
        $diagNotice.ForeColor = [System.Drawing.Color]::Firebrick
    }
})
$strategyBox.add_SelectedIndexChanged({ Update-DelayPreview })
$initialDelayBox.add_ValueChanged({ Update-DelayPreview })
$maxDelayBox.add_ValueChanged({ Update-DelayPreview })
$incrementBox.add_ValueChanged({ Update-DelayPreview })
$consecutiveBox.add_ValueChanged({ Update-DelayPreview })
$closeButton.add_Click({ $form.Close() })
$saveButton.add_Click({
    if ($script:localCommandInProgress) { return }
    $prompt = $promptBox.Text.Trim()
    if (-not $prompt) {
        [System.Windows.Forms.MessageBox]::Show((T 'msg_prompt_empty'), 'CodexWorkResumer', 'OK', 'Warning') | Out-Null
        return
    }
    $delayStrategy = Get-DelayStrategy
    if ($delayStrategy -ne 'fixed' -and [int]$maxDelayBox.Value -lt [int]$initialDelayBox.Value) {
        [System.Windows.Forms.MessageBox]::Show((T 'msg_max_less_initial'), 'CodexWorkResumer', 'OK', 'Warning') | Out-Null
        return
    }
    $quotaMode = if ($schedulerSequential.Checked) { 'sequential_completion' } else { 'staggered_start' }
    $payload = [ordered]@{
        retry_prompt = $prompt
        max_recovery_attempts = [int]$recoveryBox.Value
        max_consecutive_retries = [int]$consecutiveBox.Value
        initial_delay_seconds = [int]$initialDelayBox.Value
        max_delay_seconds = [int]$maxDelayBox.Value
        delay_increment_seconds = [int]$incrementBox.Value
        memory_limit_mb = [int]$memoryBox.Value
        resume_interval_seconds = [int]$resumeIntervalBox.Value
        quota_resume_mode = $quotaMode
        max_parallel_sessions = [int]$parallelLimitBox.Value
        usage_refresh_minutes = [int]$usageRefreshBox.Value
        cli_resume_fallback_enabled = [bool]$cliFallbackCheck.Checked
        delay_strategy = $delayStrategy
        show_notifications = [bool]$notificationsCheck.Checked
        paused = -not [bool]$enabledCheck.Checked
        monitoring_paused = -not [bool]$monitoringCheck.Checked
        shared_app_server_enabled = [bool]$sharedCheck.Checked
    }
    $currentConfig = Read-JsonFile $configPath
    $storedSharedEnabled = if ($currentConfig) { Get-SharedModeRequested $currentConfig } else { Get-SharedModeRequested $config }
    $storedSharedPort = if ($currentConfig) { [int]$currentConfig.shared_app_server_port } else { [int]$config.shared_app_server_port }
    $sharedModeChanged = [bool]$sharedCheck.Checked -ne $storedSharedEnabled
    $sharedModeEnabling = [bool]$sharedCheck.Checked -and -not $storedSharedEnabled
    $sharedModeChecking = [bool]$sharedCheck.Checked
    $requestPath = Join-Path $DataDir ('settings-request-' + [guid]::NewGuid().ToString('N') + '.json')
    $script:localCommandInProgress = $true
    Set-SettingsBusy $true
    if ($sharedModeChecking) {
        $noticeLabel.Text = T 'save_checking_health'
    } elseif ($sharedModeChanged) {
        $noticeLabel.Text = T 'save_closing_shared'
    } else {
        $noticeLabel.Text = T 'save_saving'
    }
    try {
        [System.IO.File]::WriteAllText($requestPath, ($payload | ConvertTo-Json -Depth 4), [System.Text.UTF8Encoding]::new($false))
        $exitCode = Start-LocalCommand 'save-settings' @{
            CODEX_AUTO_RETRY_SETTINGS_FILE = $requestPath
        }
        if ($exitCode -eq -2) {
            throw (T 'save_timeout')
        }
        if ($exitCode -ne 0) { throw (T 'save_validation_failed') }
        $noticeLabel.Text = T 'save_saved'
        $noticeLabel.ForeColor = [System.Drawing.Color]::SeaGreen
        Update-RuntimeView
    } catch {
        if ($sharedModeChanged) {
            $latestConfig = Read-JsonFile $configPath
            if ($latestConfig) {
                $sharedCheck.Checked = Get-SharedModeRequested $latestConfig
            } else {
                $sharedCheck.Checked = $storedSharedEnabled
            }
        }
        if ($sharedModeEnabling -and $exitCode -eq $localCommandExitPortReserved) {
            $noticeLabel.Text = [string]::Format((T 'save_fail_reserved'), $storedSharedPort)
        } elseif ($sharedModeEnabling -and $exitCode -eq $localCommandExitPortConflict) {
            $noticeLabel.Text = [string]::Format((T 'save_fail_conflict'), $storedSharedPort)
        } elseif ($script:localCommandTimedOut) {
            $noticeLabel.Text = T 'save_fail_timeout'
        } elseif ($sharedModeEnabling) {
            $noticeLabel.Text = T 'save_fail_health'
        } elseif ($sharedModeChanged) {
            $noticeLabel.Text = T 'save_fail_close'
        } else {
            $noticeLabel.Text = T 'save_fail_range'
        }
        $noticeLabel.ForeColor = [System.Drawing.Color]::Firebrick
    } finally {
        Remove-Item -LiteralPath $requestPath -Force -ErrorAction SilentlyContinue
        $script:localCommandInProgress = $false
        Set-SettingsBusy $false
    }
})

$form.add_FormClosing({
    if ($script:localCommandProcess) {
        Stop-LocalCommandProcess $script:localCommandProcess
    }
})

$timer = [System.Windows.Forms.Timer]::new()
$timer.Interval = if ($SmokeTest) { 100 } else { 1000 }
$smokeDeadline = [DateTimeOffset]::UtcNow.AddSeconds(8)
$timer.add_Tick({
    try {
        $script:refreshFrame++
        if (Test-SettingsMemoryLimit) { return }
        if (([DateTimeOffset]::UtcNow - $script:lastThemeCheck).TotalSeconds -ge 30) { Apply-AppTheme }
        Complete-SafeCodexLaunch
        if ($SmokeTest) {
            $signalSeen = Test-Path -LiteralPath $smokeClosePath
            $deadlineReached = [DateTimeOffset]::UtcNow -ge $smokeDeadline
            if ($signalSeen -or $deadlineReached) {
                $form.Close()
                return
            }
            return
        }
        Update-RuntimeView
    } catch {
        if ($SmokeTest) {
            [System.IO.File]::WriteAllText(
                (Join-Path $DataDir 'settings-smoke-error.txt'),
                ($_ | Out-String),
                [System.Text.UTF8Encoding]::new($false)
            )
            $form.Close()
            return
        }
        throw
    }
})
$form.add_Shown({
    try {
    Apply-Language
    Apply-AppTheme
    Update-RuntimeView
    if ($SmokeTest -and (Test-Path -LiteralPath (Join-Path $DataDir 'settings-list-fixture.flag'))) {
        if ($taskList.Items.Count -lt 30) { throw 'The Session list fixture did not load.' }
        # Repeated refreshes must keep the same rows and the user's position.
        $smokeRow = $taskList.Items[20]
        $smokeThreadID = [string]$smokeRow.Tag.ThreadID
        $smokeRow.Selected = $true
        $taskList.TopItem = $smokeRow
        [System.Windows.Forms.Application]::DoEvents()
        $smokeTopID = [string]$taskList.TopItem.Tag.ThreadID
        Update-RuntimeView
        if (-not [object]::ReferenceEquals($smokeRow, $taskList.Items[20]) -or
            [string]$taskList.TopItem.Tag.ThreadID -ne $smokeTopID -or
            -not $smokeRow.Selected) {
            throw 'A routine Session refresh replaced rows or reset selection and scroll.'
        }
        $smokeOrder = @($taskList.Items | ForEach-Object { [string]$_.Tag.ThreadID })
        $smokePlan = [ordered]@{
            version = 1
            revision = 1
            configured = $true
            ordered_thread_ids = $smokeOrder
            selected_thread_ids = @($smokeThreadID)
        }
        [System.IO.File]::WriteAllText(
            $taskPlanPath,
            (ConvertTo-Json -InputObject $smokePlan -Depth 4),
            [System.Text.UTF8Encoding]::new($false)
        )
        Update-RuntimeView
        if (-not [object]::ReferenceEquals($smokeRow, $taskList.Items[20]) -or
            [string]$taskList.TopItem.Tag.ThreadID -ne $smokeTopID -or
            -not $smokeRow.Selected -or -not $smokeRow.Checked -or
            $smokeRow.Text -match 'Codex Task ') {
            throw 'Selecting a watched Session rebuilt the list or hid its official title.'
        }
        [System.IO.File]::WriteAllText(
            (Join-Path $DataDir 'settings-list-smoke.ok'),
            'scroll-and-selection-preserved',
            [System.Text.UTF8Encoding]::new($false)
        )
        $timer.Start()
        return
    }
    if ($SmokeTest) {
        if ($tabControl.TabPages.Count -ne 4 -or $guideList.Items.Count -lt 8 -or $null -eq $monitoringCheck -or $null -eq $diagDebugButton -or $diagnosticValueLabels.Count -lt 16) {
            throw 'Settings, status guide, or diagnostics page did not initialize.'
        }
        if (-not $overviewPage.AutoScroll -or
            $queueGroup.Parent -ne $overviewPage -or $schedulerGroup.Parent -ne $overviewPage -or
            $monitorGroup.Parent -ne $overviewPage -or $historyGroup.Parent -ne $overviewPage -or
            $statusGroup.Parent -ne $monitorGroup -or $usageGroup.Parent -ne $monitorGroup) {
            throw 'The main page is not a scrollable four-section layout.'
        }
        if ($form.FormBorderStyle -ne [System.Windows.Forms.FormBorderStyle]::Sizable -or
            $tabControl.Bottom -gt $form.ClientSize.Height -or
            -not $settingsPage.AutoScroll -or -not $guidePage.AutoScroll -or -not $diagnosticsPage.AutoScroll) {
            throw 'The main window or its tabs cannot resize vertically without clipping.'
        }
        $originalHeight = $tabControl.Height
        $form.ClientSize = [System.Drawing.Size]::new(1000, 540)
        [System.Windows.Forms.Application]::DoEvents()
        if ($tabControl.Height -ge $originalHeight -or $tabControl.Bottom -gt $form.ClientSize.Height) {
            throw 'The tab content did not follow vertical window resizing.'
        }
        $form.ClientSize = [System.Drawing.Size]::new(1000, 720)
        [System.Windows.Forms.Application]::DoEvents()
        foreach ($section in @($queueGroup, $schedulerGroup, $monitorGroup, $historyGroup)) {
            if ($section.Right -gt $overviewPage.DisplayRectangle.Width) {
                throw "An overview section extends beyond its scrollable width: $($section.Text)"
            }
        }
        if ($usage5Progress -is [System.Windows.Forms.ProgressBar] -or $usage7Progress -is [System.Windows.Forms.ProgressBar] -or
            $usage5Progress.Controls.Count -ne 1 -or $usage7Progress.Controls.Count -ne 1) {
            throw 'An animated quota bar or manual completion control remains.'
        }
        $stoppedWatched = [pscustomobject]@{ status = 'stopped'; stop_reason = 'controller_failed'; selected_for_monitoring = $true; info_unavailable = $false; last_recovery_succeeded_at = '' }
        $catalogStoppedWatched = [pscustomobject]@{ status = 'stopped'; stop_reason = ''; selected_for_monitoring = $true; info_unavailable = $false; last_recovery_succeeded_at = '' }
        $recovered = [pscustomobject]@{ status = 'completed'; stop_reason = ''; selected_for_monitoring = $true; info_unavailable = $false; last_recovery_succeeded_at = '2026-10-11T10:00:00Z' }
        $quotaWait = [pscustomobject]@{ status = 'pending'; class = 'rate_limit'; wait_reason = 'quota_limited' }
        $quotaCheck = [pscustomobject]@{ status = 'pending'; class = 'rate_limit'; wait_reason = 'quota_status_unavailable' }
        $ordinaryRateLimit = [pscustomobject]@{ status = 'pending'; class = 'rate_limit'; wait_reason = '' }
        if ((Get-TaskStatusText $stoppedWatched) -ne (T 'task_stopped_watched') -or
            (Get-TaskStatusText $catalogStoppedWatched) -ne (T 'task_stopped_catalog_watched') -or
            (Get-TaskStatusText $recovered) -ne (T 'task_recovery_success') -or
            (Get-TaskStatusText $quotaWait) -ne (T 'task_quota_wait') -or
            (Get-TaskStatusText $quotaCheck) -ne (T 'task_quota_check') -or
            (Get-TaskStatusText $ordinaryRateLimit) -ne (T 'task_retry_pending')) {
            throw 'Stopped monitoring and verified resume outcomes are not distinct in the task list.'
        }
        $dragRowBounds = [System.Drawing.Rectangle]::new(0, 20, 100, 20)
        if ((Get-TaskDropSlotForRow 2 $dragRowBounds 29) -ne 2 -or
            (Get-TaskDropSlotForRow 2 $dragRowBounds 30) -ne 3) {
            throw 'Drag target upper and lower halves do not resolve to distinct insertion slots.'
        }
        foreach ($case in @(
            @(1, 0, 4, 0), @(1, 1, 4, -1), @(1, 2, 4, -1),
            @(1, 3, 4, 2), @(1, 4, 4, 3), @(3, 0, 4, 0),
            @(0, 4, 4, 3), @(-1, 1, 4, -1), @(0, 5, 4, -1)
        )) {
            if ((Get-TaskMoveIndex $case[0] $case[1] $case[2]) -ne $case[3]) {
                throw "Drag insertion index is wrong for source $($case[0]), slot $($case[1]), count $($case[2])."
            }
        }
        $dragCueItem = [System.Windows.Forms.ListViewItem]::new('☰  Example')
        $dragCueItem.ToolTipText = 'Example task'
        Set-TaskDragCue ([pscustomobject]@{ Target = $dragCueItem; After = $true })
        if ($taskSelectionHint.Text -ne ((T 'task_drag_after') -f 'Example task')) {
            throw 'The drag destination is not displayed in the current language.'
        }
        Clear-TaskDragCue
        if ($taskSelectionHint.Text -match 'Example task') { throw 'The drag destination remains visible after the drag ends.' }
        foreach ($language in @('zh', 'en')) {
            foreach ($key in @(
                'task_stopped_watched', 'task_stopped_unwatched', 'task_stopped_catalog',
                'task_stopped_catalog_watched', 'task_quota_wait', 'task_quota_check', 'task_retry_pending',
                'task_retry_running', 'task_recovery_success', 'task_recovery_handoff'
            )) {
                $width = [System.Windows.Forms.TextRenderer]::MeasureText([string]$script:i18n[$key][$language], $taskList.Font).Width
                if ($width -gt ($taskList.Columns[3].Width - 8)) {
                    throw "The task status column clips $key/${language}: $width pixels."
                }
            }
        }
        $smokeControl = Read-JsonFile $controlPath
        $expectedPauseText = if ($smokeControl -and [bool]$smokeControl.paused) { T 'resume_dispatch' } else { T 'pause_dispatch' }
        if ($autoDispatchButton.Text -ne $expectedPauseText) {
            throw 'The main pause button does not reflect persisted pause state.'
        }
        $wasSequential = $schedulerSequential.Checked
        $schedulerSequential.Checked = $true
        if ($resumeIntervalBox.Visible -or $parallelLimitBox.Visible -or -not $usageRefreshBox.Visible) {
            throw 'Sequential mode did not hide only the interval parameters.'
        }
        $schedulerStaggered.Checked = $true
        if (-not $resumeIntervalBox.Visible -or -not $parallelLimitBox.Visible -or -not $usageRefreshBox.Visible) {
            throw 'Staggered mode did not restore its parameters.'
        }
        $schedulerSequential.Checked = $wasSequential
        $longestStatus = [System.Windows.Forms.TextRenderer]::MeasureText((T 'status_shared_temp_unavail'), $serviceValue.Font)
        if ($serviceValue.Bottom -gt $queueValue.Top -or $queueValue.Bottom -gt $scanValue.Top -or
            $statusGroup.Bottom -gt $usageGroup.Top -or
            $monitorGroup.Bottom -gt $queueGroup.Top -or $queueGroup.Bottom -gt $schedulerGroup.Top -or
            $schedulerGroup.Bottom -gt $historyGroup.Top -or $historyGroup.Bottom -gt $overviewPage.AutoScrollMinSize.Height -or
            $longestStatus.Width -gt $serviceValue.ClientSize.Width) {
            throw 'The four overview sections or status labels overlap or clip their longest status text.'
        }
        if ($taskList.Bottom -gt $taskSelectionHint.Top -or $taskSelectionHint.Bottom -gt $refreshTasksButton.Top) {
            throw 'The task selection hint overlaps the list or action buttons.'
        }
        if ($refreshTasksButton.Bottom -gt $taskActionHint.Top -or
            $schedulerSaveButton.Right -gt $schedulerGroup.ClientSize.Width -or
            $autoDispatchButton.Right -gt $monitorGroup.ClientSize.Width) {
            throw 'The new overview controls overlap or extend outside their sections.'
        }
        foreach ($key in @('task_selection_hint', 'task_selection_legacy', 'task_plan_invalid', 'task_drag_before', 'task_drag_after')) {
            foreach ($language in @('zh', 'en')) {
                $sample = [string]$script:i18n[$key][$language]
                if ($key -in @('task_drag_before', 'task_drag_after')) { $sample = $sample -f ('測' * 31 + '…') }
                $measured = [System.Windows.Forms.TextRenderer]::MeasureText($sample, $taskSelectionHint.Font)
                if ($measured.Width -gt $taskSelectionHint.ClientSize.Width) {
                    throw "The task selection hint is clipped: $key/$language"
                }
            }
        }
        foreach ($language in @('zh', 'en')) {
            foreach ($field in @(@('guide_source', $guideSource), @('guide_scheduler', $guideScheduler), @('guide_task_outcomes', $guideTaskOutcomes))) {
                $measured = [System.Windows.Forms.TextRenderer]::MeasureText([string]$script:i18n[$field[0]][$language], $field[1].Font)
                if ($measured.Width -gt $field[1].ClientSize.Width) {
                    throw "The status guide description is clipped: $($field[0])/$language"
                }
            }
            foreach ($field in @(
                @('scheduler_intro', $schedulerIntro),
                @('scheduler_scope', $schedulerScope),
                @('scheduler_scope_sequential', $schedulerScope),
                @('monitor_control_note', $monitorNote),
                @('history_summary', $historySummary)
            )) {
                $width = [System.Windows.Forms.TextRenderer]::MeasureText([string]$script:i18n[$field[0]][$language], $field[1].Font).Width
                if ($width -gt $field[1].ClientSize.Width) { throw "Overview text is clipped: $($field[0])/$language" }
            }
        }
        [System.IO.File]::WriteAllText(
            (Join-Path $DataDir 'settings-smoke.ok'),
            'passed',
            [System.Text.UTF8Encoding]::new($false)
        )
        [System.IO.File]::WriteAllText(
            (Join-Path $DataDir 'settings-layout-smoke.ok'),
            'separated',
            [System.Text.UTF8Encoding]::new($false)
        )
    }
    $timer.Start()
    } catch {
        if ($SmokeTest) {
            [System.IO.File]::WriteAllText(
                (Join-Path $DataDir 'settings-smoke-error.txt'),
                ($_ | Out-String),
                [System.Text.UTF8Encoding]::new($false)
            )
            $form.Close()
            return
        }
        throw
    }
})
$form.add_FormClosed({ $timer.Stop(); $timer.Dispose() })
[void]$form.ShowDialog()
