package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	currentTaskPlanVersion  = 1
	maxTaskPlanThreads      = 100
	maxTaskPlanPayloadSize  = 32 * 1024
	localSaveTaskPlanAction = "save_task_plan"
)

// TaskPlan stores only stable Codex thread IDs. It is intentionally separate
// from control.json and the retry queue: selecting or reordering a task never
// authorizes an immediate Resume.
type TaskPlan struct {
	Version           int       `json:"version"`
	Revision          int64     `json:"revision"`
	Configured        bool      `json:"configured"`
	OrderedThreadIDs  []string  `json:"ordered_thread_ids"`
	SelectedThreadIDs []string  `json:"selected_thread_ids"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
}

type taskPlanUpdatePayload struct {
	ExpectedRevision  *int64    `json:"expected_revision"`
	OrderedThreadIDs  *[]string `json:"ordered_thread_ids"`
	SelectedThreadIDs *[]string `json:"selected_thread_ids"`
}

func defaultTaskPlan() TaskPlan {
	return TaskPlan{
		Version:           currentTaskPlanVersion,
		OrderedThreadIDs:  []string{},
		SelectedThreadIDs: []string{},
	}
}

func loadTaskPlan(path string) (TaskPlan, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultTaskPlan(), nil
	}
	if err != nil {
		return TaskPlan{}, err
	}
	if len(data) > maxTaskPlanPayloadSize {
		return TaskPlan{}, errors.New("task plan is too large")
	}
	var plan TaskPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return TaskPlan{}, fmt.Errorf("parse task plan: %w", err)
	}
	if !plan.Configured || plan.Revision < 1 || plan.Version != currentTaskPlanVersion {
		return TaskPlan{}, errors.New("invalid task plan version or revision")
	}
	if err := validateTaskPlanIDs(plan.OrderedThreadIDs, plan.SelectedThreadIDs); err != nil {
		return TaskPlan{}, err
	}
	return plan, nil
}

func saveTaskPlan(path string, expectedRevision int64, orderedIDs, selectedIDs []string, now time.Time) (TaskPlan, error) {
	if expectedRevision < 0 {
		return TaskPlan{}, errors.New("invalid task plan revision")
	}
	orderedIDs = normalizedTaskPlanIDs(orderedIDs)
	selectedIDs = normalizedTaskPlanIDs(selectedIDs)
	if err := validateTaskPlanIDs(orderedIDs, selectedIDs); err != nil {
		return TaskPlan{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return TaskPlan{}, err
	}
	var result TaskPlan
	err := withConfigFileLock(path, func() error {
		current, err := loadTaskPlan(path)
		if err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return fmt.Errorf("task plan changed; expected revision %d, current revision %d", expectedRevision, current.Revision)
		}
		if current.Revision == int64(^uint64(0)>>1) {
			return errors.New("task plan revision exhausted")
		}
		result = TaskPlan{
			Version:           currentTaskPlanVersion,
			Revision:          current.Revision + 1,
			Configured:        true,
			OrderedThreadIDs:  orderedIDs,
			SelectedThreadIDs: selectedIDs,
			UpdatedAt:         now.UTC(),
		}
		return writeJSONAtomic(path, result)
	})
	return result, err
}

func saveTaskPlanJSON(path, payload string, now time.Time) (TaskPlan, error) {
	if len(payload) == 0 || len(payload) > maxTaskPlanPayloadSize {
		return TaskPlan{}, errors.New("invalid task plan payload size")
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	var update taskPlanUpdatePayload
	if err := decoder.Decode(&update); err != nil {
		return TaskPlan{}, fmt.Errorf("parse task plan payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return TaskPlan{}, errors.New("task plan payload has trailing data")
	}
	if update.ExpectedRevision == nil || update.OrderedThreadIDs == nil || update.SelectedThreadIDs == nil {
		return TaskPlan{}, errors.New("task plan revision and both ID arrays are required")
	}
	return saveTaskPlan(path, *update.ExpectedRevision, *update.OrderedThreadIDs, *update.SelectedThreadIDs, now)
}

func normalizedTaskPlanIDs(ids []string) []string {
	normalized := make([]string, 0, len(ids))
	for _, id := range ids {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(id)))
	}
	return normalized
}

func validateTaskPlanIDs(orderedIDs, selectedIDs []string) error {
	if len(orderedIDs) > maxTaskPlanThreads || len(selectedIDs) > maxTaskPlanThreads {
		return fmt.Errorf("task plan is limited to %d threads", maxTaskPlanThreads)
	}
	ordered := make(map[string]struct{}, len(orderedIDs))
	for _, id := range orderedIDs {
		if len(id) != 36 || threadIDFromPath(id+".jsonl") != id {
			return errors.New("task plan contains an invalid thread ID")
		}
		if _, duplicate := ordered[id]; duplicate {
			return errors.New("task plan contains duplicate ordered thread IDs")
		}
		ordered[id] = struct{}{}
	}
	selected := make(map[string]struct{}, len(selectedIDs))
	for _, id := range selectedIDs {
		if _, included := ordered[id]; !included {
			return errors.New("selected thread ID is missing from task order")
		}
		if _, duplicate := selected[id]; duplicate {
			return errors.New("task plan contains duplicate selected thread IDs")
		}
		selected[id] = struct{}{}
	}
	return nil
}

func (p TaskPlan) IsSelected(threadID string) bool {
	for _, id := range p.SelectedThreadIDs {
		if id == strings.ToLower(strings.TrimSpace(threadID)) {
			return true
		}
	}
	return false
}

// Order is zero based; IDs absent from the explicit order sort after it.
func (p TaskPlan) Order(threadID string) int {
	threadID = strings.ToLower(strings.TrimSpace(threadID))
	for index, id := range p.OrderedThreadIDs {
		if id == threadID {
			return index
		}
	}
	return maxTaskPlanThreads + 1
}
