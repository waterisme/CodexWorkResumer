package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadLatestRolloutUsageReturnsOnlyRecentCodexSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC)
	home := t.TempDir()
	directory := filepath.Join(home, "sessions", "2026", "09", "22")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "rollout-2026-09-22T05-59-00-019fcb64-4cec-7f90-b0a9-94086c070a10.jsonl")
	line := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":72,"window_minutes":300,"resets_at":1800000000},"secondary":{"used_percent":44,"window_minutes":10080,"resets_at":1800500000},"rate_limit_reached_type":""}}}`+"\n", now.Add(-time.Minute).Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	snapshot, err := readLatestRolloutUsage([]sessionRoot{{CodexHome: home, Sessions: filepath.Join(home, "sessions")}}, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Available || snapshot.Source != "rollout_snapshot" || snapshot.FiveHour == nil || snapshot.Weekly == nil {
		t.Fatalf("unexpected rollout quota snapshot: %+v", snapshot)
	}
	if snapshot.FiveHour.RemainingPercent != 28 || snapshot.Weekly.RemainingPercent != 56 || snapshot.ObservedAt == snapshot.FetchedAt {
		t.Fatalf("rollout quota values or timestamps were not preserved: %+v", snapshot)
	}
}

func TestReadLatestRolloutUsageRejectsStaleSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC)
	home := t.TempDir()
	directory := filepath.Join(home, "sessions", "2026", "09", "22")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "rollout-2026-09-22T05-30-00-019fcb64-4cec-7f90-b0a9-94086c070a11.jsonl")
	line := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":90,"window_minutes":300,"resets_at":1800000000}}}}`+"\n", now.Add(-30*time.Minute).Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	if snapshot, err := readLatestRolloutUsage([]sessionRoot{{CodexHome: home, Sessions: filepath.Join(home, "sessions")}}, now, 10*time.Minute); err == nil || snapshot.Available {
		t.Fatalf("stale rollout quota was presented as current: %+v err=%v", snapshot, err)
	}
}

func TestCodexAdapterFallsBackToRecentRolloutAfterOfficialReadFailure(t *testing.T) {
	now := time.Date(2026, 9, 22, 6, 0, 0, 0, time.UTC)
	home := t.TempDir()
	directory := filepath.Join(home, "sessions", "2026", "09", "22")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "rollout-2026-09-22T05-59-00-019fcb64-4cec-7f90-b0a9-94086c070a12.jsonl")
	line := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":36,"window_minutes":300,"resets_at":1800000000}}}}`+"\n", now.Add(-time.Minute).Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	adapter := NewCodexAdapter("ws://127.0.0.1:49621")
	adapter.dial = func(context.Context, string) (appServerCaller, error) { return nil, fmt.Errorf("offline") }
	adapter.sidecarDial = nil
	adapter.rolloutRoots = []sessionRoot{{CodexHome: home, Sessions: filepath.Join(home, "sessions")}}
	adapter.rolloutMaxAge = 10 * time.Minute
	adapter.now = func() time.Time { return now }

	snapshot, err := adapter.ReadUsage(context.Background())
	if err != nil || !snapshot.Available || snapshot.Source != "rollout_snapshot" || snapshot.FiveHour == nil {
		t.Fatalf("recent rollout did not act as bounded fallback: snapshot=%+v err=%v", snapshot, err)
	}
}
