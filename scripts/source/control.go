package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const currentControlVersion = 1

type ControlState struct {
	Version          int       `json:"version"`
	Paused           bool      `json:"paused"`
	MonitoringPaused bool      `json:"monitoring_paused"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type ControlCommandAction string

const (
	commandRetryNow            ControlCommandAction = "retry_now"
	commandCancelRetry         ControlCommandAction = "cancel_retry"
	commandRestartRetry        ControlCommandAction = "restart_retry"
	commandResumeTask          ControlCommandAction = "resume_task"
	commandConfirmTaskComplete ControlCommandAction = "confirm_task_complete"
)

type ControlCommand struct {
	Version     int                  `json:"version"`
	Action      ControlCommandAction `json:"action"`
	ThreadID    string               `json:"thread_id"`
	RolloutPath string               `json:"rollout_path,omitempty"`
	Class       FailureClass         `json:"class,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
}

type controlCommandFile struct {
	Path    string
	Command ControlCommand
}

func defaultControlState() ControlState {
	return ControlState{Version: currentControlVersion}
}

func loadOrCreateControlState(path string) (ControlState, error) {
	state := defaultControlState()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeJSONAtomic(path, state); err != nil {
			return ControlState{}, err
		}
		return state, nil
	}
	if err != nil {
		return ControlState{}, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return ControlState{}, fmt.Errorf("parse control state: %w", err)
	}
	if state.Version != currentControlVersion {
		return ControlState{}, fmt.Errorf("control state version must be %d", currentControlVersion)
	}
	return state, nil
}

func saveControlState(path string, paused bool, now time.Time) (ControlState, error) {
	return updateControlState(path, now, func(state *ControlState) {
		state.Paused = paused
	})
}

func saveMonitoringPaused(path string, paused bool, now time.Time) (ControlState, error) {
	return updateControlState(path, now, func(state *ControlState) {
		state.MonitoringPaused = paused
	})
}

func saveControlSettings(path string, autoResumePaused, monitoringPaused bool, now time.Time) (ControlState, error) {
	return updateControlState(path, now, func(state *ControlState) {
		state.Paused = autoResumePaused
		state.MonitoringPaused = monitoringPaused
	})
}

func updateControlState(path string, now time.Time, mutate func(*ControlState)) (ControlState, error) {
	state, err := loadOrCreateControlState(path)
	if err != nil {
		return ControlState{}, err
	}
	if mutate != nil {
		mutate(&state)
	}
	state.Version = currentControlVersion
	state.UpdatedAt = now.UTC()
	if err := writeJSONAtomic(path, state); err != nil {
		return ControlState{}, err
	}
	return state, nil
}

func queueControlCommand(directory string, action ControlCommandAction, threadID string, now time.Time) (ControlCommand, error) {
	return queueControlCommandWithTask(directory, action, threadID, "", classNone, now)
}

func queueControlCommandWithTask(directory string, action ControlCommandAction, threadID, rolloutPath string, class FailureClass, now time.Time) (ControlCommand, error) {
	threadID = strings.ToLower(strings.TrimSpace(threadID))
	command := ControlCommand{
		Version:     currentControlVersion,
		Action:      action,
		ThreadID:    threadID,
		RolloutPath: strings.TrimSpace(rolloutPath),
		Class:       class,
		CreatedAt:   now.UTC(),
	}
	if err := command.validate(); err != nil {
		return ControlCommand{}, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return ControlCommand{}, err
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return ControlCommand{}, err
	}
	name := fmt.Sprintf("%s-%s.json", now.UTC().Format("20060102T150405.000000000Z"), hex.EncodeToString(random))
	if err := writeJSONAtomic(filepath.Join(directory, name), command); err != nil {
		return ControlCommand{}, err
	}
	return command, nil
}

func (c ControlCommand) validate() error {
	if c.Version != currentControlVersion {
		return fmt.Errorf("command version must be %d", currentControlVersion)
	}
	if c.Action != commandRetryNow && c.Action != commandCancelRetry && c.Action != commandRestartRetry && c.Action != commandResumeTask && c.Action != commandConfirmTaskComplete {
		return errors.New("unsupported control command")
	}
	if threadIDFromPath(c.ThreadID+".jsonl") != strings.ToLower(c.ThreadID) {
		return errors.New("invalid thread id")
	}
	if c.CreatedAt.IsZero() {
		return errors.New("command timestamp is required")
	}
	if c.Action == commandResumeTask {
		if strings.TrimSpace(c.RolloutPath) == "" || !filepath.IsAbs(c.RolloutPath) || !strings.EqualFold(threadIDFromPath(c.RolloutPath), c.ThreadID) {
			return errors.New("valid task rollout path is required")
		}
		if !catalogResumeClassAllowed(c.Class) {
			return errors.New("retryable failure class is required")
		}
	}
	return nil
}

func catalogResumeClassAllowed(class FailureClass) bool {
	switch class {
	case classTransient, classRateLimit, classServer, classAuthTransient, classAuthLimited, classEmptyResponse, classUnknown:
		return true
	default:
		return false
	}
}

func loadControlCommandFiles(directory string) ([]controlCommandFile, []string, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	commands := make([]controlCommandFile, 0, len(entries))
	invalid := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return commands, invalid, readErr
		}
		var command ControlCommand
		if json.Unmarshal(data, &command) != nil || command.validate() != nil {
			invalid = append(invalid, path)
			continue
		}
		commands = append(commands, controlCommandFile{Path: path, Command: command})
	}
	return commands, invalid, nil
}
