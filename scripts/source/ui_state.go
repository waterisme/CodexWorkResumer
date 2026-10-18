package main

import (
	"fmt"
)

type UIStateDefinition struct {
	Code           string `json:"code"`
	LabelZH        string `json:"label_zh"`
	LabelEN        string `json:"label_en"`
	Color          string `json:"color"`
	MeaningZH      string `json:"meaning_zh"`
	MeaningEN      string `json:"meaning_en"`
	BehaviorZH     string `json:"behavior_zh"`
	BehaviorEN     string `json:"behavior_en"`
	NotificationZH string `json:"notification_zh"`
	NotificationEN string `json:"notification_en"`
}

var uiStateCatalog = []UIStateDefinition{
	{Code: "waiting", LabelZH: "等待中", LabelEN: "Waiting", Color: "#12826B", MeaningZH: "系統匣顯示綠色；監控正常，正在等待可處理的 Task。", MeaningEN: "The tray icon is green; monitoring is healthy and waiting for work.", BehaviorZH: "2 秒 scheduler 只維持輕量 heartbeat；Session 掃描依主畫面設定執行，預設每 60 秒。", BehaviorEN: "The 2-second scheduler only keeps a lightweight heartbeat; session scans follow the main-window setting and default to 60 seconds.", NotificationZH: "不通知", NotificationEN: "None"},
	{Code: "running", LabelZH: "執行中", LabelEN: "Running", Color: "#1570EF", MeaningZH: "系統匣顯示藍色；正在 Resume 一個或多個 Task。", MeaningEN: "The tray icon is blue; one or more tasks are being resumed.", BehaviorZH: "依多 Task 間隔啟動；優先使用正式恢復通道，只有使用者啟用且主路徑不可用時才使用背景 CLI 備援。", BehaviorEN: "Start tasks using the configured gap. Prefer the official recovery route and use the background CLI only when the user opted in and the primary route is unavailable.", NotificationZH: "不重複通知", NotificationEN: "No repeated alerts"},
	{Code: "rate_limited", LabelZH: "額度受限", LabelEN: "Rate Limited", Color: "#E58A00", MeaningZH: "系統匣顯示橘色；Codex 回報額度限制，顯示的剩餘百分比可能尚未反映該限制。", MeaningEN: "The tray icon is amber because Codex reported a quota limit; the displayed remaining percentage may not reflect that limit yet.", BehaviorZH: "停止快速重試；reset 後重查額度。已設定名單只續跑勾選項，未設定時沿用舊版全部救援。", BehaviorEN: "Stop fast retries and recheck quota after reset. A saved watchlist limits resumption to checked tasks; with no list, legacy all-task rescue remains.", NotificationZH: "進入時通知一次；不僅憑百分比宣告恢復", NotificationEN: "Notify once on entry; do not claim recovery from percentages alone"},
	{Code: "error", LabelZH: "錯誤", LabelEN: "Error", Color: "#D13A36", MeaningZH: "系統匣顯示紅色；背景服務或恢復通道需要處理。", MeaningEN: "The tray icon is red; the background service or recovery channel needs attention.", BehaviorZH: "保留狀態並引導使用者開啟主畫面。", BehaviorEN: "Preserve state and direct the user to the main window.", NotificationZH: "每次事件一次", NotificationEN: "Once per incident"},
	{Code: "paused", LabelZH: "已暫停", LabelEN: "Paused", Color: "#697586", MeaningZH: "系統匣顯示灰色；Task 掃描與額度更新已暫停。", MeaningEN: "The tray icon is gray; task scanning and quota refresh are paused.", BehaviorZH: "只維持輕量 heartbeat，直到恢復監控。", BehaviorEN: "Keep only a lightweight heartbeat until monitoring resumes.", NotificationZH: "不通知", NotificationEN: "None"},
}

type UIQuotaDefinition struct {
	Code      string `json:"code"`
	Color     string `json:"color"`
	MeaningZH string `json:"meaning_zh"`
	MeaningEN string `json:"meaning_en"`
}

var uiQuotaCatalog = []UIQuotaDefinition{
	{Code: "normal", Color: "#1565C0", MeaningZH: "剩餘額度高於 20%。", MeaningEN: "More than 20% quota remains."},
	{Code: "low", Color: "#EF6C00", MeaningZH: "剩餘額度為 6% 至 20%，只改變顏色、不重複通知。", MeaningEN: "6% to 20% remains; color changes without repeated alerts."},
	{Code: "critical", Color: "#C62828", MeaningZH: "剩餘額度為 0% 至 5%，等待正式 rate limit／reset 資料。", MeaningEN: "0% to 5% remains; wait for authoritative rate-limit/reset data."},
}

type UIState struct {
	Code   string `json:"code"`
	Label  string `json:"label"`
	Color  string `json:"color"`
	Detail string `json:"detail"`
}

func stateDefinition(code string) UIStateDefinition {
	for _, definition := range uiStateCatalog {
		if definition.Code == code {
			return definition
		}
	}
	return uiStateCatalog[0]
}

func deriveUIState(snapshot ManagementSnapshot) UIState {
	code, detail := "waiting", "等待工作"
	switch {
	case snapshot.MonitoringPaused:
		code, detail = "paused", "監控已暫停"
	case !snapshot.Running:
		code, detail = "error", "背景服務未執行"
	case snapshot.ControllerState == "codex_not_running" && !snapshot.CLIResumeFallbackEnabled:
		code, detail = "error", "Codex 已退出"
	case snapshot.ControllerState == "shared_app_server_port_reserved":
		code, detail = "error", "本機連接埠被 Windows 保留"
	case snapshot.ControllerState == "shared_app_server_config_invalid":
		code, detail = "error", "Codex自動續跑助手設定不相容"
	case snapshot.StoppedRetries > 0:
		code, detail = "error", fmt.Sprintf("%d 個 Task 已停止", snapshot.StoppedRetries)
	case snapshot.RateLimitedRetries > 0:
		code, detail = "rate_limited", fmt.Sprintf("%d 個 Task 等待額度 reset", snapshot.RateLimitedRetries)
	case snapshot.ActiveRetries > 0:
		code, detail = "running", fmt.Sprintf("正在 Resume %d 個 Task", snapshot.ActiveRetries)
	case snapshot.Paused:
		detail = "自動 Resume 已停止"
	case snapshot.PendingRetries > 0:
		detail = fmt.Sprintf("%d 個 Task 等待 Resume", snapshot.PendingRetries)
	}
	definition := stateDefinition(code)
	return UIState{Code: code, Label: definition.LabelEN, Color: definition.Color, Detail: detail}
}
