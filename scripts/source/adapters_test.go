package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeAdapter struct {
	value UsageSnapshot
	err   error
	calls int
}

type fakeRateLimitCaller struct {
	method string
	result appServerRateLimitsResult
}

func (f *fakeRateLimitCaller) Call(_ context.Context, method string, _ any, destination any) error {
	f.method = method
	target, ok := destination.(*appServerRateLimitsResult)
	if !ok {
		return errors.New("unexpected destination")
	}
	*target = f.result
	return nil
}

func (*fakeRateLimitCaller) Close() error { return nil }

func (f *fakeAdapter) Kind() AdapterKind { return adapterCodex }
func (f *fakeAdapter) CacheKey() string  { return "codex:test" }
func (f *fakeAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Usage: true, Tasks: true, Resume: true}
}
func (f *fakeAdapter) Status() AdapterStatus {
	return AdapterStatus{Kind: adapterCodex, Capabilities: f.Capabilities(), Available: true}
}
func (f *fakeAdapter) ReadUsage(context.Context) (UsageSnapshot, error) {
	f.calls++
	return f.value, f.err
}

func TestUsageSnapshotSelectsFiveHourAndWeeklyWindows(t *testing.T) {
	usedFive, usedWeek := 72.5, 44.0
	five, week := fiveHourWindowMinutes, weeklyWindowMinutes
	resetFive, resetWeek := int64(1_800_000_000), int64(1_800_500_000)
	result := appServerRateLimitsResult{RateLimitsByLimitID: map[string]appServerRateLimit{
		"codex": {
			LimitID:              "codex",
			Primary:              &appServerRateLimitWindow{UsedPercent: &usedFive, WindowDurationMins: &five, ResetsAt: &resetFive},
			Secondary:            &appServerRateLimitWindow{UsedPercent: &usedWeek, WindowDurationMins: &week, ResetsAt: &resetWeek},
			RateLimitReachedType: "primary",
		},
	}}
	snapshot := usageSnapshotFromRateLimits(result)
	if snapshot.FiveHour == nil || snapshot.Weekly == nil {
		t.Fatalf("expected both quota windows: %+v", snapshot)
	}
	if snapshot.FiveHour.UsedPercent != 72.5 || snapshot.FiveHour.RemainingPercent != 27.5 || snapshot.FiveHour.ResetsAt != resetFive {
		t.Fatalf("incorrect five-hour window: %+v", snapshot.FiveHour)
	}
	if snapshot.Weekly.UsedPercent != 44 || snapshot.Weekly.RemainingPercent != 56 || snapshot.Weekly.ResetsAt != resetWeek {
		t.Fatalf("incorrect weekly window: %+v", snapshot.Weekly)
	}
	if snapshot.RateLimitReached != "primary" {
		t.Fatalf("server quota classification was lost: %+v", snapshot)
	}
}

func TestCodexAdapterUsesOfficialRateLimitsMethod(t *testing.T) {
	used := 12.0
	duration := fiveHourWindowMinutes
	reset := int64(1_800_000_000)
	caller := &fakeRateLimitCaller{result: appServerRateLimitsResult{RateLimits: &appServerRateLimit{
		LimitID: "codex",
		Primary: &appServerRateLimitWindow{UsedPercent: &used, WindowDurationMins: &duration, ResetsAt: &reset},
	}}}
	adapter := NewCodexAdapter("ws://127.0.0.1:49621")
	adapter.dial = func(context.Context, string) (appServerCaller, error) { return caller, nil }
	snapshot, err := adapter.ReadUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caller.method != "account/rateLimits/read" {
		t.Fatalf("unexpected app-server method: %q", caller.method)
	}
	if !snapshot.Available || snapshot.FiveHour == nil || snapshot.FiveHour.UsedPercent != 12 {
		t.Fatalf("unexpected usage snapshot: %+v", snapshot)
	}
}

func TestCodexAdapterUsesSidecarWithoutSharedEndpoint(t *testing.T) {
	used := 18.0
	duration := fiveHourWindowMinutes
	reset := int64(1_800_000_000)
	caller := &fakeRateLimitCaller{result: appServerRateLimitsResult{RateLimits: &appServerRateLimit{
		LimitID: "codex",
		Primary: &appServerRateLimitWindow{UsedPercent: &used, WindowDurationMins: &duration, ResetsAt: &reset},
	}}}
	adapter := NewCodexAdapter("")
	adapter.sidecarDial = func(context.Context) (appServerCaller, error) { return caller, nil }
	snapshot, err := adapter.ReadUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caller.method != "account/rateLimits/read" || !snapshot.Available || snapshot.FiveHour == nil {
		t.Fatalf("sidecar did not return official quota: %+v", snapshot)
	}
}

func TestCodexAdapterFallsBackToSidecarWhenSharedEndpointIsUnavailable(t *testing.T) {
	used := 22.0
	duration := fiveHourWindowMinutes
	reset := int64(1_800_000_000)
	caller := &fakeRateLimitCaller{result: appServerRateLimitsResult{RateLimits: &appServerRateLimit{
		LimitID: "codex",
		Primary: &appServerRateLimitWindow{UsedPercent: &used, WindowDurationMins: &duration, ResetsAt: &reset},
	}}}
	adapter := NewCodexAdapter("ws://127.0.0.1:49621")
	adapter.dial = func(context.Context, string) (appServerCaller, error) { return nil, errors.New("offline") }
	adapter.sidecarDial = func(context.Context) (appServerCaller, error) { return caller, nil }
	snapshot, err := adapter.ReadUsage(context.Background())
	if err != nil || snapshot.FiveHour == nil || snapshot.FiveHour.UsedPercent != used {
		t.Fatalf("sidecar fallback failed: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestUsageSnapshotDoesNotMislabelOtherDurations(t *testing.T) {
	used := 25.0
	duration := 60
	reset := int64(1_800_000_000)
	result := appServerRateLimitsResult{RateLimits: &appServerRateLimit{
		LimitID: "codex",
		Primary: &appServerRateLimitWindow{UsedPercent: &used, WindowDurationMins: &duration, ResetsAt: &reset},
	}}
	snapshot := usageSnapshotFromRateLimits(result)
	if snapshot.FiveHour != nil || snapshot.Weekly != nil {
		t.Fatalf("unexpected named quota window: %+v", snapshot)
	}
}

func TestUsageMonitorClearsOldValueAfterRefreshFailure(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	adapter := &fakeAdapter{value: UsageSnapshot{Adapter: adapterCodex, Available: true, FiveHour: &UsageWindow{UsedPercent: 70}}}
	monitor := newUsageMonitor(3 * time.Minute)
	first := monitor.Snapshot(context.Background(), now, adapter)
	if first.FiveHour == nil || adapter.calls != 1 {
		t.Fatalf("initial usage read failed: value=%+v calls=%d", first, adapter.calls)
	}
	adapter.err = errors.New("offline")
	adapter.value = UsageSnapshot{Adapter: adapterCodex, UnavailableReason: "rate_limits_read_failed"}
	failed := monitor.Snapshot(context.Background(), now.Add(3*time.Minute), adapter)
	if failed.Available || failed.FiveHour != nil || failed.UnavailableReason != "rate_limits_read_failed" {
		t.Fatalf("stale quota survived failed refresh: %+v", failed)
	}
	if adapter.calls != 2 {
		t.Fatalf("unexpected read count: %d", adapter.calls)
	}
}

func TestUsageMonitorUsesTimerSizedCacheWithoutBusyReads(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	adapter := &fakeAdapter{value: UsageSnapshot{Adapter: adapterCodex, Available: true}}
	monitor := newUsageMonitor(3 * time.Minute)
	_ = monitor.Snapshot(context.Background(), now, adapter)
	_ = monitor.Snapshot(context.Background(), now.Add(time.Minute), adapter)
	_ = monitor.Snapshot(context.Background(), now.Add(2*time.Minute), adapter)
	if adapter.calls != 1 {
		t.Fatalf("usage monitor polled too often: %d", adapter.calls)
	}
}

func TestUsageMonitorAsyncRefreshDoesNotBlockOrExposeStaleValue(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	release := make(chan struct{})
	adapter := &blockingUsageAdapter{started: started, release: release}
	monitor := newUsageMonitor(3 * time.Minute)
	monitor.value = UsageSnapshot{Adapter: adapterCodex, Available: true, FiveHour: &UsageWindow{UsedPercent: 99}}

	before := time.Now()
	pending := monitor.SnapshotAsync(now, adapter)
	if time.Since(before) > 250*time.Millisecond || pending.Available || pending.FiveHour != nil || pending.UnavailableReason != "usage_refresh_pending" {
		t.Fatalf("async refresh blocked or exposed stale quota: elapsed=%s value=%+v", time.Since(before), pending)
	}
	<-started
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		value := monitor.SnapshotAsync(now.Add(time.Second), adapter)
		if value.Available && value.FiveHour != nil && value.FiveHour.UsedPercent == 25 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("async quota result was not published")
}

func TestUsageMonitorAsyncRefreshKeepsLastValueWithRefreshingMarker(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	release := make(chan struct{})
	adapter := &blockingUsageAdapter{started: started, release: release}
	monitor := newUsageMonitor(time.Minute)
	monitor.cacheKey = adapter.CacheKey()
	monitor.lastAttempt = now.Add(-time.Minute)
	monitor.value = UsageSnapshot{Adapter: adapterCodex, Available: true, FiveHour: &UsageWindow{UsedPercent: 80}}
	pending := monitor.SnapshotAsync(now, adapter)
	if !pending.Available || !pending.Refreshing || pending.FiveHour == nil || pending.FiveHour.UsedPercent != 80 {
		t.Fatalf("last valid quota was not retained during refresh: %+v", pending)
	}
	<-started
	close(release)
}

type blockingUsageAdapter struct {
	started chan struct{}
	release chan struct{}
}

func (*blockingUsageAdapter) Kind() AdapterKind { return adapterCodex }
func (*blockingUsageAdapter) CacheKey() string  { return "codex:blocking" }
func (*blockingUsageAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{Usage: true}
}
func (a *blockingUsageAdapter) Status() AdapterStatus {
	return AdapterStatus{Kind: adapterCodex, Capabilities: a.Capabilities(), Available: true}
}
func (a *blockingUsageAdapter) ReadUsage(context.Context) (UsageSnapshot, error) {
	close(a.started)
	<-a.release
	return UsageSnapshot{Adapter: adapterCodex, Available: true, FiveHour: &UsageWindow{UsedPercent: 25}}, nil
}

func TestQuotaGateWaitsForLatestExhaustedWindowAndRechecksAfterReset(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snapshot := UsageSnapshot{
		Available: true,
		FiveHour:  &UsageWindow{UsedPercent: 100, ResetsAt: now.Add(30 * time.Minute).Unix()},
		Weekly:    &UsageWindow{UsedPercent: 100, ResetsAt: now.Add(2 * time.Hour).Unix()},
	}
	retryAt, wait := quotaWaitUntil(snapshot, now, 3*time.Minute)
	if !wait || !retryAt.Equal(now.Add(2*time.Hour+quotaResetGrace)) {
		t.Fatalf("quota gate did not wait for every exhausted window: wait=%v retry_at=%s", wait, retryAt)
	}
}

func TestQuotaGatePreservesFastRetryWhenQuotaIsAvailable(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	snapshot := UsageSnapshot{
		Available: true,
		FiveHour:  &UsageWindow{UsedPercent: 82, ResetsAt: now.Add(time.Hour).Unix()},
		Weekly:    &UsageWindow{UsedPercent: 45, ResetsAt: now.Add(4 * 24 * time.Hour).Unix()},
	}
	if retryAt, wait := quotaWaitUntil(snapshot, now, 3*time.Minute); wait || !retryAt.IsZero() {
		t.Fatalf("ordinary transient rate limit was incorrectly held: wait=%v retry_at=%s", wait, retryAt)
	}
}

func TestQuotaGateFailsClosedWhenFreshUsageIsUnavailable(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	retryAt, wait := quotaWaitUntil(UsageSnapshot{}, now, 3*time.Minute)
	if !wait || !retryAt.Equal(now.Add(3*time.Minute)) {
		t.Fatalf("missing quota data did not schedule a bounded recheck: wait=%v retry_at=%s", wait, retryAt)
	}
}

func TestWorkAdapterIsExplicitlyUnavailable(t *testing.T) {
	adapter := NewWorkAdapter()
	status := adapter.Status()
	if status.Available || status.Capabilities.Usage || status.Capabilities.Tasks || status.Capabilities.Resume ||
		status.UnavailableReason != "no_supported_work_interface" {
		t.Fatalf("Work adapter advertised unsupported capabilities: %+v", status)
	}
	if _, err := adapter.ReadUsage(context.Background()); !errors.Is(err, errAdapterUnavailable) {
		t.Fatalf("unexpected Work adapter error: %v", err)
	}
}
