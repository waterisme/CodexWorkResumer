package main

import (
	"testing"
	"time"
)

func TestUIStateCatalogCoversRuntimeStates(t *testing.T) {
	want := map[string]bool{"waiting": false, "running": false, "rate_limited": false, "error": false, "paused": false}
	for _, definition := range uiStateCatalog {
		if definition.LabelZH == "" || definition.LabelEN == "" || definition.Color == "" || definition.MeaningZH == "" || definition.BehaviorZH == "" || definition.NotificationZH == "" || definition.NotificationEN == "" {
			t.Fatalf("incomplete state definition: %+v", definition)
		}
		if _, ok := want[definition.Code]; !ok {
			t.Fatalf("unexpected state definition: %s", definition.Code)
		}
		want[definition.Code] = true
	}
	for code, found := range want {
		if !found {
			t.Fatalf("missing state definition: %s", code)
		}
	}
	if len(uiQuotaCatalog) != 3 {
		t.Fatalf("quota guide entries = %d, want 3", len(uiQuotaCatalog))
	}
	for _, definition := range uiQuotaCatalog {
		if definition.Code == "" || definition.Color == "" || definition.MeaningZH == "" || definition.MeaningEN == "" {
			t.Fatalf("incomplete quota definition: %+v", definition)
		}
	}
}

func TestDeriveUIStatePriority(t *testing.T) {
	if state := deriveUIState(ManagementSnapshot{Running: true, MonitoringPaused: true, ActiveRetries: 1}); state.Code != "paused" {
		t.Fatalf("monitoring pause state = %s", state.Code)
	}
	if state := deriveUIState(ManagementSnapshot{Running: true, RateLimitedRetries: 1, ActiveRetries: 1}); state.Code != "rate_limited" {
		t.Fatalf("rate limit state = %s", state.Code)
	}
	if state := deriveUIState(ManagementSnapshot{Running: true, ActiveRetries: 1}); state.Code != "running" {
		t.Fatalf("running state = %s", state.Code)
	}
}

func TestUIStateKeepsProviderQuotaFailureVisibleWithPositivePercentage(t *testing.T) {
	now := time.Now().UTC()
	state := deriveUIState(ManagementSnapshot{
		Running: true, RateLimitedRetries: 1,
		CodexUsage: UsageSnapshot{Available: true, Source: "account/rateLimits/read", FetchedAt: now.Format(time.RFC3339Nano),
			FiveHour: &UsageWindow{UsedPercent: 90, ResetsAt: now.Add(time.Hour).Unix()},
			Weekly:   &UsageWindow{UsedPercent: 14, ResetsAt: now.Add(4 * 24 * time.Hour).Unix()}},
	})
	if state.Code != "rate_limited" {
		t.Fatalf("provider quota failure was hidden by aggregate percentage: %+v", state)
	}
}
