# CodexWorkResumer：`sybxxx/codex-auto-retry` 架構研究與 Fork／重寫評估

研究日期：2026-09-19

研究範圍：`sybxxx/codex-auto-retry` 的 `main` 分支原始碼、最新 GitHub Release、OpenAI 官方 Codex app-server 文件，以及本機 Codex app-server rate-limit 實際回傳結構。
研究基線：原始碼中的 `appVersion` 是 `0.7.11`；GitHub 最新 Release 也是 `v0.7.11`，但 Release 頁顯示 `main` 已比該 tag 多 11 個 commits。因此以下結論以研究日下載的 `main` 為準，不把 Release ZIP 與 `main` 視為完全相同。

> 本文件只完成研究與設計判斷，沒有修改 upstream 程式、安裝 Go／Node.js、建立正式專案根目錄或操作 Hermes。

> **2026-09-22 實作註記：**上句描述的是 2026-09-19 的 upstream 研究階段。現在的 Fork 已進入 **v1.1.0（Build 4）**：保留 Desktop IPC 優先路徑，加入 `account/rateLimits/read` 額度查詢、10 分鐘內 rollout snapshot fallback、非阻塞 tray 更新，以及預設關閉的同 session CLI Resume fallback。ChatGPT Work 仍無已驗證的正式列出／Resume 介面，因此維持不支援；Unsnooze 只作比較研究，未整合進程式。

> **2026-10-01 Build 6 實作註記：**主畫面透過官方 app-server `thread/list` 加上每個 Task 的最新 turn 結構化狀態，顯示最近使用的 10 個 Codex Task，並永遠合併所有恢復佇列項目。`usageLimitExceeded`、`rateLimitExceeded` 與 `sessionBudgetExceeded` 會明確顯示為「5H／額度暫停」並進入 quota-aware 等待。2 秒 scheduler heartbeat 不變，但完整 Session 掃描改為可調、預設 60 秒，避免長期常駐時反覆遍歷數百 MB rollout。額度更新期間保留上一筆有效畫面並顯示更新動畫；正式更新失敗才顯示 N/A。釘選狀態尚無已驗證、可由獨立 app-server 穩定對應 Windows Codex 側邊欄的介面，因此目前不宣稱支援「只列釘選」。

> **2026-10-07 未發行效能註記：**保留 2 秒排程與 1 秒畫面倒數，但管理畫面的 `state.json` 改為檔案變動或最多 5 秒後重讀；未改變的 `status.json` 心跳每 10 秒寫一次，操作狀態變化仍立即發布。共享後台健康時探測間隔 30 秒，故障時 10 秒；額度查詢仍依使用者設定，預設 1 分鐘。以下對原版的 2 秒全掃描／寫檔分析仍是歷史研究，不代表目前 Fork 行為。

> **2026-09-30 Build 5 實作註記：**程式啟動時會真正套用上次儲存的「Codex自動續跑助手」啟用／停用狀態，而非只還原核取方塊；主畫面分開顯示續跑通道是否可用與目前有無待續跑工作。系統匣單擊切換 Mini Monitor、雙擊開啟主畫面、右鍵維持控制選單；Mini Monitor 繼續儲存顯示狀態、位置、模式與最上層設定。

## 一、先給結論

1. **原專案是 Go 寫的 Windows 常駐 watchdog**。它另有一個登入後啟動的 supervisor、watchdog 內建的原生 Win32 tray、按需啟動的 PowerShell WinForms 設定畫面，以及按需啟動的 Go MCP 管理面板。
2. **scanner 是 polling，不是 event-driven**。預設每 **2 秒**完整走訪所有 `sessions` 與 `archived_sessions` 目錄；對每個 rollout JSONL 只讀上次 byte offset 之後新增的內容。
3. **現有 rate-limit 處理只是一般 retry**。HTTP 429 或文字命中 rate-limit/capacity 關鍵字後，沿用 5、10、20、40…秒的退避（預設上限 300 秒），且整條自動恢復最多 30 分鐘。它不讀 `resetsAt`，所以不符合 quota-aware Resume。
4. **原專案目前不能顯示 5H／Weekly**。程式裡沒有 `account/rateLimits/read`、`account/rateLimits/updated`、`usedPercent` 或 `resetsAt`。但是官方 app-server 已提供這些欄位；本機實測也確實回傳 300 分鐘與 10080 分鐘兩個 window，所以新增功能在 Codex 路徑上可行。
5. **目前找不到可讓這個外部 Windows 工具直接列出或 Resume ChatGPT Work task 的正式公開介面**。Codex app-server 的 thread API 是 Codex thread surface，列出的 `sourceKinds` 也沒有 Work。Work 必須保持獨立 adapter；在沒有正式介面前只能標為不支援，或另做明確 opt-in 的 Windows UI Automation 實驗路徑。
6. **建議 Fork，不建議重寫**。核心恢復、安全、IPC、queue、tray、部署和測試大多可以沿用；重寫最危險的部分正是原作者已花最多成本處理的 thread correlation、Windows named pipe、goal/subagent、防重複與 fail-open 行為。
7. **正式方案應保留 upstream remote**，把新增功能盡量放在新檔案／adapter 中，只在 daemon、state、management、tray 與 UI 入口做小範圍接點，降低未來同步衝突。

## 二、原專案架構

主要資料流如下：

```text
Codex Desktop
  └─ rollout JSONL（sessions / archived_sessions）
       └─ 每 2 秒 polling scanner
            └─ 事件解析與故障分類
                 └─ 每個 thread 一份 retry state
                      └─ due queue（預設最多 4 個同時 dispatch）
                           ├─ 官方 Windows named pipe：\\.\pipe\codex-ipc
                           │    └─ owner discovery → thread-follower-start-turn
                           └─ 可選 shared app-server WebSocket
                                └─ thread/resume → 狀態複查 → turn/start

Windows 登入
  └─ supervisor process
       └─ watchdog worker process
            ├─ scanner / queue / state / status / log
            ├─ Win32 System Tray
            └─ memory guard

按需 UI
  ├─ PowerShell WinForms 設定畫面
  └─ Go MCP server + 內嵌 TypeScript/HTML 管理面板
```

核心檔案：

- `daemon.go`：2 秒主迴圈、掃描、排程、dispatch、狀態寫入。
- `scanner.go` / `events.go`：rollout JSONL 增量讀取與 lifecycle event 解析。
- `classifier.go`：錯誤分類。
- `retry_state.go` / `state.go`：每個 thread 的 pending、awaiting、stopped 狀態、防重複與持久化。
- `runner.go` / `shared_controller.go`：恢復策略與 app-server 操作。
- `codex_ipc_windows.go`：官方 Windows named pipe owner route。
- `app_server_rpc.go`：shared app-server WebSocket JSON-RPC client。
- `tray_windows.go`：原生 Win32 tray。
- `management.go` / `mcp_server.go`：檔案式管理 service 與 MCP 工具／面板。
- `ui/settings.ps1`：Windows Forms 設定視窗。
- `supervisor.go`：worker 異常退出後的重啟。

原始碼入口：[GitHub repository](https://github.com/sybxxx/codex-auto-retry)、[upstream architecture](https://github.com/sybxxx/codex-auto-retry/blob/main/docs/architecture.md)、[latest release](https://github.com/sybxxx/codex-auto-retry/releases/latest)。

## 三、Codex 工作如何被偵測

### 3.1 主 scanner

- 型態：**polling**。
- 預設頻率：`poll_interval_seconds = 2`。
- 可設定範圍：1～60 秒。
- 實作：`daemon.run()` 建立 `time.NewTicker(PollIntervalSeconds * time.Second)`；每個 tick 執行 `scanSessions()`。
- 掃描範圍：預設 Codex home 的 `sessions` 和 `archived_sessions`，也支援 Cockpit instance homes 與自訂 roots。
- 掃描方式：每次都用 `filepath.WalkDir` 走訪目錄樹；已知檔案若 size 未變就不讀內容，若增加才從持久化 byte offset 往後讀。
- 啟動第一次只建立 baseline 到目前檔尾，不把既有完整歷史全部當成新故障；watchdog 停止期間新出現的未處理 failure 仍可在重啟後處理。

直接證據：[config.go](https://github.com/sybxxx/codex-auto-retry/blob/main/scripts/source/config.go)、[daemon.go](https://github.com/sybxxx/codex-auto-retry/blob/main/scripts/source/daemon.go)、[scanner.go](https://github.com/sybxxx/codex-auto-retry/blob/main/scripts/source/scanner.go)。

### 3.2 解析的事件

它只保留 lifecycle metadata，不保留對話內容：

- `task_started`
- `task_complete`
- `turn_aborted`
- `user_message`（轉成 content-free 的 `task_user_input`）
- `thread_goal_updated`
- assistant message／tool result 的「有進展」布林標記
- 固定格式的 subagent recovery notice

thread key 來自 rollout 檔名的第一個 UUID。新格式 `<thread-id>_<turn-id>.jsonl` 的第二個 UUID只視為 turn，不會誤當 task ID。

### 3.3 所有 timer / ticker

| 位置 | 頻率 | 用途 |
|---|---:|---|
| daemon 主 ticker | 預設 2 秒 | 掃描 rollout、讀 control/config、排程、寫 state/status |
| tray Win32 timer | 1 秒 | 讀 management snapshot、更新圖示與倒數 tooltip |
| watchdog memory guard | 5 秒 | 讀本身 private bytes |
| shared app-server memory probe | 最多每 5 秒 | shared mode 的 monitor-only memory limit |
| controller readiness probe | 最多每 10 秒 | shared backend／Codex IPC 健康檢查 |
| awaiting lifecycle probe | 每 5 秒，需兩次 inactive | 修復「已收到 task_started、但 task_complete 沒寫入」的窄窗口 |
| WinForms 設定視窗 | 開啟時每 1 秒 | 重讀 status/config/control/state |
| MCP 面板 | 開啟時每 5 秒向 MCP 取資料；每 1 秒本地倒數 | 面板刷新 |
| supervisor crash backoff | 1 秒翻倍到 60 秒 | worker 異常退出才啟用；等待期間每 250ms 看 stop marker |

它沒有 tight busy loop；等待都使用 ticker/timer/message loop。

## 四、完成與錯誤判斷

### 4.1 正常完成

`task_complete` 同時滿足：

- `error` 空白；且
- `last_agent_message` 未知，或已知且非空。

若 `last_agent_message` 明確為 `null` 或空字串，會被歸為 `empty_response` 並重試。若未來 schema 根本沒有該欄位，程式會把它視為成功，而不是誤判空回覆。

### 4.2 故障分類表

| 類型 | 判斷 | 現行行為 |
|---|---|---|
| timeout / network | 408、425；`timeout`、`timed out`、`deadline exceeded`、connection/DNS/TLS/WebSocket/stream 中斷等文字 | `transient`，一般退避重試 |
| server error | HTTP 500～599 | `server`，一般退避重試 |
| rate limit | HTTP 429；`rate limit`、`too many requests`、`quota temporarily`、`capacity`、`overloaded`、`try again later` | `rate_limit`，**仍只是一般退避重試** |
| auth 暫時不可用 | auth/login/oauth service unavailable、cooldown 等 | `auth_transient`，一般上限內重試 |
| 401/403 / credential | unauthorized、login required、token expired、API key 等 | `auth_limited`，預設最多 6 次 |
| empty response | completion 有明確 final 欄位但為空 | `empty_response`，一般退避重試 |
| 特殊 CC Switch wrapper | 精確符合 `cc_switch_upstream_error`、`upstream_status=400`、cause/message 是 upstream request failed | 當作 transient |
| unknown provider error | 沒命中上述規則 | 預設最多 3 次 |
| 永久錯誤 | context/token limit、invalid request/payload/schema、model missing、policy、permission、approval、普通 400/404/405/409/410/413/415/422 | 不重試 |
| 使用者取消 | `turn_aborted` 或 cancellation 文字 | 清除 pending/awaiting/retry counters，不重試 |

分類程式：[classifier.go](https://github.com/sybxxx/codex-auto-retry/blob/main/scripts/source/classifier.go)、事件完成判斷：[events.go](https://github.com/sybxxx/codex-auto-retry/blob/main/scripts/source/events.go)。

### 4.3 目前 backoff 與停止條件

- 預設首次等待 5 秒。
- 預設 exponential：5、10、20、40、80、160、300、300…秒。
- 也支援 fixed 與 linear。
- 每次故障 recovery budget 預設 15 次。
- 連續無可見進展上限預設 5 次；出現 assistant reply 或 completed tool result 會重設這一個計數，但不會重設本次故障總 attempts。
- 整條 automatic recovery 有 **30 分鐘 hard circuit breaker**。

因此原始碼現況無法等待 5 小時或一週 quota reset；即使把最大 delay 調高，也會先撞到 30 分鐘總上限。

## 五、如何 Resume 原本的 Codex task

### 5.1 ID 與設定來源

- queue key 是 persistent thread UUID，不是「最新 task」或畫面目前選中的 task。
- 最新 `turn_context` 與 `thread_settings_applied` 會被反向讀取。
- 只 allowlist：working directory、workspace roots、model/provider、service tier、reasoning effort/summary、personality、approval policy/reviewer、permissions。

### 5.2 Windows 官方 IPC 路徑

- named pipe：`\\.\pipe\codex-ipc`。
- framing：4-byte little-endian 長度 + JSON payload。
- 流程：
  1. `initialize`
  2. `thread-owner-discovery`，參數包含 `hostId=local` 與 `conversationId=<thread UUID>`
  3. 對找到的 exact owner client 送 `thread-follower-start-turn`
  4. `conversationId` 保持同一個 thread；`inheritThreadSettings=true`；input 與 responseItems 都是空陣列
- 結果：建立的是**同一個 thread 裡的新 turn**，不是新 thread/session。

程式：[codex_ipc_windows.go](https://github.com/sybxxx/codex-auto-retry/blob/main/scripts/source/codex_ipc_windows.go)。

### 5.3 shared app-server WebSocket 路徑

1. `thread/loaded/list`
2. 未載入才 `thread/resume`
3. `thread/read` 再確認不是 active
4. 讀 goal state；遇到 completed/paused/usageLimited/budgetLimited 等會 fail closed
5. 普通對話用 `turn/start` + empty input；只有 app-server 明確拒絕 empty input 才送設定的 fallback 文字（預設「繼續」）

程式：[shared_controller.go](https://github.com/sybxxx/codex-auto-retry/blob/main/scripts/source/shared_controller.go)、[app_server_rpc.go](https://github.com/sybxxx/codex-auto-retry/blob/main/scripts/source/app_server_rpc.go)。官方 app-server 同樣明確區分 `thread/resume` 與 `turn/start`，可參考 [OpenAI Codex app-server 官方文件](https://learn.chatgpt.com/docs/app-server)。

### 5.4 如何避免重複 Resume

- `eventKey` 由 thread、事件種類、turn、timestamp 與錯誤摘要 hash 組成，寫入 `ProcessedEvents`。
- 每個 thread 同時只能有一份 `PendingRetry` / `AwaitingRetry`，且 `active` map 以 thread ID 去重。
- dispatch 結果回來時再次核對 event key 與 attempt，舊 job/result 直接忽略。
- 第一個落在 dispatch window 的 `task_started` 被綁成 `RetryTurnID`；只有同一 turn 的 `task_complete` 能結束或延續這條 retry chain。
- 新的人工 turn/user input/abort 會取消自動 retry。
- shared WebSocket 路徑在 resume 前後讀 live status；已 active 則不再 start。
- 官方 IPC 路徑在已 acknowledge 後，把 lifecycle probe 保守視為 active，避免因無法直接查官方 stdio child 而補送第二個 turn。
- subagent 使用 deterministic recovery event ID 與 persisted `ParentNotified`，避免 parent 被重複通知或另建 child。

研究基線的一個重要細節是：upstream 的 `dispatchDueLocked()` 把 `shared_app_server_enabled=false` 當成整體 dispatch gate。v1.1.0（Build 4）已將選用的 CLI fallback 從此 gate 拆開；使用者明確開啟 fallback 後，允許的普通可恢復故障即使 shared backend 關閉也能進入同 session CLI 路徑。Desktop IPC／shared app-server 的既有邊界仍維持保守，避免在 owner 或連線狀態不明時重複 dispatch。

## 六、多 task 的 queue / scheduler

- 每個 thread 有完全獨立的 retry state。
- due jobs 依 `DueAt` 排序。
- `max_parallel_retries` 預設 **4**，可設定 1～16。
- 同一 tick 最多取可用 slots 數量，之後每個 job 各開 goroutine，**並行 dispatch**。
- 如果該 thread 已 active/awaiting，不會再送一份；如果 controller 回報 task active，該 task 留在自己 queue 裡稍後重查，不會取消其他 task。
- 目前**沒有全域 30 秒 stagger**。多個 task 同時到期時，最多四個會幾乎一起送出。

這代表第 5 階段的「Task A → 等 30 秒 → Task B」必須新增全域 dispatch pacing，不能只調 `max_parallel_retries`。

## 七、System Tray、背景程序與 UI 通訊

### 7.1 Tray library

Tray 沒有使用 systray 第三方 library。它直接用 `golang.org/x/sys/windows` 載入 `user32.dll` / `shell32.dll`，呼叫：

- `RegisterClassExW`
- hidden message window
- `SetTimer`
- `Shell_NotifyIconW`
- popup menu / message loop

它也監聽 Windows `TaskbarCreated`，Explorer 重啟後會重新註冊 tray icon。

### 7.2 現有 tray 能力

- tooltip 狀態：running、paused、waiting、active、stopped。
- double-click：開啟設定。
- 右鍵：開啟設定、暫停／恢復自動重試、退出。

尚未具備需求中的獨立「迷你監控」、「停止自動 Resume」命令，以及明確的 `Rate Limited` tray 狀態。

### 7.3 UI 與 Go backend 如何通訊

- watchdog 每 2 秒原子寫 `state.json` 與 `status.json`。
- `control.json` 儲存 pause 狀態。
- `commands` 目錄的一次性 JSON command files 表示 retry-now / cancel / restart。
- PowerShell WinForms 設定視窗讀上述檔案；變更設定或下命令時呼叫同一個 watchdog exe 的 local command 子命令，不直接寫 `state.json`。
- MCP panel 透過 Go MCP SDK 的 stdio transport 呼叫 `get_auto_retry_status` 等工具；Go management service 再讀同一組原子檔案。

這是低耦合的 file-based control plane，適合繼續沿用。

## 八、CPU、RAM 與長期常駐評估

### 8.1 有利因素

- 所有等待都使用 timer/ticker/message loop，沒有 busy loop。
- rollout 只讀 append 部分，不重複 decode 全檔。
- state 有硬上限：20,000 processed events、2,000 cursors、500 inactive threads、8 MB state file。
- daemon log 5 MB rotate，保留 active + 3 backups。
- watchdog 有預設 1 GB private-memory hard guard；shared app-server 有預設 4 GB monitor-only guard。
- MCP server 與 WinForms 設定視窗都是按需啟動，不必常駐。

### 8.2 idle 時仍會做的工作

- 每 2 秒 `WalkDir` 掃全部 sessions + archived_sessions。
- 每 2 秒重讀 config/control，並原子重寫 state/status，即使沒有新事件。
- tray 每 1 秒再讀 management snapshot 所需的數個 JSON 檔。
- memory guard 每 5 秒查 private bytes。
- shared mode 開啟時還會做 app-server memory 與 10 秒 readiness probe。
- supervisor 本身常駐等待 worker process。

### 8.3 結論

它在一般數量的 rollout 檔案下應屬**低 CPU、可長期常駐**，而且沒有 busy loop；但不能僅憑架構宣稱 idle CPU 一定「接近 0」。最大的持續成本不是 JSON 增量解析，而是 2 秒一次的全目錄 WalkDir、固定寫兩個 JSON，以及 tray 1 秒一次讀檔。當 archived sessions 很多、磁碟慢、工作目錄位於網路磁碟或防毒軟體攔截 atomic rename 時，成本會放大。

建議 Fork 後：

- scanner 採 Windows directory change notification + 低頻 safety reconciliation，或至少把 active 與 archive 掃描頻率拆開。
- 只有 state/status 實際變動或 heartbeat 到期才寫檔。
- tray 優先讀單一小型 status cache，不要每秒解析完整 state。
- `supervisor.log` 目前沒有 rotate；雖然只記 process lifecycle、正常情況非常少寫，仍應補上 bounded rotation。

## 九、所有 rate-limit 相關程式碼

真正會影響 runtime 的位置只有：

- `model.go`：定義 `classRateLimit = "rate_limit"`。
- `classifier.go`：HTTP 429 與 rate-limit/capacity 字串分類。
- `classifier_test.go`：429 測試。
- `runner.go`：沒有 rate-limit 特例；所有 failure class 都使用同一個 `retryDelay()`。
- `retry_state.go`：沒有 quota wait state；仍受 attempts、consecutive 與 30 分鐘 circuit breaker 限制。
- `panel.ts` / `settings.ps1`：只把 `rate_limit` 顯示成「請求限流／Rate Limited」。
- README 與 docs：宣稱 rate limit 可恢復，但沒有 quota window API。

全 repo 搜尋沒有找到：

- `account/rateLimits/read`
- `account/rateLimits/updated`
- `usedPercent`
- `resetsAt`
- `windowDurationMins`
- Weekly / 5H quota monitor

## 十、5H / Weekly 與 app-server rate limits

### 10.1 原專案目前能不能取得

**不能。** 現有程式沒有呼叫 rate-limit API，也沒有資料 model 或 UI 欄位。

### 10.2 官方 app-server 能不能取得

**能取得 quota windows，但必須以回傳資料為準，不能硬假設一定存在。** 官方文件定義：

- request：`account/rateLimits/read`
- notification：`account/rateLimits/updated`
- `primary` / `secondary`
- `usedPercent`
- `windowDurationMins`
- `resetsAt`（Unix seconds）
- `rateLimitReachedType`
- 新版另有 `rateLimitsByLimitId`

官方 schema 與範例：[OpenAI Codex app-server：Rate limits](https://learn.chatgpt.com/docs/app-server#rate-limits-chatgpt)。

研究當天對本機 Codex 帳號做唯讀驗證，實際回傳：

- primary：`windowDurationMins = 300`（5H）
- secondary：`windowDurationMins = 10080`（7 天）
- 兩者都有 `usedPercent` 與 `resetsAt`

因此需求中的 5H／Weekly 已有正式資料來源。倒數不需要另一個 API，只要以 monotonic/local timer 對 `resetsAt` 計算顯示；到期時必須重新 `account/rateLimits/read`，不能把本地倒數歸零就宣稱 quota 已恢復。

### 10.3 建議整合方式

- 新增持久化但有 freshness 的 `RateLimitSnapshot`，包含 `fetchedAt`、window duration、used%、resetsAt 與 error。
- UI 只有在資料存在且未過 freshness policy 時顯示；否則顯示 `N/A`，不可保留舊值冒充最新。
- 以使用者設定的 1/3/5/30/custom 分鐘 timer 呼叫 `account/rateLimits/read`，預設 3 分鐘。
- 同一條持久 app-server connection 同時接收 `account/rateLimits/updated`；notification 可提前刷新，但不能取代定期 read 與 reconnect。
- 現有 `appServerRPCClient.Call()` 只等單次 response 並忽略 notification，呼叫後連線即關閉；要接 `updated`，需新增 persistent client/read pump，而不是在現有單次 Call 迴圈硬塞 UI 邏輯。

### 10.4 Quota-aware Resume 所需狀態變更

不能只在 classifier 多加一個 if。至少要新增：

- `QuotaWaiting` 狀態：記錄被哪個 bucket 擋住、observed `resetsAt`、下次 verification 時間、使用者是否停止。
- rate-limit failure 後先讀 quota；只有 `usedPercent >= 100` 或 server 的 `rateLimitReachedType` 明確指出 quota，才進 quota wait。一般 TPM/capacity/短暫 429 仍走原作者快速 retry。
- quota wait 不消耗 provider attempt，也不受 30 分鐘 recovery circuit breaker 影響。
- 到 `resetsAt` 後先 read；只有 quota 確認恢復才 Resume。
- Resume 後若下一次再用完，建立下一個新的 quota-wait cycle；因此可無限循環，直到使用者對該 task 或全域下 stop。
- 若 quota API 無資料，不可顯示假倒數，也不可宣稱已恢復。應顯示 `N/A`／等待驗證，採保守且有上限的 verification cadence。

## 十一、Task 清單可行性

原專案目前只顯示 retry queue 中的 short thread ID；沒有名稱、project path、最後活動時間或選取狀態。

Codex app-server 的 `thread/list` 已支援 pagination、`updated_at` 排序與 status，回傳可包含 `name`、`updatedAt`、`status` 等；[官方文件](https://learn.chatgpt.com/docs/app-server#list-threads-with-pagination--filters) 也列出可依 `cwd` 過濾。project path 可優先取 thread/session metadata；若回傳未含 cwd，再從該 thread 最新 allowlisted `turn_context` 取 `cwd`，不需讀對話內容。

因此 Codex task 的多選清單可實作；但需要新增 task catalog/cache，而不是直接重用目前只含 failure queue 的 `managedRetries()`。

## 十二、ChatGPT Work 支援研究

> **2026-10-07 再驗證：**OpenAI Docs 仍未提供可讓獨立 Windows EXE 列出、判斷故障並原地續跑 Work Task 的公開介面；本機受限工作階段也未能驗證 UI Automation 的對話身分。保留 fail-closed 的 `WorkAdapter`，詳細證據與實驗閘門見 [Work 續跑可行性驗證](work-resume-feasibility.md)。

### 12.1 現有架構能不能直接取得／Resume Work task

**目前找不到。** 技術原因：

1. scanner 只知道本機 Codex home 的 rollout JSONL。
2. Windows named pipe payload 是 Codex Desktop 的 thread owner / conversation ID route，原專案沒有 Work protocol。
3. 官方 Codex app-server `thread/list` 的 `sourceKinds` 是 `cli`、`vscode`、`exec`、`appServer`、subAgent 類型與 `unknown`，沒有公開的 Work task kind。
4. 官方 Work 文件說明 Work 可在 desktop local 或 cloud 執行；cloud task 在遠端執行，不會自然出現在本機 Codex rollout store。參考 [ChatGPT Work 入門](https://learn.chatgpt.com/docs/get-started-with-work) 與 [Work overview](https://learn.chatgpt.com/docs/enterprise/chatgpt-work-overview)。
5. 在目前查到的公開官方文件中，沒有提供第三方 Windows 常駐程式用來列出、取得 failure lifecycle、或 Resume 任意 ChatGPT Work task 的公開 API。

「Work 與 Codex 共用一些底層執行／權限機制」不等於 Work task 已暴露在 Codex app-server；不能據此假設 thread ID 可互通。

### 12.2 可接受的替代方案

- 先定義 `WorkAdapter` interface，但回傳 `Unsupported / 目前找不到正式介面`，UI 不顯示假的 Work task。
- 未來若 OpenAI 公開 Work task API，再在 adapter 內實作，不修改 Codex recovery core。
- 若使用者日後明確授權，可另做 `WorkUIAutomationAdapter`：只控制 ChatGPT desktop UI，必須有視窗／元素 identity、焦點、防誤送、版本相容與人工確認；它是高風險 fallback，不可成為 CodexAdapter 的 fallback。
- 對 cloud Work，可考慮使用產品本身的 scheduled/heartbeat 能力，而不是把它偽裝成 Codex IPC Resume；但這仍不是任意既有 Work task 的通用 recovery API。

## 十三、Fork 與重寫比較

| 面向 | A. Fork `codex-auto-retry` | B. 重寫 CodexWorkResumer |
|---|---|---|
| 可沿用功能 | 約 70～80% 核心能力可直接沿用：scanner、分類、same-thread resume、IPC/shared transport、goal/subagent、安全狀態機、queue、tray、MCP、installer、tests | 幾乎都要重做；最多參考協議與測試案例 |
| 需要新增 | usage client、quota wait、task catalog/selection、global stagger、mini monitor、adapter abstraction、Work unsupported stub | 原功能與新增功能全部重建 |
| 修改風險 | 中等；主要風險是 quota state 與 scheduler 會碰 daemon/state/management | 很高；最難驗證的是 duplicate side effect、防錯 thread、pause/goal/subagent 競態與 Windows IPC |
| upstream 同步 | 可保留 `upstream` remote；若新增功能放新檔案、避免大改 transport，仍可 merge/rebase | 沒有直接同步；每次 upstream IPC/schema 修正都要人工搬運 |
| Windows 常駐穩定性 | 已有 supervisor、tray Explorer recovery、atomic writes、bounded state、fail-open、memory guard | 全部要重新累積實戰驗證 |
| CPU / RAM | 現況低但可優化 polling/write；可不更換語言/runtime | 可從零做更精簡 event-driven，但短期較難達到同等穩定性 |
| Work 支援難度 | 與 Fork/重寫無關；瓶頸是缺正式 Work API。adapter 可隔離 | 同樣缺 API，重寫不會讓 Work 介面憑空出現 |
| 開發成本 | 中；新增範圍明確，有大量既有測試可保護 | 高；估計是 Fork 的 2～4 倍，且驗證面更大 |
| 維護成本 | 中；需管理 upstream 衝突，但可吸收原作者修正 | 高；所有 Windows/Codex protocol 變更自行追蹤 |

### 建議

選 **Fork**。

技術理由不是單純「比較快」，而是原專案已實作需求裡風險最高、最難靠單元測試完全重建的部分：exact owner routing、同一 thread 空輸入 continuation、設定復原、turn correlation、防 duplicate、goal/subagent fail-closed、shared backend ownership 與 Windows fail-open。新增需求雖然大，但大多可疊在既有 state machine 與 control plane 上。

建議 Git 策略：

- 正式 repo：`origin` 指向自己的 CodexWorkResumer repository。
- `upstream` 指向 `https://github.com/sybxxx/codex-auto-retry.git`。
- 以 upstream `main` 為歷史基礎，不做 source copy 後重新 `git init`。
- 品牌/UI/docs 與新 adapter 分支化；transport 核心盡量維持可 merge。
- 每次同步先跑 upstream tests，再跑新增 quota/scheduler/UI tests。

## 十四、預計修改與新增的檔案

以下是確認 Fork 後的預估，現在尚未動手：

### 14.1 既有檔案

- `scripts/source/config.go`、`config_test.go`：usage refresh、custom minutes、task stagger、selection、mini monitor 設定。
- `scripts/source/model.go`：quota snapshot、quota-wait、task descriptor、adapter 狀態。
- `scripts/source/state.go`、`state_migration.go` 與 tests：新 state schema 與 migration。
- `scripts/source/classifier.go`、tests：保留 broad rate-limit classification，增加「需向 quota API 確認」的語意，不直接把所有 429 當 quota exhaustion。
- `scripts/source/daemon.go`、`daemon_test.go`：usage monitor lifecycle、quota verification、global dispatch spacing。
- `scripts/source/retry_state.go` 與 recovery tests：quota wait 不受 30 分鐘上限、可反覆進入下一個 quota cycle。
- `scripts/source/app_server_rpc.go`、tests：persistent read pump／notification routing，或新增 client 而只保留共用 wire types。
- `scripts/source/shared_controller.go`、tests：Codex adapter 接點、rate-limit read 與 task list 能力；避免把 Work 混入。
- `scripts/source/management.go`、`management_test.go`：usage、task catalog、selection、mini monitor snapshot/control。
- `scripts/source/mcp_server.go`：新增 management tool 欄位與操作。
- `scripts/source/tray_windows.go`、tests：Waiting/Running/Rate Limited/Error、迷你監控、停止自動 Resume。
- `scripts/source/ui/settings.ps1`：主畫面 task 多選、usage 設定、stagger、Work unavailable 說明。
- `scripts/source/ui/src/panel.ts`、`panel.css`、`panel.html`：內嵌管理面板同步功能。
- `scripts/source/ui/dist/panel.html`：由既有 frontend build 產生，不手工維護 minified bundle。
- `scripts/source/logger.go`、`supervisor.go` 與 tests：視需要把 supervisor log 也納入 rotate。
- `README.md`、`README_zh.md`、`docs/architecture.md`、`docs/project-map.md`：品牌與新架構文件。
- installer/build metadata：只在正式 Fork 與版本建立後調整；第 3 階段不動。

### 14.2 建議新增檔案

- `adapter.go`：`CodexAdapter` / `WorkAdapter` capability contract。
- `codex_adapter.go`：包住現有 controller，不重寫成熟 IPC 邏輯。
- `work_adapter.go`：先回報 unsupported；未來正式 API adapter 的位置。
- `rate_limits.go`、`rate_limits_test.go`：官方 schema、bucket normalization、freshness。
- `usage_monitor.go`、`usage_monitor_test.go`：timer + updated notification + reconnect。
- `quota_scheduler.go`、`quota_scheduler_test.go`：wait-until-reset、re-read、恢復確認、循環。
- `task_catalog.go`、`task_catalog_test.go`：`thread/list` pagination、名稱/path/status/last activity。
- `dispatch_pacer.go`、`dispatch_pacer_test.go`：多 task 30 秒全域間隔。
- `mini_monitor_windows.go`、tests：獨立小窗與 Always on Top；優先原生 Win32，避免讓長期顯示依賴常駐 PowerShell。

## 十五、尚未驗證／不可宣稱的事項

- 沒有執行 upstream binary，因此本文件的 CPU/RAM 是架構評估，不是實測 benchmark。
- 沒有安裝 Go／Node，也沒有執行 upstream test suite。
- 沒有建立另一個正式專案根目錄；研究時預定路徑不存在，依指示不先建立。
- 目前找不到 Work task 的正式直接控制 API；不得把「可能共用底層」寫成已支援。
- app-server schema 能回傳 5H/Weekly，但實際某個帳號、方案或暫時狀態仍可能缺欄位；實作必須顯示 `N/A`。

## 十六、停止點

第 3 階段研究已完成。下一步只有在使用者確認後才會：

1. 以 upstream Git 歷史為基礎，在指定的本機路徑建立 Fork 工作目錄；
2. 保留 `upstream` remote；
3. 先做小步、可測試的 adapter／rate-limit state 設計；
4. 不處理 Hermes、不更動其他專案、不修改系統 PATH。
