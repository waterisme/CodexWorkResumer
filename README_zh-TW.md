# CodexWorkResumer

CodexWorkResumer 是以 [sybxxx/codex-auto-retry](https://github.com/sybxxx/codex-auto-retry) 為基礎的 Windows 額度監控與同一工作續跑工具。專案保留 upstream Git 關係，方便日後檢視與同步原作者更新。

目前本機建置：**v1.3.1（Build 15）**。`VERSION.json` 是版本的唯一基準，每個可交付 Build 都必須在 `CHANGELOG.md` 留下對應紀錄。這是原始碼版本；安裝中的程式須另行升級及驗收，不能因原始碼版本更新就視為已生效。

[English](README.md) | [繁體中文]

## 主要功能

- 監看 Codex rollout 事件，辨識 timeout、網路錯誤、server error、rate limit 與空回覆等可恢復故障。
- 優先透過 Codex Desktop 正式 IPC，在原本的 task／session 原地續跑。
- 多個 task 各自保留狀態，具備排程、退避、防重複 Resume 與使用者中止保護。
- 執行中的 Task 可先勾選額度續跑監控，勾選不會立即 Resume；拖曳清單可安排額度恢復後順序。尚未建立名單時，沿用舊版對全部額度故障自動救援；第一次儲存名單後只監控勾選項。暫時故障救援不受勾選影響。
- 額度續跑有「間隔啟動」與「循序完成」兩種模式，仍共用同一排程器。預設間隔啟動，跨 Task 啟動相隔 30 秒，可觀測執行上限預設 2；上限是盡力而為，不能涵蓋未被本程式看見的外部工作。循序模式會在可確認的正向完成、後續掃描仍未見新回合或待續工作時自動交棒；額度暫停、仍在執行、目標未完成或狀態不明時不推定完成。
- 顯示 5H 與 Weekly 剩餘額度、reset 時間及倒數；一小時內改以剩餘分鐘顯示。
- 提供 Windows 系統匣、四區主畫面，以及英文／繁體中文介面。四區依序為監控與操作、Session 清單與順序、排程模式與參數、通知與執行紀錄。獨立 Mini Monitor 將由另一項開發工作製作，不包含在本程式內。
- 清單區分「本次重試已停止但額度監控仍勾選」與「同一工作自動續跑的回合已成功結束」；後者不代表整項專案工作已完成。
- Session 清單更新時保留使用者的捲動位置，拖曳時標示預計插入位置，切換監控勾選時避免畫面閃動或短暫顯示英文狀態代碼。Build 14 的介面調整仍須升級後在實際 Windows 視窗驗收。
- idle 使用 timer／sleep，不使用 busy loop；log 有容量限制與輪替。
- 主畫面額度倒數維持每秒更新；大型恢復狀態檔依變動或最多 5 秒後重讀，減少閒置負擔。

## 額度資料

程式先透過 Codex app-server 的 `account/rateLimits/read` 讀取即時額度。若這次正式查詢失敗，只接受本機 rollout 中 **10 分鐘內**實際觀察到的 rate-limit snapshot；資料不存在或已過期時顯示 `N/A`，不會拿舊值冒充最新值。

顯示的是「剩餘百分比」，與 Windows 版 Codex 的方向一致：

- 剩餘百分比 = `100 - usedPercent`
- 5H 對應 300 分鐘 window
- Weekly 對應 10080 分鐘 window
- reset 到期後必須重新查詢，不能只靠本機倒數判定額度已恢復

## Resume 路徑

1. **Codex Desktop IPC（預設）**：優先使用正式 Windows named pipe，讓原本 Desktop task 原地續跑。
2. **共享 app-server（選用）**：只有使用者明確啟用時才使用；不需要為了一般安裝先關閉 Codex。
3. **背景 CLI fallback（選用，預設關閉）**：只有主要 Desktop 路徑明確不可用、且屬於允許的普通可恢復故障時，才以 `codex exec resume` 續跑同一 session。提示文字由 stdin 傳入，不放在命令列參數；30 分鐘未觀察到完成事件便停止等待，不會自動重送造成重複。

CLI fallback 的執行結果不會直接出現在原本 Desktop 視窗，因此只有確實需要時才建議開啟。quota／rate-limit 等待仍必須先確認額度恢復，不能用 CLI 繞過限制。

## 安裝與執行

一般使用者使用 Windows 發行包即可，不需要另外安裝 Go 或 Node.js，也不需要修改系統 PATH。安裝與安全啟動方式請看 [Windows 安裝說明](release/windows/README-安裝說明.txt)。

安裝程式只會處理本專案自己的檔案與設定。正常模式可在 Codex 開啟時安裝及查詢額度；只有切換需要 Codex 啟動參數的特殊共享模式時，才可能要求重新啟動 Codex。

若 Microsoft Store／MSIX Codex 拒絕由安全啟動器直接建立程序，啟動器會嘗試 Windows 官方套件入口。此時是**官方後台模式**，不代表選用的共享恢復通道已接入；啟動前也不會強制結束現有 Codex。

## 介面狀態

- **Waiting**：監控中，等待可處理事件。
- **Running**：正在進行 Resume 或驗證。
- **Rate Limited**：已確認受額度限制，等待 reset 後再次查詢。
- **Error**：需要使用者注意的錯誤。
- **N/A**：目前沒有足夠新鮮、可驗證的額度資料。

系統匣圖示單擊或雙擊會開啟主畫面，右鍵顯示控制選單。程式啟動時會依上次儲存的「Codex自動續跑助手」選擇真正套用啟用或停用，而不是只還原勾選外觀。原有 Mini Monitor 專用設定已從主程式移除，但不刪除舊偏好資料。

設定頁的「狀態、顏色與行為說明」會列出狀態定義；日後狀態或行為改變時，這份說明也必須同步更新。

主畫面橘色額度受限提示只反映**目前仍在等待額度的排程項目**，不會因已停止或歷史項目持續警告。若 Codex 曾回報額度拒絕，即使畫面仍有剩餘百分比，也必須遵守 reset 等待與重新查詢，不能靠百分比直接強制續跑。

## ChatGPT Work 支援

目前找不到可讓外部 Windows 工具直接列出或 Resume ChatGPT Work task 的正式公開介面，因此 **Work Resume 尚未實作，也不會顯示假的支援**。架構保留獨立 `WorkAdapter`；若未來只有 Windows UI Automation 可用，也必須留在獨立 adapter，不污染 Codex 的正式 IPC 路徑。

## 隱私與安全

- 只讀取恢復判斷所需的 allowlisted rollout metadata，不把對話內容寫入 log。
- 不記錄完整提示詞、response body、token、cookie 或命名管線 payload。
- 自動 Resume 會在新人工輸入、中止、不可恢復錯誤或安全條件不足時停止。
- 設定、狀態與 log 保留在使用者本機。

## 開發驗證

專案已有 Go 單元測試、PowerShell smoke tests、TypeScript typecheck、UI build、版本與 build provenance 驗證。正式交付時仍應在 Windows 上完成對應版本的完整驗證；無法執行的項目必須明確標成 skipped 或 unverified。

Build 11 更新管理面板的 npm 建置相依套件；npm 弱點掃描只代表掃描時的鎖定檔，不等於已完成真實 Codex 安裝、額度耗盡或安全啟動情境驗收。

授權條款請參閱 [LICENSE](LICENSE)，第三方相依套件授權全文請參閱 [THIRD_PARTY_LICENSES.txt](THIRD_PARTY_LICENSES.txt)；貢獻與安全通報請參閱 [CONTRIBUTING.md](CONTRIBUTING.md) 與 [SECURITY.md](SECURITY.md)。

公開發布前仍須完成 [準備與驗收清單](docs/PUBLICATION_READINESS.md)；目前沒有推送或發布 Build 15。
