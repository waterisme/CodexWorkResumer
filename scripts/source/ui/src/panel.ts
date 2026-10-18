import {
  App,
  applyDocumentTheme,
  applyHostFonts,
  applyHostStyleVariables,
  type McpUiHostContext,
} from "@modelcontextprotocol/ext-apps";
import {
  Activity,
  Clock,
  Play,
  RefreshCw,
  RotateCcw,
  Save,
  X,
  createIcons,
} from "lucide";
import "./panel.css";

type FailureClass =
  | "transient"
  | "rate_limit"
  | "server"
  | "auth_transient"
  | "auth_limited"
  | "empty_response"
  | "unknown"
  | "none";

type ManagedRetry = {
  thread_id: string;
  label: string;
  project_path?: string;
  task_type: "Codex" | "Work";
  last_activity_at?: string;
  state: "pending" | "starting" | "running" | "stopped";
  class: FailureClass;
  due_at?: string;
  seconds_remaining: number;
  recovery_attempt: number;
  max_recovery_attempts?: number;
  consecutive_retry: number;
  max_consecutive_retries?: number;
  action?: string;
  can_retry_now: boolean;
  can_cancel: boolean;
  can_restart: boolean;
  stop_reason?: string;
};

type UsageWindow = {
  used_percent: number;
  remaining_percent: number;
  window_duration_mins: number;
  resets_at: number;
};

type UsageSnapshot = {
  adapter: "codex" | "work";
  available: boolean;
  source?: string;
  fetched_at?: string;
  observed_at?: string;
  unavailable_reason?: string;
  five_hour?: UsageWindow;
  weekly?: UsageWindow;
};

type ManagementSnapshot = {
  version: string;
  build: number;
  running: boolean;
  heartbeat_stale: boolean;
  paused: boolean;
  monitoring_paused: boolean;
  shared_app_server_enabled: boolean;
  shared_app_server_requested?: boolean;
  cli_resume_fallback_enabled: boolean;
  desktop_transport?: "official_stdio" | "shared_websocket" | "stopped" | "unknown";
  recovery_mode?: "shared_websocket" | "safe_launcher_required" | "none";
  automatic_recovery_supported?: boolean;
  recovery_capability_reason?: string;
  startup_approved: "enabled" | "disabled" | "unknown";
  retry_prompt: string;
  max_recovery_attempts: number;
  max_consecutive_retries: number;
  initial_delay_seconds: number;
  max_delay_seconds: number;
  delay_increment_seconds: number;
  delay_strategy: "fixed" | "linear" | "exponential";
  resume_interval_seconds: number;
  usage_refresh_minutes: number;
  show_notifications: boolean;
  memory_limit_mb: number;
  memory_usage_mb?: number;
  memory_guard_triggered?: boolean;
  shared_app_server_memory_usage_mb?: number;
  shared_app_server_memory_limit_mb?: number;
  shared_app_server_memory_guard_triggered?: boolean;
  retry_safety_warning?: string;
  shared_app_server_port: number;
  now: string;
  last_scan_at?: string;
  pending_retries: number;
  active_retries: number;
  stopped_retries: number;
  rate_limited_retries: number;
  watched_roots: number;
  controller_state?: string;
  last_error?: string;
  notice?: string;
  codex_usage: UsageSnapshot;
  retries: ManagedRetry[];
};

type ToolResult = {
  structuredContent?: unknown;
  isError?: boolean;
  content?: Array<{ type: string; text?: string }>;
};

const iconSet = { Activity, Clock, Play, RefreshCw, RotateCcw, Save, X };
const elements = {
  shell: required<HTMLElement>("app-shell"),
  serviceLine: required<HTMLElement>("service-line"),
  serviceStatus: required<HTMLElement>("service-status"),
  version: required<HTMLElement>("version"),
  refreshButton: required<HTMLButtonElement>("refresh-button"),
  notice: required<HTMLElement>("notice"),
  queueCount: required<HTMLElement>("queue-count"),
  nextRetry: required<HTMLElement>("next-retry"),
  usageSummary: required<HTMLElement>("usage-summary"),
  usageFiveHourRow: required<HTMLElement>("usage-5h-row"),
  usageFiveHourBar: required<HTMLElement>("usage-5h-bar"),
  usageFiveHourRemaining: required<HTMLElement>("usage-5h-remaining"),
  usageFiveHourCountdown: required<HTMLElement>("usage-5h-countdown"),
  usageWeeklyRow: required<HTMLElement>("usage-weekly-row"),
  usageWeeklyBar: required<HTMLElement>("usage-weekly-bar"),
  usageWeeklyRemaining: required<HTMLElement>("usage-weekly-remaining"),
  usageWeeklyCountdown: required<HTMLElement>("usage-weekly-countdown"),
  queueSummary: required<HTMLElement>("queue-summary"),
  queueList: required<HTMLElement>("queue-list"),
  selectAllTasks: required<HTMLInputElement>("select-all-tasks"),
  resumeSelected: required<HTMLButtonElement>("resume-selected"),
  resumeSelectedLabel: required<HTMLElement>("resume-selected-label"),
  scanTime: required<HTMLElement>("scan-time"),
  pauseToggle: required<HTMLInputElement>("pause-toggle"),
  monitoringToggle: required<HTMLInputElement>("monitoring-toggle"),
  sharedAppServerToggle: required<HTMLInputElement>("shared-app-server-toggle"),
  sharedAppServerDescription: required<HTMLElement>("shared-app-server-description"),
  sharedAppServerPort: required<HTMLElement>("shared-app-server-port"),
  cliFallbackToggle: required<HTMLInputElement>("cli-fallback-toggle"),
  startupApprovalStatus: required<HTMLElement>("startup-approval-status"),
  pauseDescription: required<HTMLElement>("pause-description"),
  monitoringDescription: required<HTMLElement>("monitoring-description"),
  retryPrompt: required<HTMLTextAreaElement>("retry-prompt"),
  promptCount: required<HTMLElement>("prompt-count"),
  promptError: required<HTMLElement>("prompt-error"),
  savePrompt: required<HTMLButtonElement>("save-prompt"),
  maxRecoveryAttempts: required<HTMLInputElement>("max-recovery-attempts"),
  maxConsecutiveRetries: required<HTMLInputElement>("max-consecutive-retries"),
  memoryLimit: required<HTMLInputElement>("memory-limit-mb"),
  resumeInterval: required<HTMLInputElement>("resume-interval-seconds"),
  usageRefresh: required<HTMLInputElement>("usage-refresh-minutes"),
  delayStrategies: requiredAll<HTMLInputElement>('input[name="delay-strategy"]'),
  initialDelayLabel: required<HTMLElement>("initial-delay-label"),
  initialDelay: required<HTMLInputElement>("initial-delay"),
  maxDelay: required<HTMLInputElement>("max-delay"),
  delayIncrement: required<HTMLInputElement>("delay-increment"),
  delayPreview: required<HTMLElement>("delay-preview"),
  settingsError: required<HTMLElement>("settings-error"),
  notificationsToggle: required<HTMLInputElement>("notifications-toggle"),
  saveSettings: required<HTMLButtonElement>("save-settings"),
};

let app: App | null = null;
let snapshot: ManagementSnapshot | null = null;
let savedPrompt = "";
let savedSettings = "";
let busyCount = 0;
let noticeTimer = 0;
let statusPollInFlight = false;
const selectedTaskIds = new Set<string>();

function required<T extends HTMLElement>(id: string): T {
  const element = document.getElementById(id);
  if (!element) throw new Error(`Missing element: ${id}`);
  return element as T;
}

function requiredAll<T extends Element>(selector: string): T[] {
  const values = Array.from(document.querySelectorAll<T>(selector));
  if (values.length === 0) throw new Error(`Missing elements: ${selector}`);
  return values;
}

function refreshIcons(): void {
  createIcons({ icons: iconSet });
}

function handleHostContext(context: McpUiHostContext): void {
  if (context.theme) applyDocumentTheme(context.theme);
  if (context.styles?.variables) applyHostStyleVariables(context.styles.variables);
  if (context.styles?.css?.fonts) applyHostFonts(context.styles.css.fonts);
  if (context.safeAreaInsets) {
    const { top, right, bottom, left } = context.safeAreaInsets;
    elements.shell.style.paddingTop = `${Math.max(14, top)}px`;
    elements.shell.style.paddingRight = `${Math.max(14, right)}px`;
    elements.shell.style.paddingBottom = `${Math.max(14, bottom)}px`;
    elements.shell.style.paddingLeft = `${Math.max(14, left)}px`;
  }
}

function extractSnapshot(result: ToolResult): ManagementSnapshot | null {
  const value = result.structuredContent;
  if (!value || typeof value !== "object") return null;
  const candidate = value as Partial<ManagementSnapshot>;
  if (!Array.isArray(candidate.retries) || typeof candidate.retry_prompt !== "string") return null;
  return candidate as ManagementSnapshot;
}

function render(next: ManagementSnapshot): void {
  const keepSettingsDraft = snapshot !== null && currentSettings() !== savedSettings;
  snapshot = next;
  savedPrompt = next.retry_prompt;
  elements.version.textContent = next.version ? `v${next.version}${next.build > 0 ? ` (Build ${next.build})` : ""}` : "";
  elements.retryPrompt.disabled = false;
  elements.pauseToggle.disabled = false;
  elements.sharedAppServerToggle.disabled = false;
  if (!keepSettingsDraft) {
    elements.retryPrompt.value = next.retry_prompt;
    elements.maxRecoveryAttempts.value = String(next.max_recovery_attempts);
    elements.maxConsecutiveRetries.value = String(next.max_consecutive_retries);
    elements.memoryLimit.value = String(next.memory_limit_mb);
    elements.resumeInterval.value = String(next.resume_interval_seconds);
    elements.usageRefresh.value = String(next.usage_refresh_minutes);
    elements.initialDelay.value = String(next.initial_delay_seconds);
    elements.maxDelay.value = String(next.max_delay_seconds);
    elements.delayIncrement.value = String(next.delay_increment_seconds);
    for (const option of elements.delayStrategies) option.checked = option.value === next.delay_strategy;
    elements.notificationsToggle.checked = next.show_notifications;
    elements.cliFallbackToggle.checked = next.cli_resume_fallback_enabled;
  }
  savedSettings = serializedSettings(next);
  elements.pauseToggle.checked = !next.paused;
  elements.monitoringToggle.checked = !next.monitoring_paused;
  elements.sharedAppServerToggle.checked = next.shared_app_server_requested ?? next.shared_app_server_enabled;
  elements.sharedAppServerDescription.textContent = next.shared_app_server_enabled
    ? `正在使用已通過健康檢查的 Codex自動續跑助手（連接埠 ${next.shared_app_server_port}）`
    : next.shared_app_server_requested ? "Codex自動續跑助手暫時無法使用，啟用偏好已保留；安全啟動入口會嘗試恢復" : "預設關閉；關閉時仍可查看額度";
  elements.sharedAppServerPort.textContent = next.shared_app_server_port > 0 ? `連接埠 ${next.shared_app_server_port}` : "";
  const startupApprovalLabels: Record<ManagementSnapshot["startup_approved"], string> = {
    enabled: "Windows 登入啟動：已啟用",
    disabled: "Windows 登入啟動：已停用",
    unknown: "Windows 登入啟動：狀態未知",
  };
  elements.startupApprovalStatus.textContent = startupApprovalLabels[next.startup_approved] ?? startupApprovalLabels.unknown;
  elements.startupApprovalStatus.dataset.state = next.startup_approved;
  updatePromptState();
  renderService(next);
  renderMetrics(next);
  renderUsage(next.codex_usage);
  renderQueue(next);
  renderScanTime(next);
  if (next.notice) showNotice(next.notice, false);
  refreshIcons();
}

function renderUsage(usage: UsageSnapshot | undefined): void {
  renderUsageWindow(
    usage?.available ? usage.five_hour : undefined,
    elements.usageFiveHourRow,
    elements.usageFiveHourBar,
    elements.usageFiveHourRemaining,
    elements.usageFiveHourCountdown,
  );
  renderUsageWindow(
    usage?.available ? usage.weekly : undefined,
    elements.usageWeeklyRow,
    elements.usageWeeklyBar,
    elements.usageWeeklyRemaining,
    elements.usageWeeklyCountdown,
  );
  if (!usage?.available || !(usage.observed_at ?? usage.fetched_at)) {
    elements.usageSummary.textContent = "N/A";
    return;
  }
  const fetched = new Date(usage.observed_at ?? usage.fetched_at ?? "");
  elements.usageSummary.textContent = Number.isFinite(fetched.getTime())
    ? `更新於 ${fetched.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}`
    : "已取得最新資料";
}

function renderUsageWindow(
  windowValue: UsageWindow | undefined,
  row: HTMLElement,
  bar: HTMLElement,
  remaining: HTMLElement,
  countdown: HTMLElement,
): void {
  if (!windowValue || !Number.isFinite(windowValue.used_percent) || !Number.isFinite(windowValue.remaining_percent)
    || !Number.isFinite(windowValue.resets_at) || windowValue.resets_at <= 0) {
    remaining.textContent = "N/A";
    countdown.textContent = "N/A";
    bar.style.width = "0%";
    row.classList.remove("usage-low", "usage-critical", "usage-empty");
    delete countdown.dataset.dueAt;
    delete countdown.dataset.countdownStyle;
    countdown.removeAttribute("title");
    return;
  }
  const remainingPercent = Math.max(0, Math.min(100, windowValue.remaining_percent));
  remaining.textContent = `剩餘 ${formatPercent(remainingPercent)}%`;
  bar.style.width = `${remainingPercent}%`;
  row.classList.toggle("usage-low", remainingPercent > 5 && remainingPercent <= 20);
  row.classList.toggle("usage-critical", remainingPercent > 0 && remainingPercent <= 5);
  row.classList.toggle("usage-empty", remainingPercent <= 0);
  const resetAt = new Date(windowValue.resets_at * 1000);
  countdown.title = `Reset：${resetAt.toLocaleString([], {
    year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", second: "2-digit",
  })}`;
  countdown.dataset.dueAt = resetAt.toISOString();
  countdown.dataset.countdownStyle = "quota";
  updateCountdownElement(countdown);
}

function formatPercent(value: number): string {
  return Number.isInteger(value) ? String(value) : value.toFixed(1).replace(/\.0$/, "");
}

function renderService(next: ManagementSnapshot): void {
  const dot = document.createElement("span");
  dot.className = "status-dot";
  let label = "未執行";
  let detail = "未偵測到有效心跳";
  if (next.running && next.monitoring_paused) {
    label = "Paused";
    detail = "Task 掃描與額度更新已暫停，只維持背景 heartbeat";
    dot.classList.add("status-dot-warning");
  } else if (next.controller_state === "memory_limit_exceeded") {
    label = "記憶體保護已停止";
    detail = `背景服務記憶體 ${next.memory_usage_mb ?? 0} MB，已超過上限 ${next.memory_limit_mb} MB`;
    dot.classList.add("status-dot-danger");
  } else if (
    next.running &&
    next.cli_resume_fallback_enabled &&
    ["shared_app_server_disabled", "codex_restart_required", "codex_not_running", "codex_app_not_ready"].includes(next.controller_state ?? "")
  ) {
    label = "背景 CLI 備援待命";
    detail = "正式恢復通道不可用時，可在背景繼續同一 Session；結果不會回到原 Codex 視窗";
    dot.classList.add("status-dot-positive");
  } else if (next.running && next.controller_state === "codex_restart_required") {
    label = "Codex 未接入 Codex自動續跑助手";
    detail = "Codex自動續跑助手已啟動，但目前 Codex 仍使用官方後台；請使用安全啟動 Codex 入口";
    dot.classList.add("status-dot-warning");
  } else if (next.running && next.controller_state === "official_ipc_ready") {
    label = "Codex 已接入官方恢復通道";
    detail = "新版 Codex 使用官方 IPC，自動恢復要求會轉交目前 Task 的擁有者";
    dot.classList.add("status-dot-positive");
  } else if (next.running && next.controller_state === "codex_not_running") {
    label = "Codex 已退出";
    detail = "相關 Task 已停止自動重試；啟動 Codex 後可手動重新開始";
    dot.classList.add("status-dot-danger");
  } else if (next.running && next.controller_state === "shared_app_server_disabled") {
    label = next.shared_app_server_requested ? "Codex自動續跑助手暫時無法使用" : "Codex自動續跑助手已關閉";
    detail = "Codex 繼續使用官方後台；開啟 Codex自動續跑助手後才會執行靜默恢復";
    dot.classList.add("status-dot-warning");
  } else if (next.running && next.controller_state === "shared_app_server_port_reserved") {
    label = "連接埠被 Windows 保留";
    detail = "Codex自動續跑助手未啟動，自動重試已停止；更換連接埠後再啟用";
    dot.classList.add("status-dot-danger");
  } else if (next.running && next.controller_state === "shared_app_server_port_conflict") {
    label = "本機連接埠正在遷移";
    detail = `首選連接埠無法使用；啟用 Codex自動續跑助手時會選擇安全的本機連接埠（目前設定 ${next.shared_app_server_port}）`;
    dot.classList.add("status-dot-danger");
  } else if (next.running && next.controller_state === "shared_app_server_migration_deferred") {
    label = "等待 Codex 關閉";
    detail = "Codex自動續跑助手清理或遷移已延後，避免中斷目前 Codex 工作階段";
    dot.classList.add("status-dot-warning");
  } else if (next.running && next.controller_state === "shared_app_server_environment_conflict") {
    label = "Codex自動續跑助手環境衝突";
    detail = "偵測到 CODEX_APP_SERVER_WS_URL 已指向其他位址，程式未覆寫；請清理衝突值後再啟用";
    dot.classList.add("status-dot-danger");
  } else if (next.running && next.controller_state === "shared_app_server_ownership_unknown") {
    label = "Codex自動續跑助手歸屬未知";
    detail = "程式無法確認背景程序歸屬，已停止自動清理；請先關閉 Codex 並人工核對後再恢復";
    dot.classList.add("status-dot-danger");
  } else if (next.running && next.controller_state === "shared_app_server_config_invalid") {
    label = "Codex自動續跑助手設定不相容";
    detail = "已自動切回 Codex 官方後台，避免錯誤設定繼續影響對話";
    dot.classList.add("status-dot-danger");
  } else if (next.running && next.controller_state === "shared_app_server_memory_limit_exceeded") {
    label = "Codex自動續跑助手記憶體保護";
    detail = `Codex自動續跑助手已停止接管（${next.shared_app_server_memory_usage_mb ?? 0} MB/${next.shared_app_server_memory_limit_mb ?? 0} MB），未強制關閉 Codex`;
    dot.classList.add("status-dot-warning");
  } else if (next.running && next.controller_state && !["ready", "starting", "official_ipc_ready"].includes(next.controller_state)) {
    label = "恢復通道異常";
    detail = `自動重試已停止持續空轉：${controllerStateLabel(next.controller_state)}`;
    dot.classList.add("status-dot-danger");
  } else if (next.running && next.rate_limited_retries > 0) {
    label = "Rate Limited";
    detail = `${next.rate_limited_retries} 個 Task 正等待額度重設；到期後會重新確認再 Resume`;
    dot.classList.add("status-dot-warning");
  } else if (next.running && next.paused) {
    label = "已暫停";
    detail = "監控維持執行，新的重試暫不執行";
    dot.classList.add("status-dot-warning");
  } else if (next.running) {
    label = "執行中";
    detail = `正在監控 ${next.watched_roots} 個工作階段位置`;
    dot.classList.add("status-dot-positive");
  } else {
    dot.classList.add("status-dot-danger");
  }
  elements.serviceStatus.replaceChildren(dot, document.createTextNode(label));
  elements.serviceLine.textContent = detail;
  if (next.running && next.automatic_recovery_supported === false && next.recovery_capability_reason === "official_stdio_not_externally_controllable") {
    elements.serviceLine.textContent = `${detail}；目前為僅監控模式，尚未傳送自動恢復要求`;
  }
  if (next.memory_guard_triggered) {
    elements.serviceLine.textContent = `${detail}；記憶體保護已觸發（${next.memory_usage_mb ?? 0} MB/${next.memory_limit_mb} MB）`;
  }
  if (next.shared_app_server_memory_guard_triggered) {
    elements.serviceLine.textContent = `${elements.serviceLine.textContent}；Codex自動續跑助手記憶體保護已觸發（${next.shared_app_server_memory_usage_mb ?? 0} MB/${next.shared_app_server_memory_limit_mb ?? 0} MB），未強制關閉 Codex`;
  }
  if (next.retry_safety_warning) {
    elements.serviceLine.textContent = `${elements.serviceLine.textContent}；${next.retry_safety_warning}`;
  }
  elements.pauseDescription.textContent = next.paused ? "已暫停新的重試" : "執行中";
  elements.monitoringDescription.textContent = next.monitoring_paused ? "監控已暫停" : "監控中";
}

function renderMetrics(next: ManagementSnapshot): void {
  const total = next.pending_retries + next.active_retries + next.stopped_retries;
  elements.queueCount.textContent = String(total);
  const pending = next.retries
    .filter((retry) => retry.state === "pending" && retry.due_at)
    .sort((a, b) => Date.parse(a.due_at ?? "") - Date.parse(b.due_at ?? ""));
  if (next.monitoring_paused) {
    elements.nextRetry.textContent = "監控已暫停";
    delete elements.nextRetry.dataset.dueAt;
  } else if (next.paused && pending.length > 0) {
    elements.nextRetry.textContent = "等待恢復";
  } else if (pending.length > 0) {
    elements.nextRetry.dataset.dueAt = pending[0].due_at ?? "";
    updateCountdownElement(elements.nextRetry);
  } else if (next.active_retries > 0) {
    elements.nextRetry.textContent = "正在重試";
    delete elements.nextRetry.dataset.dueAt;
  } else {
    elements.nextRetry.textContent = "--";
    delete elements.nextRetry.dataset.dueAt;
  }
  if (total === 0) {
    elements.queueSummary.textContent = "目前沒有等待中的 Task";
  } else {
    elements.queueSummary.textContent = `${next.pending_retries} 個等待中，${next.active_retries} 個執行中，${next.stopped_retries} 個已停止`;
  }
}

function renderQueue(next: ManagementSnapshot): void {
  elements.queueList.replaceChildren();
  const selectable = next.retries.filter(canSelectForResume);
  const selectableIDs = new Set(selectable.map((retry) => retry.thread_id));
  for (const threadID of selectedTaskIds) {
    if (!selectableIDs.has(threadID)) selectedTaskIds.delete(threadID);
  }
  if (next.retries.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-state";
    empty.append(icon("activity"), document.createTextNode("佇列為空"));
    elements.queueList.append(empty);
    updateTaskSelection(next);
    return;
  }
  for (const retry of next.retries) {
    elements.queueList.append(createQueueItem(retry, next.paused || next.monitoring_paused));
  }
  updateTaskSelection(next);
}

function createQueueItem(retry: ManagedRetry, paused: boolean): HTMLElement {
  const row = document.createElement("article");
  row.className = "queue-item";
  row.dataset.threadId = retry.thread_id;

  const selector = document.createElement("input");
  selector.type = "checkbox";
  selector.className = "task-selector";
  selector.checked = selectedTaskIds.has(retry.thread_id);
  selector.disabled = !canSelectForResume(retry);
  selector.setAttribute("aria-label", `選取 ${retry.label}`);
  selector.addEventListener("change", () => {
    if (selector.checked) selectedTaskIds.add(retry.thread_id);
    else selectedTaskIds.delete(retry.thread_id);
    if (snapshot) updateTaskSelection(snapshot);
  });

  const main = document.createElement("div");
  main.className = "queue-main";
  const queueIcon = document.createElement("span");
  queueIcon.className = `queue-icon${retry.state === "pending" ? "" : retry.state === "stopped" ? " stopped" : " active"}`;
  queueIcon.append(icon(retry.state === "pending" ? "clock" : retry.state === "stopped" ? "x" : "refresh-cw"));
  const copy = document.createElement("div");
  copy.className = "queue-copy";
  const title = document.createElement("div");
  title.className = "queue-title";
  title.textContent = retry.label;
  title.title = retry.thread_id;
  const project = document.createElement("div");
  project.className = "queue-project";
  project.textContent = retry.project_path || "專案路徑：N/A";
  project.title = retry.project_path || "N/A";
  const meta = document.createElement("div");
  meta.className = "queue-meta";
  const recovery = retry.max_recovery_attempts
    ? `本次故障恢復 ${retry.recovery_attempt}/${retry.max_recovery_attempts}`
    : `本次故障恢復 ${retry.recovery_attempt}`;
  const consecutive = retry.max_consecutive_retries
    ? `連續無進展 ${retry.consecutive_retry}/${retry.max_consecutive_retries}`
    : `連續無進展 ${retry.consecutive_retry}`;
  const stateLabel = retry.state === "pending"
    ? "等待中"
    : retry.state === "stopped"
      ? stoppedStateLabel(retry)
      : actionLabel(retry.action);
  meta.append(
    textSpan(retry.task_type || "Codex", "task-type"),
    textSpan(classLabel(retry.class)),
    textSpan(recovery),
    textSpan(consecutive),
    textSpan(stateLabel),
    textSpan(formatLastActivity(retry.last_activity_at)),
  );
  copy.append(title, project, meta);
  main.append(queueIcon, copy);

  const state = document.createElement("div");
  state.className = "queue-state";
  const primary = document.createElement("strong");
  const secondary = document.createElement("span");
  if (retry.state === "pending" && retry.due_at) {
    primary.dataset.dueAt = retry.due_at;
    updateCountdownElement(primary);
    secondary.textContent = paused ? "恢復後執行" : "後重試";
  } else if (retry.state === "stopped") {
    primary.textContent = "已停止";
    secondary.textContent = stopReasonLabel(retry);
  } else {
    primary.textContent = retry.state === "running" ? "執行中" : "啟動中";
    secondary.textContent = actionLabel(retry.action);
  }
  state.append(primary, secondary);

  const actions = document.createElement("div");
  actions.className = "queue-actions";
  if (retry.can_retry_now) {
    actions.append(actionButton("play", "立即重試", "retry-action", () => runThreadAction("retry_now", retry.thread_id)));
  }
  if (retry.can_cancel) {
    actions.append(actionButton("x", "取消這次重試", "cancel-action", () => runThreadAction("cancel_retry", retry.thread_id)));
  }
  if (retry.can_restart) {
    actions.append(actionButton("rotate-ccw", "重新開始計數並重試", "retry-action", () => runThreadAction("restart_retry", retry.thread_id)));
  }
  row.append(selector, main, state, actions);
  return row;
}

function canSelectForResume(retry: ManagedRetry): boolean {
  return retry.can_retry_now || retry.can_restart;
}

function updateTaskSelection(next: ManagementSnapshot): void {
  const selectable = next.retries.filter(canSelectForResume);
  const selectedCount = selectable.filter((retry) => selectedTaskIds.has(retry.thread_id)).length;
  elements.selectAllTasks.disabled = selectable.length === 0 || busyCount > 0;
  elements.selectAllTasks.checked = selectable.length > 0 && selectedCount === selectable.length;
  elements.selectAllTasks.indeterminate = selectedCount > 0 && selectedCount < selectable.length;
  elements.resumeSelected.disabled = selectedCount === 0 || busyCount > 0;
  elements.resumeSelectedLabel.textContent = selectedCount > 0 ? `Resume 已選擇（${selectedCount}）` : "Resume 已選擇";
}

function actionButton(iconName: string, label: string, className: string, action: () => void): HTMLButtonElement {
  const button = document.createElement("button");
  button.type = "button";
  button.className = `queue-action ${className}`;
  button.title = label;
  button.setAttribute("aria-label", label);
  button.append(icon(iconName));
  button.addEventListener("click", action);
  return button;
}

function icon(name: string): HTMLElement {
  const element = document.createElement("i");
  element.dataset.lucide = name;
  element.setAttribute("aria-hidden", "true");
  return element;
}

function textSpan(value: string, className = ""): HTMLElement {
  const span = document.createElement("span");
  span.textContent = value;
  if (className) span.className = className;
  return span;
}

function formatLastActivity(value?: string): string {
  if (!value) return "最後活動：N/A";
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "最後活動：N/A";
  return `最後活動：${date.toLocaleString([], {
    month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit",
  })}`;
}

function renderScanTime(next: ManagementSnapshot): void {
  if (!next.last_scan_at) {
    elements.scanTime.textContent = "";
    return;
  }
  const date = new Date(next.last_scan_at);
  elements.scanTime.textContent = `掃描於 ${date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}`;
}

function updateCountdowns(): void {
  document.querySelectorAll<HTMLElement>("[data-due-at]").forEach(updateCountdownElement);
}

function updateCountdownElement(element: HTMLElement): void {
  const dueAt = Date.parse(element.dataset.dueAt ?? "");
  if (!Number.isFinite(dueAt)) {
    element.textContent = "--";
    return;
  }
  const seconds = Math.max(0, Math.ceil((dueAt - Date.now()) / 1000));
  element.textContent = element.dataset.countdownStyle === "quota" ? formatQuotaDuration(seconds) : formatDuration(seconds);
}

function formatQuotaDuration(totalSeconds: number): string {
  if (totalSeconds <= 0) return "重新確認中…";
  const minutes = Math.max(1, Math.ceil(totalSeconds / 60));
  if (minutes < 60) return `${minutes} 分鐘`;
  const hours = Math.ceil(minutes / 60);
  if (hours < 24) return `${hours} 小時`;
  return `${Math.ceil(hours / 24)} 天`;
}

function formatDuration(totalSeconds: number): string {
  if (totalSeconds < 60) return `${totalSeconds} 秒`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes < 60) return `${minutes} 分 ${String(seconds).padStart(2, "0")} 秒`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} 時 ${String(minutes % 60).padStart(2, "0")} 分`;
  const days = Math.floor(hours / 24);
  return `${days} 天 ${String(hours % 24).padStart(2, "0")} 時`;
}

function classLabel(value: FailureClass): string {
  const labels: Record<FailureClass, string> = {
    transient: "連線中斷",
    rate_limit: "要求遭限流",
    server: "服務供應商故障",
    auth_transient: "登入服務暫時無法使用",
    auth_limited: "登入異常",
    empty_response: "模型空白回覆",
    unknown: "未知故障",
    none: "未分類",
  };
  return labels[value] ?? "未知故障";
}

function actionLabel(value?: string): string {
  const labels: Record<string, string> = {
    dispatching: "準備恢復",
    goal_resume: "目標恢復",
    goal_active: "目標執行",
    conversation_continue: "對話繼續",
    subagent_continue: "子 Agent 恢復",
    goal_block: "目標停止",
  };
  return value ? (labels[value] ?? "正在處理") : "正在處理";
}

function stopReasonLabel(retry: ManagedRetry): string {
  if (retry.stop_reason === "codex_not_running") {
    return "Codex 已退出，自動重試已停止";
  }
  if (retry.stop_reason === "shared_app_server_disabled") {
    return "Codex自動續跑助手已關閉，Codex 仍使用官方後台";
  }
  if (retry.stop_reason === "cli_fallback_failed") {
    return "背景 CLI 無法啟動；未建立重複 Session";
  }
  if (retry.stop_reason === "cli_fallback_timeout") {
    return "30 分鐘內未收到完成事件，已停止監控且不自動重複 Resume";
  }
  if (retry.stop_reason === "codex_restart_required") {
    return "透過安全啟動 Codex 入口重新開啟後接入 Codex自動續跑助手";
  }
  if (retry.stop_reason === "codex_home_not_shared") {
    return "此 Task 不在目前 Codex 的本機工作階段目錄中";
  }
  if (retry.stop_reason === "shared_app_server_port_conflict") {
    return "首選恢復連接埠無法使用，等待安全遷移";
  }
  if (retry.stop_reason === "shared_app_server_port_reserved") {
    return "背景恢復連接埠被 Windows 保留";
  }
  if (retry.stop_reason === "shared_app_server_environment_conflict") {
    return "Codex自動續跑助手環境變數已被其他值占用";
  }
  if (retry.stop_reason === "shared_app_server_ownership_unknown") {
    return "Codex自動續跑助手歸屬無法確認，需要人工清理";
  }
  if (retry.stop_reason === "shared_app_server_config_invalid") {
    return "Codex自動續跑助手設定與目前 Codex 不相容，已自動切回官方後台";
  }
  if (retry.stop_reason === "shared_app_server_migration_deferred") {
    return "等待 Codex 關閉後完成後台遷移";
  }
  if (retry.stop_reason?.startsWith("controller_") || retry.stop_reason?.startsWith("codex_background_") || retry.stop_reason === "app_server_request_failed") {
    return "背景恢復通道連續失敗，已停止空轉";
  }
  if (retry.stop_reason === "goal_empty_response_limit_block_failed") {
    return `目標連續空白回覆達到上限，恢復已停止，但自動設為受阻失敗`;
  }
  if (retry.stop_reason === "goal_empty_response_limit") {
    return `目標連續空白回覆達到上限，目標恢復已停止`;
  }
  if (retry.stop_reason === "consecutive_retry_limit") {
    return `無進展 ${retry.consecutive_retry}/${retry.max_consecutive_retries ?? retry.consecutive_retry} 達到上限`;
  }
  if (retry.stop_reason === "recovery_time_limit") {
    return "自動恢復執行時間達到 30 分鐘上限";
  }
  return `本次恢復 ${retry.recovery_attempt}/${retry.max_recovery_attempts ?? retry.recovery_attempt} 達到上限`;
}

function stoppedStateLabel(retry: ManagedRetry): string {
  switch (retry.stop_reason) {
    case "shared_app_server_disabled":
      return "Codex自動續跑助手已關閉";
    case "codex_not_running":
      return "Codex 已退出";
    case "codex_restart_required":
      return "等待安全啟動 Codex";
    case "cli_fallback_failed":
      return "CLI 備援啟動失敗";
    case "cli_fallback_timeout":
      return "CLI 備援已達安全上限";
    case "codex_ipc_goal_control_unsupported":
      return "官方 IPC 暫不支援目標停止";
    case "subagent_recovery_event_unavailable":
      return "子 Agent 恢復事件無法使用";
    case "subagent_parent_owner_unavailable":
      return "父 Task 擁有者無法使用";
    case "subagent_parent_recovery_failed":
      return "父 Task 恢復事件失敗";
    case "codex_home_not_shared":
      return "Task 目錄未接入";
    case "shared_app_server_port_conflict":
      return "恢復連接埠衝突";
    case "shared_app_server_port_reserved":
      return "連接埠被 Windows 保留";
    case "shared_app_server_environment_conflict":
      return "Codex自動續跑助手環境衝突";
    case "shared_app_server_ownership_unknown":
      return "Codex自動續跑助手歸屬未知";
    case "shared_app_server_migration_deferred":
      return "等待 Codex 關閉";
    default:
      return "達到上限";
  }
}

function controllerStateLabel(value: string): string {
  const labels: Record<string, string> = {
    codex_restart_required: "需要安全啟動 Codex",
    official_ipc_ready: "新版 Codex 官方 IPC 已接入",
    codex_ipc_goal_control_unsupported: "官方 IPC 暫不支援目標停止，恢復已停止",
    subagent_recovery_event_unavailable: "無法確認子 Agent 的恢復事件",
    subagent_parent_owner_unavailable: "無法確認父 Task 擁有者",
    subagent_parent_recovery_failed: "父 Task 恢復事件提交失敗",
    codex_not_running: "Codex 已退出，自動重試已停止",
    shared_app_server_disabled: "Codex自動續跑助手已關閉，Codex 使用官方後台",
    codex_home_not_shared: "Task 目錄未接入本機恢復通道",
    shared_app_server_port_conflict: "本機恢復連接埠被占用",
    shared_app_server_port_reserved: "本機恢復連接埠被 Windows 保留",
    shared_app_server_environment_conflict: "CODEX_APP_SERVER_WS_URL 已被其他值占用",
    shared_app_server_ownership_unknown: "Codex自動續跑助手歸屬無法確認，需要人工清理",
    shared_app_server_migration_deferred: "等待 Codex 關閉後完成後台遷移",
    shared_app_server_config_invalid: "Codex自動續跑助手設定與目前 Codex 不相容，已切回官方後台",
    codex_background_channel_unavailable: "本機恢復通道無法使用",
    codex_background_dispatch_failed: "恢復要求失敗",
    controller_timeout: "恢復要求逾時",
    controller_unavailable: "控制器無法使用",
    cli_fallback_failed: "背景 CLI 備援啟動失敗",
    cli_fallback_timeout: "背景 CLI 備援已達 30 分鐘安全上限",
  };
  return labels[value] ?? value;
}

function updatePromptState(): void {
  const value = elements.retryPrompt.value;
  const count = Array.from(value).length;
  elements.promptCount.textContent = String(count);
  let error = "";
  if (!value.trim()) error = "重試文字不能為空白";
  else if (count > 500) error = "最多 500 個字元";
  elements.promptError.textContent = error;
  elements.savePrompt.disabled = Boolean(error) || value === savedPrompt || busyCount > 0;
  const strategy = selectedDelayStrategy();
  const initialDelay = Number(elements.initialDelay.value);
  const maxDelay = Number(elements.maxDelay.value);
  const delayIncrement = Number(elements.delayIncrement.value);
  const recoveryAttempts = Number(elements.maxRecoveryAttempts.value);
  const consecutiveRetries = Number(elements.maxConsecutiveRetries.value);
  const memoryLimit = Number(elements.memoryLimit.value);
  const resumeInterval = Number(elements.resumeInterval.value);
  const usageRefresh = Number(elements.usageRefresh.value);
  let settingsError = "";
  if (!Number.isInteger(recoveryAttempts) || recoveryAttempts < 1 || recoveryAttempts > 1000) {
    settingsError = "本次故障恢復上限應為 1 到 1000";
  } else if (!Number.isInteger(consecutiveRetries) || consecutiveRetries < 1 || consecutiveRetries > 100) {
    settingsError = "連續無進展重試上限應為 1 到 100";
  } else if (!Number.isInteger(memoryLimit) || memoryLimit < 128 || memoryLimit > 65536) {
    settingsError = "記憶體上限應為 128 到 65536 MB";
  } else if (!Number.isInteger(resumeInterval) || resumeInterval < 0 || resumeInterval > 3600) {
    settingsError = "多 Task Resume 間隔應為 0 到 3600 秒";
  } else if (!Number.isInteger(usageRefresh) || usageRefresh < 1 || usageRefresh > 1440) {
    settingsError = "額度更新頻率應為 1 到 1440 分鐘";
  } else if ((strategy !== "fixed" && strategy !== "linear" && strategy !== "exponential")
    || !Number.isInteger(initialDelay) || initialDelay < 1 || initialDelay > 3600
    || !Number.isInteger(maxDelay) || maxDelay < 1 || maxDelay > 86400
    || !Number.isInteger(delayIncrement) || delayIncrement < 1 || delayIncrement > 3600) {
    settingsError = "等待時間設定超出範圍";
  } else if (strategy !== "fixed" && maxDelay < initialDelay) {
    settingsError = "遞增等待時，最大等待不能小於首次等待";
  }
  elements.maxDelay.disabled = strategy === "fixed";
  elements.delayIncrement.disabled = strategy !== "linear";
  elements.initialDelayLabel.textContent = strategy === "fixed" ? "固定間隔（秒）" : "首次等待（秒）";
  elements.settingsError.textContent = settingsError;
  updateDelayPreview(strategy, initialDelay, maxDelay, delayIncrement, consecutiveRetries);
  elements.saveSettings.disabled = Boolean(error) || Boolean(settingsError)
    || currentSettings() === savedSettings || busyCount > 0;
}

function currentSettings(): string {
  return JSON.stringify({
    retry_prompt: elements.retryPrompt.value,
    max_recovery_attempts: Number(elements.maxRecoveryAttempts.value),
    max_consecutive_retries: Number(elements.maxConsecutiveRetries.value),
    memory_limit_mb: Number(elements.memoryLimit.value),
    resume_interval_seconds: Number(elements.resumeInterval.value),
    usage_refresh_minutes: Number(elements.usageRefresh.value),
    cli_resume_fallback_enabled: elements.cliFallbackToggle.checked,
    initial_delay_seconds: Number(elements.initialDelay.value),
    max_delay_seconds: Number(elements.maxDelay.value),
    delay_increment_seconds: Number(elements.delayIncrement.value),
    delay_strategy: selectedDelayStrategy(),
    show_notifications: elements.notificationsToggle.checked,
  });
}

function serializedSettings(value: ManagementSnapshot): string {
  return JSON.stringify({
    retry_prompt: value.retry_prompt,
    max_recovery_attempts: value.max_recovery_attempts,
    max_consecutive_retries: value.max_consecutive_retries,
    memory_limit_mb: value.memory_limit_mb,
    resume_interval_seconds: value.resume_interval_seconds,
    usage_refresh_minutes: value.usage_refresh_minutes,
    cli_resume_fallback_enabled: value.cli_resume_fallback_enabled,
    initial_delay_seconds: value.initial_delay_seconds,
    max_delay_seconds: value.max_delay_seconds,
    delay_increment_seconds: value.delay_increment_seconds,
    delay_strategy: value.delay_strategy,
    show_notifications: value.show_notifications,
  });
}

function selectedDelayStrategy(): "fixed" | "linear" | "exponential" {
  const selected = elements.delayStrategies.find((option) => option.checked)?.value;
  if (selected === "fixed" || selected === "linear") return selected;
  return "exponential";
}

function updateDelayPreview(
  strategy: "fixed" | "linear" | "exponential",
  initialDelay: number,
  maxDelay: number,
  delayIncrement: number,
  consecutiveRetries: number,
): void {
  if (!Number.isInteger(initialDelay) || initialDelay < 1 || !Number.isInteger(maxDelay) || maxDelay < 1
    || !Number.isInteger(delayIncrement) || delayIncrement < 1
    || !Number.isInteger(consecutiveRetries) || consecutiveRetries < 1) {
    elements.delayPreview.textContent = "";
    return;
  }
  const visibleCount = Math.min(consecutiveRetries, 8);
  const delays: number[] = [];
  let delay = initialDelay;
  for (let index = 0; index < visibleCount; index += 1) {
    delays.push(strategy === "fixed" ? initialDelay : Math.min(delay, maxDelay));
    if (strategy === "exponential") delay = Math.min(delay * 2, maxDelay);
    if (strategy === "linear") delay = Math.min(delay + delayIncrement, maxDelay);
  }
  const suffix = consecutiveRetries > visibleCount ? "，…" : "";
  elements.delayPreview.textContent = `等待序列：${delays.map(formatPreviewDelay).join("，")}${suffix}`;
}

function formatPreviewDelay(seconds: number): string {
  if (seconds < 60) return `${seconds} 秒`;
  if (seconds % 3600 === 0) return `${seconds / 3600} 小時`;
  if (seconds % 60 === 0) return `${seconds / 60} 分鐘`;
  return `${seconds} 秒`;
}

function setBusy(active: boolean): void {
  busyCount = Math.max(0, busyCount + (active ? 1 : -1));
  const busy = busyCount > 0;
  elements.refreshButton.disabled = busy;
  elements.refreshButton.classList.toggle("is-spinning", busy);
  elements.pauseToggle.disabled = busy || !snapshot;
  elements.sharedAppServerToggle.disabled = busy || !snapshot;
  elements.cliFallbackToggle.disabled = busy || !snapshot;
  document.querySelectorAll<HTMLButtonElement>(".queue-action").forEach((button) => {
    button.disabled = busy;
  });
  document.querySelectorAll<HTMLInputElement>(".task-selector").forEach((input) => {
    input.disabled = busy || !snapshot?.retries.some((retry) => retry.thread_id === input.closest<HTMLElement>("[data-thread-id]")?.dataset.threadId && canSelectForResume(retry));
  });
  if (snapshot) updateTaskSelection(snapshot);
  updatePromptState();
}

async function callTool(name: string, args: Record<string, unknown> = {}, quiet = false): Promise<void> {
  if (!app) {
    showNotice("管理面板尚未連線", true);
    return;
  }
  if (quiet) {
    if (statusPollInFlight) return;
    statusPollInFlight = true;
  } else {
    setBusy(true);
  }
  try {
    const result = (await app.callServerTool({ name, arguments: args })) as ToolResult;
    const next = extractSnapshot(result);
    if (next) render(next);
    else if (result.isError) throw new Error(result.content?.find((item) => item.text)?.text ?? "操作失敗");
  } catch (error) {
    if (name === "set_shared_app_server_enabled") {
      // A failed health check may still have persisted the user's preference.
      try {
        const latest = extractSnapshot((await app.callServerTool({ name: "get_auto_retry_status", arguments: {} })) as ToolResult);
        if (latest) render(latest);
      } catch { /* Retain the last known status when the status read also fails. */ }
    }
    if (name === "set_shared_app_server_enabled" && snapshot) {
      elements.sharedAppServerToggle.checked = snapshot.shared_app_server_requested ?? snapshot.shared_app_server_enabled;
    }
    if (!quiet) showNotice(error instanceof Error ? error.message : "操作失敗", true);
  } finally {
    if (quiet) {
      statusPollInFlight = false;
    } else {
      setBusy(false);
    }
  }
}

async function runThreadAction(name: "retry_now" | "cancel_retry" | "restart_retry", threadId: string): Promise<void> {
  await callTool(name, { thread_id: threadId });
  window.setTimeout(() => void callTool("get_auto_retry_status"), 1200);
}

async function resumeSelectedTasks(): Promise<void> {
  if (!app || !snapshot) return;
  const targets = snapshot.retries.filter((retry) => selectedTaskIds.has(retry.thread_id) && canSelectForResume(retry));
  if (targets.length === 0) return;
  setBusy(true);
  let completed = 0;
  try {
    for (const retry of targets) {
      const name = retry.can_restart ? "restart_retry" : "retry_now";
      const result = (await app.callServerTool({ name, arguments: { thread_id: retry.thread_id } })) as ToolResult;
      if (result.isError) throw new Error(result.content?.find((item) => item.text)?.text ?? `${retry.label} 操作失敗`);
      completed += 1;
    }
    selectedTaskIds.clear();
    const latest = extractSnapshot((await app.callServerTool({ name: "get_auto_retry_status", arguments: {} })) as ToolResult);
    if (latest) render(latest);
    showNotice(`已排入 ${completed} 個 Task；系統會依 Resume 間隔依序啟動`, false);
  } catch (error) {
    showNotice(`${completed} 個已排入；${error instanceof Error ? error.message : "其餘操作失敗"}`, true);
  } finally {
    setBusy(false);
  }
}

function showNotice(message: string, isError: boolean): void {
  window.clearTimeout(noticeTimer);
  elements.notice.textContent = message;
  elements.notice.classList.toggle("is-error", isError);
  elements.notice.hidden = false;
  noticeTimer = window.setTimeout(() => {
    elements.notice.hidden = true;
  }, isError ? 7000 : 4200);
}

elements.refreshButton.addEventListener("click", () => void callTool("get_auto_retry_status"));
elements.selectAllTasks.addEventListener("change", () => {
  if (!snapshot) return;
  for (const retry of snapshot.retries.filter(canSelectForResume)) {
    if (elements.selectAllTasks.checked) selectedTaskIds.add(retry.thread_id);
    else selectedTaskIds.delete(retry.thread_id);
  }
  renderQueue(snapshot);
});
elements.resumeSelected.addEventListener("click", () => void resumeSelectedTasks());
elements.pauseToggle.addEventListener("change", () => void callTool("set_auto_retry_paused", { paused: !elements.pauseToggle.checked }));
elements.monitoringToggle.addEventListener("change", () => void callTool("set_monitoring_paused", { paused: !elements.monitoringToggle.checked }));
  elements.sharedAppServerToggle.addEventListener("change", () => {
  const enabled = elements.sharedAppServerToggle.checked;
  elements.sharedAppServerDescription.textContent = enabled
    ? `正在使用已通過健康檢查的 Codex自動續跑助手（連接埠 ${snapshot?.shared_app_server_port ?? ""}）`
    : "預設關閉；關閉時仍可查看額度";
  void callTool("set_shared_app_server_enabled", { enabled });
});
elements.retryPrompt.addEventListener("input", updatePromptState);
elements.maxRecoveryAttempts.addEventListener("input", updatePromptState);
elements.maxConsecutiveRetries.addEventListener("input", updatePromptState);
elements.memoryLimit.addEventListener("input", updatePromptState);
elements.resumeInterval.addEventListener("input", updatePromptState);
elements.usageRefresh.addEventListener("input", updatePromptState);
for (const option of elements.delayStrategies) option.addEventListener("change", updatePromptState);
elements.initialDelay.addEventListener("input", updatePromptState);
elements.maxDelay.addEventListener("input", updatePromptState);
elements.delayIncrement.addEventListener("input", updatePromptState);
elements.notificationsToggle.addEventListener("change", updatePromptState);
elements.cliFallbackToggle.addEventListener("change", updatePromptState);
elements.savePrompt.addEventListener("click", () => void callTool("set_retry_prompt", { prompt: elements.retryPrompt.value }));
elements.saveSettings.addEventListener("click", () => void callTool("set_retry_settings", JSON.parse(currentSettings()) as Record<string, unknown>));

refreshIcons();
window.setInterval(updateCountdowns, 1000);
window.setInterval(() => {
  if (app && busyCount === 0) void callTool("get_auto_retry_status", {}, true);
}, 5000);

if (new URLSearchParams(window.location.search).has("preview")) {
  render(previewSnapshot());
} else {
  app = new App({ name: "CodexWorkResumer", version: "1.3.1" });
  app.onerror = (error) => showNotice(error instanceof Error ? error.message : "連線失敗", true);
  app.onhostcontextchanged = handleHostContext;
  app.ontoolresult = (result) => {
    const next = extractSnapshot(result as ToolResult);
    if (next) render(next);
  };
  app.connect()
    .then(() => {
      const context = app?.getHostContext();
      if (context) handleHostContext(context);
      return callTool("get_auto_retry_status");
    })
    .catch((error) => showNotice(error instanceof Error ? error.message : "連線失敗", true));
}

function previewSnapshot(): ManagementSnapshot {
  const now = Date.now();
  return {
    version: "1.3.1",
    build: 15,
    running: true,
    heartbeat_stale: false,
    paused: false,
    monitoring_paused: false,
    shared_app_server_enabled: false,
    cli_resume_fallback_enabled: false,
    startup_approved: "enabled",
    shared_app_server_port: 49621,
    retry_prompt: "繼續",
    max_recovery_attempts: 15,
    max_consecutive_retries: 5,
    memory_limit_mb: 1024,
    shared_app_server_memory_usage_mb: 0,
    shared_app_server_memory_limit_mb: 4096,
    shared_app_server_memory_guard_triggered: false,
    retry_safety_warning: "",
    initial_delay_seconds: 5,
    max_delay_seconds: 300,
    delay_increment_seconds: 2,
    delay_strategy: "exponential",
    resume_interval_seconds: 30,
    usage_refresh_minutes: 3,
    controller_state: "ready",
    show_notifications: true,
    now: new Date(now).toISOString(),
    last_scan_at: new Date(now - 1300).toISOString(),
    pending_retries: 2,
    active_retries: 1,
    stopped_retries: 1,
    rate_limited_retries: 1,
    watched_roots: 3,
    codex_usage: {
      adapter: "codex",
      available: true,
      source: "account/rateLimits/read",
      fetched_at: new Date(now - 15_000).toISOString(),
      observed_at: new Date(now - 15_000).toISOString(),
      five_hour: {
        used_percent: 72,
        remaining_percent: 28,
        window_duration_mins: 300,
        resets_at: Math.floor((now + 48 * 60_000) / 1000),
      },
      weekly: {
        used_percent: 44,
        remaining_percent: 56,
        window_duration_mins: 10080,
        resets_at: Math.floor((now + 345_600_000) / 1000),
      },
    },
    retries: [
      {
        thread_id: "019f9d5d-9c82-75b1-b7c0-20a658af0423",
        label: "CodexWorkResumer · 019f9d5d",
        project_path: "C:\\Projects\\CodexWorkResumer",
        task_type: "Codex",
        last_activity_at: new Date(now - 45_000).toISOString(),
        state: "running",
        class: "server",
        seconds_remaining: 0,
        recovery_attempt: 1,
        max_recovery_attempts: 15,
        consecutive_retry: 1,
        max_consecutive_retries: 5,
        action: "goal_resume",
        can_retry_now: false,
        can_cancel: false,
        can_restart: false,
      },
      {
        thread_id: "019f9d5d-9c82-75b1-b7c0-20a658af0424",
        label: "Research · 019f9d5e",
        project_path: "C:\\Projects\\Research",
        task_type: "Codex",
        last_activity_at: new Date(now - 120_000).toISOString(),
        state: "pending",
        class: "rate_limit",
        due_at: new Date(now + 42_000).toISOString(),
        seconds_remaining: 42,
        recovery_attempt: 4,
        max_recovery_attempts: 15,
        consecutive_retry: 1,
        max_consecutive_retries: 5,
        can_retry_now: true,
        can_cancel: true,
        can_restart: false,
      },
      {
        thread_id: "019f9d5d-9c82-75b1-b7c0-20a658af0425",
        label: "Website · 019f9d5f",
        project_path: "C:\\Projects\\Website",
        task_type: "Codex",
        last_activity_at: new Date(now - 300_000).toISOString(),
        state: "pending",
        class: "transient",
        due_at: new Date(now + 126_000).toISOString(),
        seconds_remaining: 126,
        recovery_attempt: 3,
        max_recovery_attempts: 15,
        consecutive_retry: 2,
        max_consecutive_retries: 5,
        can_retry_now: true,
        can_cancel: true,
        can_restart: false,
      },
      {
        thread_id: "019f9d5d-9c82-75b1-b7c0-20a658af0426",
        label: "Mobile · 019f9d60",
        project_path: "C:\\Projects\\Mobile",
        task_type: "Codex",
        last_activity_at: new Date(now - 900_000).toISOString(),
        state: "stopped",
        class: "server",
        seconds_remaining: 0,
        recovery_attempt: 15,
        max_recovery_attempts: 15,
        consecutive_retry: 5,
        max_consecutive_retries: 5,
        can_retry_now: false,
        can_cancel: false,
        can_restart: true,
      },
    ],
  };
}
