package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	adapterCodex AdapterKind = "codex"
	adapterWork  AdapterKind = "work"

	fiveHourWindowMinutes = 5 * 60
	weeklyWindowMinutes   = 7 * 24 * 60
	defaultUsageRefresh   = 3 * time.Minute
)

var errAdapterUnavailable = errors.New("task adapter is unavailable")

type AdapterKind string

type AdapterCapabilities struct {
	Usage  bool `json:"usage"`
	Tasks  bool `json:"tasks"`
	Resume bool `json:"resume"`
}

type AdapterStatus struct {
	Kind              AdapterKind         `json:"kind"`
	Capabilities      AdapterCapabilities `json:"capabilities"`
	Available         bool                `json:"available"`
	UnavailableReason string              `json:"unavailable_reason,omitempty"`
}

type UsageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	RemainingPercent   float64 `json:"remaining_percent"`
	WindowDurationMins int     `json:"window_duration_mins"`
	ResetsAt           int64   `json:"resets_at"`
}

type UsageSnapshot struct {
	Adapter           AdapterKind  `json:"adapter"`
	Available         bool         `json:"available"`
	Refreshing        bool         `json:"refreshing,omitempty"`
	Source            string       `json:"source,omitempty"`
	FetchedAt         string       `json:"fetched_at,omitempty"`
	ObservedAt        string       `json:"observed_at,omitempty"`
	UnavailableReason string       `json:"unavailable_reason,omitempty"`
	RateLimitReached  string       `json:"rate_limit_reached_type,omitempty"`
	FiveHour          *UsageWindow `json:"five_hour,omitempty"`
	Weekly            *UsageWindow `json:"weekly,omitempty"`
}

type TaskAdapter interface {
	Kind() AdapterKind
	CacheKey() string
	Capabilities() AdapterCapabilities
	Status() AdapterStatus
	ReadUsage(context.Context) (UsageSnapshot, error)
}

type appServerCaller interface {
	Call(context.Context, string, any, any) error
	Close() error
}

type appServerDialer func(context.Context, string) (appServerCaller, error)
type sidecarAppServerDialer func(context.Context) (appServerCaller, error)

// CodexAdapter keeps all Codex app-server behavior behind one boundary so a
// future Work adapter cannot alter the stable Codex recovery path.
type CodexAdapter struct {
	endpoint      string
	dial          appServerDialer
	sidecarDial   sidecarAppServerDialer
	rolloutRoots  []sessionRoot
	rolloutMaxAge time.Duration
	now           func() time.Time
}

func NewCodexAdapter(endpoint string) *CodexAdapter {
	return NewCodexAdapterForConfig(endpoint, defaultConfig())
}

func NewCodexAdapterForConfig(endpoint string, config Config) *CodexAdapter {
	return &CodexAdapter{
		endpoint: endpoint,
		dial: func(ctx context.Context, endpoint string) (appServerCaller, error) {
			return dialAppServerRPC(ctx, endpoint)
		},
		sidecarDial:   dialCodexAppServerSidecar,
		rolloutRoots:  discoverSessionRoots(config),
		rolloutMaxAge: defaultRolloutUsageMaxAge,
		now:           func() time.Time { return time.Now().UTC() },
	}
}

func (a *CodexAdapter) Kind() AdapterKind { return adapterCodex }

func (a *CodexAdapter) CacheKey() string {
	if a == nil {
		return string(adapterCodex)
	}
	if a.endpoint == "" {
		return string(adapterCodex) + ":stdio"
	}
	return string(adapterCodex) + ":" + a.endpoint
}

func (a *CodexAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Usage: true, Tasks: true, Resume: true}
}

func (a *CodexAdapter) Status() AdapterStatus {
	available := a != nil && ((a.endpoint != "" && a.dial != nil) || (a.sidecarDial != nil && codexAppServerSidecarSupported()))
	status := AdapterStatus{Kind: adapterCodex, Capabilities: a.Capabilities(), Available: available}
	if !available {
		status.UnavailableReason = "app_server_unavailable"
	}
	return status
}

func (a *CodexAdapter) ReadUsage(ctx context.Context) (UsageSnapshot, error) {
	if a == nil {
		return UsageSnapshot{Adapter: adapterCodex, UnavailableReason: "app_server_unavailable"}, errAdapterUnavailable
	}
	var client appServerCaller
	var err error
	if a.endpoint != "" && a.dial != nil {
		client, err = a.dial(ctx, a.endpoint)
	}
	if client == nil && a.sidecarDial != nil {
		client, err = a.sidecarDial(ctx)
	}
	if client == nil && err == nil {
		err = errAdapterUnavailable
	}
	if err != nil {
		return a.readRolloutUsageFallback("app_server_unavailable", err)
	}
	defer client.Close()

	var result appServerRateLimitsResult
	if err := client.Call(ctx, "account/rateLimits/read", map[string]any{}, &result); err != nil {
		return a.readRolloutUsageFallback("rate_limits_read_failed", err)
	}
	now := time.Now().UTC()
	if a.now != nil {
		now = a.now().UTC()
	}
	snapshot := usageSnapshotFromRateLimits(result)
	snapshot.Adapter = adapterCodex
	snapshot.Source = "account/rateLimits/read"
	snapshot.FetchedAt = now.Format(time.RFC3339Nano)
	snapshot.ObservedAt = snapshot.FetchedAt
	snapshot.Available = snapshot.FiveHour != nil || snapshot.Weekly != nil
	if !snapshot.Available {
		snapshot.UnavailableReason = "rate_limits_unavailable"
	}
	return snapshot, nil
}

func (a *CodexAdapter) readRolloutUsageFallback(reason string, primaryErr error) (UsageSnapshot, error) {
	now := time.Now().UTC()
	if a != nil && a.now != nil {
		now = a.now().UTC()
	}
	maxAge := defaultRolloutUsageMaxAge
	var roots []sessionRoot
	if a != nil {
		roots = a.rolloutRoots
		if a.rolloutMaxAge > 0 {
			maxAge = a.rolloutMaxAge
		}
	}
	if fallback, err := readLatestRolloutUsage(roots, now, maxAge); err == nil {
		return fallback, nil
	}
	return UsageSnapshot{Adapter: adapterCodex, Source: "account/rateLimits/read", UnavailableReason: reason}, primaryErr
}

// WorkAdapter is intentionally unavailable until OpenAI exposes a supported
// Work task API or an isolated UI-automation implementation is added.
type WorkAdapter struct{}

func NewWorkAdapter() *WorkAdapter { return &WorkAdapter{} }

func (*WorkAdapter) Kind() AdapterKind                 { return adapterWork }
func (*WorkAdapter) CacheKey() string                  { return string(adapterWork) }
func (*WorkAdapter) Capabilities() AdapterCapabilities { return AdapterCapabilities{} }
func (a *WorkAdapter) Status() AdapterStatus {
	return AdapterStatus{
		Kind: adapterWork, Capabilities: a.Capabilities(), Available: false,
		UnavailableReason: "no_supported_work_interface",
	}
}
func (*WorkAdapter) ReadUsage(context.Context) (UsageSnapshot, error) {
	return UsageSnapshot{Adapter: adapterWork, UnavailableReason: "no_supported_work_interface"}, errAdapterUnavailable
}

type appServerRateLimitWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *int     `json:"windowDurationMins"`
	ResetsAt           *int64   `json:"resetsAt"`
}

type appServerRateLimit struct {
	LimitID              string                    `json:"limitId"`
	Primary              *appServerRateLimitWindow `json:"primary"`
	Secondary            *appServerRateLimitWindow `json:"secondary"`
	RateLimitReachedType string                    `json:"rateLimitReachedType"`
}

type appServerRateLimitsResult struct {
	RateLimits          *appServerRateLimit           `json:"rateLimits"`
	RateLimitsByLimitID map[string]appServerRateLimit `json:"rateLimitsByLimitId"`
}

func usageSnapshotFromRateLimits(result appServerRateLimitsResult) UsageSnapshot {
	var selected *appServerRateLimit
	if codex, ok := result.RateLimitsByLimitID["codex"]; ok {
		selected = &codex
	} else if result.RateLimits != nil && (result.RateLimits.LimitID == "" || result.RateLimits.LimitID == "codex") {
		selected = result.RateLimits
	} else if len(result.RateLimitsByLimitID) > 0 {
		// Keep fallback deterministic for older servers that omit the canonical
		// key but still return exactly one Codex-compatible bucket.
		keys := make([]string, 0, len(result.RateLimitsByLimitID))
		for key := range result.RateLimitsByLimitID {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			limit := result.RateLimitsByLimitID[key]
			if limit.LimitID == "codex" {
				selected = &limit
				break
			}
		}
	}

	snapshot := UsageSnapshot{Adapter: adapterCodex}
	if selected == nil {
		return snapshot
	}
	snapshot.RateLimitReached = strings.TrimSpace(selected.RateLimitReachedType)
	windows := []*appServerRateLimitWindow{selected.Primary, selected.Secondary}
	for _, window := range windows {
		value := normalizeUsageWindow(window)
		if value == nil {
			continue
		}
		switch value.WindowDurationMins {
		case fiveHourWindowMinutes:
			snapshot.FiveHour = value
		case weeklyWindowMinutes:
			snapshot.Weekly = value
		}
	}
	return snapshot
}

const quotaResetGrace = 5 * time.Second

// quotaWaitUntil turns the server's current quota snapshot into a one-shot
// gate decision. A caller must fetch a fresh snapshot each time this deadline
// is reached; the deadline itself is never treated as proof that quota reset.
func quotaWaitUntil(snapshot UsageSnapshot, now time.Time, unavailableRetry time.Duration) (time.Time, bool) {
	if unavailableRetry <= 0 {
		unavailableRetry = defaultUsageRefresh
	}
	if !snapshot.Available {
		return now.Add(unavailableRetry), true
	}

	exhausted := make([]*UsageWindow, 0, 2)
	for _, window := range []*UsageWindow{snapshot.FiveHour, snapshot.Weekly} {
		if window != nil && window.UsedPercent >= 100 {
			exhausted = append(exhausted, window)
		}
	}
	serverReached := strings.TrimSpace(snapshot.RateLimitReached) != ""
	if len(exhausted) == 0 && !serverReached {
		return time.Time{}, false
	}

	// If percentages have not caught up with the server's reached signal, use
	// the earliest known reset as a safe recheck point. If multiple windows are
	// visibly exhausted, all of them must recover, so wait for the latest reset.
	targets := exhausted
	latest := len(exhausted) > 0
	if len(targets) == 0 {
		for _, window := range []*UsageWindow{snapshot.FiveHour, snapshot.Weekly} {
			if window != nil {
				targets = append(targets, window)
			}
		}
	}
	var reset time.Time
	for _, window := range targets {
		candidate := time.Unix(window.ResetsAt, 0).UTC()
		if candidate.IsZero() {
			continue
		}
		if reset.IsZero() || (latest && candidate.After(reset)) || (!latest && candidate.Before(reset)) {
			reset = candidate
		}
	}
	if reset.IsZero() || !reset.After(now) {
		return now.Add(unavailableRetry), true
	}
	return reset.Add(quotaResetGrace), true
}

// A historical failed turn remains failed after the account has quota again.
// Use a fresh official snapshot to distinguish that history from a current
// quota block. Both displayed windows must be present before claiming recovery.
func quotaAvailableNow(snapshot UsageSnapshot, now time.Time) bool {
	if !snapshot.Available || snapshot.Refreshing || snapshot.Source != "account/rateLimits/read" || snapshot.FiveHour == nil || snapshot.Weekly == nil {
		return false
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, snapshot.FetchedAt)
	if err != nil || now.Before(fetchedAt) || now.Sub(fetchedAt) > 2*time.Minute {
		return false
	}
	_, wait := quotaWaitUntil(snapshot, now, defaultUsageRefresh)
	return !wait
}

func normalizeUsageWindow(window *appServerRateLimitWindow) *UsageWindow {
	if window == nil || window.UsedPercent == nil || window.WindowDurationMins == nil || window.ResetsAt == nil ||
		*window.WindowDurationMins <= 0 || *window.ResetsAt <= 0 || math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) {
		return nil
	}
	used := math.Max(0, math.Min(100, *window.UsedPercent))
	return &UsageWindow{
		UsedPercent: used, RemainingPercent: 100 - used,
		WindowDurationMins: *window.WindowDurationMins, ResetsAt: *window.ResetsAt,
	}
}

type usageMonitor struct {
	mu              sync.Mutex
	refreshInterval time.Duration
	cacheKey        string
	lastAttempt     time.Time
	value           UsageSnapshot
	generation      uint64
	refreshing      bool
}

type usageMonitorStatus struct {
	LastAttempt time.Time
	NextRefresh time.Time
}

func newUsageMonitor(refreshInterval time.Duration) *usageMonitor {
	if refreshInterval <= 0 {
		refreshInterval = defaultUsageRefresh
	}
	return &usageMonitor{refreshInterval: refreshInterval}
}

func (m *usageMonitor) SetRefreshInterval(refreshInterval time.Duration) {
	if refreshInterval <= 0 {
		refreshInterval = defaultUsageRefresh
	}
	m.mu.Lock()
	m.refreshInterval = refreshInterval
	m.mu.Unlock()
}

func (m *usageMonitor) Invalidate() {
	m.mu.Lock()
	m.lastAttempt = time.Time{}
	m.generation++
	m.refreshing = false
	m.mu.Unlock()
}

func (m *usageMonitor) Status() usageMonitorStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := usageMonitorStatus{LastAttempt: m.lastAttempt}
	if !m.lastAttempt.IsZero() {
		result.NextRefresh = m.lastAttempt.Add(m.refreshInterval)
	}
	return result
}

func (m *usageMonitor) Snapshot(ctx context.Context, now time.Time, adapter TaskAdapter) UsageSnapshot {
	if adapter == nil {
		return UsageSnapshot{Adapter: adapterCodex, UnavailableReason: "adapter_unavailable"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := adapter.CacheKey()
	if key == m.cacheKey && !m.lastAttempt.IsZero() && now.Sub(m.lastAttempt) >= 0 && now.Sub(m.lastAttempt) < m.refreshInterval {
		value := m.value
		value.Refreshing = m.refreshing
		return value
	}
	m.cacheKey = key
	m.lastAttempt = now
	value, err := adapter.ReadUsage(ctx)
	if err != nil {
		// Never retain an earlier successful sample after a refresh failure.
		value = UsageSnapshot{Adapter: adapter.Kind(), Available: false, UnavailableReason: usageErrorReason(value)}
	}
	m.value = value
	m.refreshing = false
	return value
}

// SnapshotAsync is used by the Windows tray UI so a slow app-server quota
// read can never block its message loop. Starting a refresh immediately
// retains the last valid sample while visibly marking it as refreshing. A
// failed refresh still replaces the old value with N/A, so stale quota is
// never presented as the latest completed result.
func (m *usageMonitor) SnapshotAsync(now time.Time, adapter TaskAdapter) UsageSnapshot {
	if adapter == nil {
		return UsageSnapshot{Adapter: adapterCodex, UnavailableReason: "adapter_unavailable"}
	}
	m.mu.Lock()
	key := adapter.CacheKey()
	if key == m.cacheKey && !m.lastAttempt.IsZero() && now.Sub(m.lastAttempt) >= 0 && now.Sub(m.lastAttempt) < m.refreshInterval {
		value := m.value
		value.Refreshing = m.refreshing
		m.mu.Unlock()
		return value
	}
	previous := m.value
	keepPrevious := key == m.cacheKey && previous.Available
	m.cacheKey = key
	m.lastAttempt = now
	m.generation++
	generation := m.generation
	m.refreshing = true
	pending := unavailableUsage(adapter.Kind(), "usage_refresh_pending")
	if keepPrevious {
		pending = previous
	}
	pending.Refreshing = true
	m.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		value, err := adapter.ReadUsage(ctx)
		cancel()
		if err != nil {
			value = UsageSnapshot{Adapter: adapter.Kind(), Available: false, UnavailableReason: usageErrorReason(value)}
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.generation != generation || m.cacheKey != key {
			return
		}
		value.Refreshing = false
		m.value = value
		m.refreshing = false
	}()
	return pending
}

func usageErrorReason(value UsageSnapshot) string {
	if value.UnavailableReason != "" {
		return value.UnavailableReason
	}
	return "usage_unavailable"
}

func unavailableUsage(kind AdapterKind, reason string) UsageSnapshot {
	return UsageSnapshot{Adapter: kind, Available: false, UnavailableReason: reason}
}

func adapterEndpoint(port int) string {
	if port < 1 || port > 65535 {
		return ""
	}
	return fmt.Sprintf("ws://127.0.0.1:%d", port)
}
