//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogResumePathAcceptsWindowsExtendedDrivePrefix(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".codex")
	sessions := filepath.Join(home, "sessions", "2026", "10", "01")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	threadID := "019f9d5d-9c82-75b1-b7c0-20a658af0423"
	rollout := filepath.Join(sessions, "rollout-2026-10-01T01-02-03-"+threadID+".jsonl")
	extended := "\\\\?\\" + rollout
	config := defaultConfig()
	config.IncludeDefaultHome = false
	config.IncludeCockpitHomes = false
	config.SessionRoots = []string{home}
	root, err := validateCatalogResumePath(config, threadID, extended)
	if err != nil || !filepath.IsAbs(normalizeCatalogRolloutPath(extended)) || root.CodexHome != home {
		t.Fatalf("extended rollout path was rejected: root=%+v err=%v", root, err)
	}
	if got := normalizeCatalogRolloutPath(extended); got != rollout {
		t.Fatalf("extended path was not normalized: %q", got)
	}
}
