//go:build windows

package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCLIResumeCommandUsesStdinAndIsolatesBrokenDesktopRoute(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv(sharedAppServerEnvironmentName, "ws://127.0.0.1:49621")
	t.Setenv("CODEX_HOME", `C:\old-home`)
	job := RetryJob{
		Kind:      jobRecovery,
		ThreadID:  "019fcb64-4cec-7f90-b0a9-94086c070a20",
		CodexHome: home,
		Class:     classServer,
	}
	command, err := buildCLIResumeCommand(`C:\Codex\codex.exe`, job, ResumeSettings{CWD: cwd}, "繼續")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{`C:\Codex\codex.exe`, "exec", "resume", "--skip-git-repo-check", job.ThreadID, "-"}
	if strings.Join(command.Args, "|") != strings.Join(wantArgs, "|") || !strings.EqualFold(command.Dir, filepath.Clean(cwd)) {
		t.Fatalf("unexpected CLI fallback command: args=%v dir=%q", command.Args, command.Dir)
	}
	prompt, err := io.ReadAll(command.Stdin)
	if err != nil || string(prompt) != "繼續\n" {
		t.Fatalf("retry prompt was not passed privately on stdin: %q err=%v", prompt, err)
	}
	hasHome := false
	for _, item := range command.Env {
		lower := strings.ToLower(item)
		if strings.HasPrefix(lower, strings.ToLower(sharedAppServerEnvironmentName)+"=") {
			t.Fatalf("broken shared desktop route leaked into CLI fallback: %s", item)
		}
		if strings.EqualFold(item, "CODEX_HOME="+home) {
			hasHome = true
		}
	}
	if !hasHome {
		t.Fatalf("CLI fallback did not preserve the task Codex home: %v", command.Env)
	}
}

func TestShouldUseCLIResumeFallbackIsBoundedToKnownSafeStates(t *testing.T) {
	job := RetryJob{Kind: jobRecovery, Class: classServer}
	for _, reason := range []string{"shared_app_server_disabled", "codex_not_running", "codex_restart_required", "codex_app_not_ready", "codex_home_not_shared", "codex_thread_owner_unavailable", "codex_background_channel_unavailable"} {
		if !shouldUseCLIResumeFallback(job, DispatchResult{Outcome: outcomeRetryLater, Reason: reason}) {
			t.Fatalf("known pre-dispatch state did not allow CLI fallback: %s", reason)
		}
	}
	for _, result := range []DispatchResult{
		{Outcome: outcomeDispatched, Reason: "shared_app_server_disabled"},
		{Outcome: outcomeRetryLater, Reason: "thread_active"},
		{Outcome: outcomeRetryLater, Reason: "codex_background_dispatch_failed"},
	} {
		if shouldUseCLIResumeFallback(job, result) {
			t.Fatalf("ambiguous or already-dispatched state allowed CLI fallback: %+v", result)
		}
	}
	job.Class = classEmptyResponse
	if shouldUseCLIResumeFallback(job, DispatchResult{Outcome: outcomeRetryLater, Reason: "shared_app_server_disabled"}) {
		t.Fatal("subagent-sensitive empty response used the generic CLI fallback")
	}
}

func TestTryCLIFallbackHandlesSafePrimaryTransportFailure(t *testing.T) {
	dataDir := t.TempDir()
	config := defaultConfig()
	config.CLIResumeFallbackEnabled = true
	configPath := filepath.Join(dataDir, "config.json")
	if err := writeJSONAtomic(configPath, config); err != nil {
		t.Fatal(err)
	}
	called := 0
	runner := &appResumeRunner{
		configPath: configPath,
		cliFallback: func(job RetryJob, settings ResumeSettings, prompt string) (DispatchResult, error) {
			called++
			return DispatchResult{Outcome: outcomeDispatched, Action: actionConversationContinue, Reason: "cli_fallback_started"}, nil
		},
	}
	result, used := runner.tryCLIFallback(
		RetryJob{Kind: jobRecovery, Class: classServer}, ResumeSettings{}, "繼續",
		retryLaterResult("codex_background_channel_unavailable", false),
	)
	if !used || called != 1 || result.Outcome != outcomeDispatched || result.Reason != "cli_fallback_started" {
		t.Fatalf("safe transport failure did not use CLI fallback: used=%v called=%d result=%+v", used, called, result)
	}
}
