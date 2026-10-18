# ChatGPT Work 原 Task 續跑可行性驗證

驗證日期：2026-10-07。範圍是「獨立 Windows 常駐程式自動辨識故障，並續跑原本的 ChatGPT Work 對話」，不是在 Work 介面中由使用者手動傳送後續訊息。

## 結論

目前找不到已公開、可由本專案 EXE 呼叫的 Work Task 清單、故障狀態事件與原 Task Resume API。這是介面缺口，不是把 Codex 的 thread ID 改標為 Work 就能解決。現有 `WorkAdapter` 應維持 `Available=false`、`Tasks=false`、`Resume=false`；主畫面不得列出推測的 Work Task 或提供假的 Resume 按鈕。

## 已驗證的界線

1. [OpenAI Docs：ChatGPT Work](https://learn.chatgpt.com/docs/get-started-with-work) 說明使用者能在 App 內延續符合條件的既有 Work 對話，也區分本機與雲端執行；它沒有提供第三方常駐程式辨識額度故障與 Resume 原對話的 API。
2. [OpenAI Docs：Codex app-server](https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server) 的 `thread/resume` 使用的是呼叫端先前儲存的 Codex app-server thread ID；文件沒有宣稱它能接受 ChatGPT Work 對話 ID。不得以此路徑試送 Work ID。
3. 此 Codex App 工作階段內的管理工具能列出 ChatGPT 對話並傳送可見的後續訊息，但這是 App 內部工具能力，不是提供給本專案獨立 EXE 的公開 IPC/API；清單也未提供足夠的 Work/Codex 分類、故障原因或額度重設事件。不能把「可以傳訊息」當成「可以安全自動救援」。
4. 一般受限執行階段讀不到 ChatGPT 視窗；唯讀桌面檢查放寬後，可辨識 `ChatGPT` 頂層視窗及 `RootWebArea`，但 ControlView／RawView 都沒有提供對話清單、穩定 Work 對話 ID 或目前故障狀態。這表示**目前仍無法驗證**精確目標與互動，不表示所有桌面版本都不可能使用 UI Automation。[OpenAI Docs：Computer Use](https://learn.chatgpt.com/docs/computer-use) 也說明 Windows GUI 操作需前景桌面；它不等於可長期背景運作的正式 Work API。

## 若要做獨立 UI Automation 實驗版，必須先通過的閘門

- 以穩定的對話 ID 精確識別目標，而不是只比對可能重複的標題或畫面位置。
- 可讀取「目前確實停止／失敗、原因可恢復、沒有正在執行或等待使用者批准的 turn」；額度不足時還要能可靠得知該 Work 帳號的重設狀態。
- 只在使用者明確啟用實驗模式時控制前景視窗；每次送出前再驗證視窗、帳號、對話 ID 與狀態，避免打斷其他工作或續跑錯誤對話。
- 能實測同一 Work 對話續跑成功、重複觸發被攔住、重啟後狀態正確，並在 App 更新或 UI 讀取失敗時自動停用。

在上述條件未通過前，不會把 UI 點擊或新建 Codex Session 包裝成 Work 原地續跑。Work 路徑與 Codex IPC／排程器維持隔離；Codex 現有功能不受此研究影響。
