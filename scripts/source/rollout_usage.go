package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultRolloutUsageMaxAge = 10 * time.Minute
	rolloutUsageTailBytes     = 4 * 1024 * 1024
	rolloutUsageCandidateMax  = 64
)

var errRolloutUsageUnavailable = errors.New("recent Codex rollout quota snapshot is unavailable")

type rolloutRateLimitWindow struct {
	UsedPercent  *float64 `json:"used_percent"`
	WindowMinute *int     `json:"window_minutes"`
	ResetsAt     *int64   `json:"resets_at"`
}

type rolloutRateLimits struct {
	LimitID          string                  `json:"limit_id"`
	Primary          *rolloutRateLimitWindow `json:"primary"`
	Secondary        *rolloutRateLimitWindow `json:"secondary"`
	RateLimitReached string                  `json:"rate_limit_reached_type"`
}

type rolloutUsageEvent struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type       string             `json:"type"`
		RateLimits *rolloutRateLimits `json:"rate_limits"`
	} `json:"payload"`
}

type rolloutUsageCandidate struct {
	path    string
	updated time.Time
}

func readLatestRolloutUsage(roots []sessionRoot, now time.Time, maxAge time.Duration) (UsageSnapshot, error) {
	if maxAge <= 0 {
		maxAge = defaultRolloutUsageMaxAge
	}
	now = now.UTC()
	candidates := recentRolloutUsageCandidates(roots, now, maxAge)
	var latest UsageSnapshot
	var latestObserved time.Time
	for _, candidate := range candidates {
		snapshot, observed, ok := readLatestRolloutUsageFile(candidate.path)
		if !ok || observed.After(now.Add(time.Minute)) || now.Sub(observed) < 0 || now.Sub(observed) > maxAge {
			continue
		}
		if latestObserved.IsZero() || observed.After(latestObserved) {
			latest = snapshot
			latestObserved = observed
		}
	}
	if latestObserved.IsZero() || (latest.FiveHour == nil && latest.Weekly == nil) {
		return UsageSnapshot{Adapter: adapterCodex, Source: "rollout_snapshot", UnavailableReason: "recent_rollout_usage_unavailable"}, errRolloutUsageUnavailable
	}
	latest.Adapter = adapterCodex
	latest.Available = true
	latest.Source = "rollout_snapshot"
	latest.FetchedAt = now.Format(time.RFC3339Nano)
	latest.ObservedAt = latestObserved.Format(time.RFC3339Nano)
	return latest, nil
}

func recentRolloutUsageCandidates(roots []sessionRoot, now time.Time, maxAge time.Duration) []rolloutUsageCandidate {
	cutoff := now.Add(-maxAge)
	seenDirectories := make(map[string]struct{})
	candidates := make([]rolloutUsageCandidate, 0, 16)
	for _, root := range roots {
		for _, base := range root.scanDirectories() {
			for _, directory := range recentDatedDirectories(base, now, maxAge) {
				clean := strings.ToLower(filepath.Clean(directory))
				if _, found := seenDirectories[clean]; found {
					continue
				}
				seenDirectories[clean] = struct{}{}
				entries, err := os.ReadDir(directory)
				if err != nil {
					continue
				}
				for _, entry := range entries {
					name := strings.ToLower(entry.Name())
					if entry.IsDir() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
						continue
					}
					info, err := entry.Info()
					if err != nil || info.ModTime().UTC().Before(cutoff) {
						continue
					}
					candidates = append(candidates, rolloutUsageCandidate{path: filepath.Join(directory, entry.Name()), updated: info.ModTime().UTC()})
				}
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].updated.After(candidates[j].updated) })
	if len(candidates) > rolloutUsageCandidateMax {
		candidates = candidates[:rolloutUsageCandidateMax]
	}
	return candidates
}

func recentDatedDirectories(base string, now time.Time, maxAge time.Duration) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, 6)
	add := func(value time.Time) {
		path := filepath.Join(base, value.Format("2006"), value.Format("01"), value.Format("02"))
		key := strings.ToLower(filepath.Clean(path))
		if _, found := seen[key]; found {
			return
		}
		seen[key] = struct{}{}
		result = append(result, path)
	}
	start := now.Add(-maxAge - 24*time.Hour)
	for cursor := start; !cursor.After(now.Add(24 * time.Hour)); cursor = cursor.Add(24 * time.Hour) {
		add(cursor.UTC())
		add(cursor.In(time.Local))
	}
	return result
}

func readLatestRolloutUsageFile(path string) (UsageSnapshot, time.Time, bool) {
	file, err := os.Open(path)
	if err != nil {
		return UsageSnapshot{}, time.Time{}, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() || info.Size() <= 0 {
		return UsageSnapshot{}, time.Time{}, false
	}
	start := info.Size() - rolloutUsageTailBytes
	if start < 0 {
		start = 0
	}
	data := make([]byte, info.Size()-start)
	read, err := file.ReadAt(data, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return UsageSnapshot{}, time.Time{}, false
	}
	data = data[:read]
	if start > 0 {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			return UsageSnapshot{}, time.Time{}, false
		}
	}
	lines := bytes.Split(data, []byte{'\n'})
	for index := len(lines) - 1; index >= 0; index-- {
		if snapshot, observed, ok := parseRolloutUsageLine(lines[index]); ok {
			return snapshot, observed, true
		}
	}
	return UsageSnapshot{}, time.Time{}, false
}

func parseRolloutUsageLine(line []byte) (UsageSnapshot, time.Time, bool) {
	if !bytes.Contains(line, []byte(`"rate_limits"`)) || !bytes.Contains(line, []byte(`"token_count"`)) {
		return UsageSnapshot{}, time.Time{}, false
	}
	var event rolloutUsageEvent
	if json.Unmarshal(line, &event) != nil || event.Type != "event_msg" || event.Payload.Type != "token_count" || event.Payload.RateLimits == nil {
		return UsageSnapshot{}, time.Time{}, false
	}
	observed, err := time.Parse(time.RFC3339Nano, event.Timestamp)
	if err != nil {
		return UsageSnapshot{}, time.Time{}, false
	}
	limits := event.Payload.RateLimits
	if limits.LimitID != "" && !strings.EqualFold(limits.LimitID, "codex") {
		return UsageSnapshot{}, time.Time{}, false
	}
	snapshot := UsageSnapshot{Adapter: adapterCodex, RateLimitReached: strings.TrimSpace(limits.RateLimitReached)}
	for _, raw := range []*rolloutRateLimitWindow{limits.Primary, limits.Secondary} {
		window := normalizeRolloutUsageWindow(raw)
		if window == nil {
			continue
		}
		switch window.WindowDurationMins {
		case fiveHourWindowMinutes:
			snapshot.FiveHour = window
		case weeklyWindowMinutes:
			snapshot.Weekly = window
		}
	}
	return snapshot, observed.UTC(), snapshot.FiveHour != nil || snapshot.Weekly != nil
}

func normalizeRolloutUsageWindow(window *rolloutRateLimitWindow) *UsageWindow {
	if window == nil || window.UsedPercent == nil || window.WindowMinute == nil || window.ResetsAt == nil ||
		*window.WindowMinute <= 0 || *window.ResetsAt <= 0 || math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) {
		return nil
	}
	used := math.Max(0, math.Min(100, *window.UsedPercent))
	return &UsageWindow{
		UsedPercent: used, RemainingPercent: 100 - used,
		WindowDurationMins: *window.WindowMinute, ResetsAt: *window.ResetsAt,
	}
}
