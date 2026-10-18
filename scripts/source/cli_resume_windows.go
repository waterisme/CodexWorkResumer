//go:build windows

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const cliResumeStartupGrace = 750 * time.Millisecond

func dispatchCLIResumeFallback(job RetryJob, settings ResumeSettings, prompt string, logger *safeLogger) (DispatchResult, error) {
	executable, err := findCodexExecutable()
	if err != nil {
		return DispatchResult{}, err
	}
	command, err := buildCLIResumeCommand(executable, job, settings, prompt)
	if err != nil {
		return DispatchResult{}, err
	}
	if err := command.Start(); err != nil {
		return DispatchResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case waitErr := <-done:
		if waitErr != nil {
			return DispatchResult{}, waitErr
		}
		if logger != nil {
			logger.Printf("CLI fallback completed thread=%s category=cli_fallback", shortThreadID(job.ThreadID))
		}
	case <-time.After(cliResumeStartupGrace):
		if logger != nil {
			logger.Printf("CLI fallback started thread=%s category=cli_fallback", shortThreadID(job.ThreadID))
		}
	}
	return DispatchResult{Outcome: outcomeDispatched, Action: actionConversationContinue, Reason: "cli_fallback_started"}, nil
}

func buildCLIResumeCommand(executable string, job RetryJob, settings ResumeSettings, prompt string) (*exec.Cmd, error) {
	if !threadIDPattern.MatchString(job.ThreadID + ".jsonl") {
		return nil, errors.New("invalid thread id")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("CLI fallback prompt is empty")
	}
	cwd := filepath.Clean(strings.TrimSpace(settings.CWD))
	if !filepath.IsAbs(cwd) {
		return nil, errors.New("CLI fallback working directory is unavailable")
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return nil, errors.New("CLI fallback working directory is unavailable")
	}
	codexHome := filepath.Clean(strings.TrimSpace(job.CodexHome))
	if !filepath.IsAbs(codexHome) {
		return nil, errors.New("CLI fallback Codex home is unavailable")
	}
	command := exec.Command(executable, "exec", "resume", "--skip-git-repo-check", job.ThreadID, "-")
	command.Dir = cwd
	command.Stdin = strings.NewReader(prompt + "\n")
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.Env = replaceEnvironmentValue(withoutEnvironmentValue(os.Environ(), sharedAppServerEnvironmentName), "CODEX_HOME", codexHome)
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
	return command, nil
}

func withoutEnvironmentValue(environment []string, key string) []string {
	prefix := strings.ToLower(strings.TrimSpace(key)) + "="
	result := make([]string, 0, len(environment))
	for _, item := range environment {
		if strings.HasPrefix(strings.ToLower(item), prefix) {
			continue
		}
		result = append(result, item)
	}
	return result
}
