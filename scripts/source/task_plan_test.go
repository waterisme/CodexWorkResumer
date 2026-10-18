package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	planThreadA = "11111111-1111-4111-8111-111111111111"
	planThreadB = "22222222-2222-4222-8222-222222222222"
	planThreadC = "33333333-3333-4333-8333-333333333333"
)

func TestTaskPlanMissingIsUnconfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-plan.json")
	plan, err := loadTaskPlan(path)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Configured || plan.Revision != 0 || plan.Version != currentTaskPlanVersion {
		t.Fatalf("unexpected default plan: %+v", plan)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("loading a missing plan must not create one: %v", err)
	}
}

func TestTaskPlanSavesSelectionOrderWithoutDispatch(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "task-plan.json")
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.FixedZone("test", 8*3600))
	plan, err := saveTaskPlan(path, 0, []string{strings.ToUpper(planThreadB), planThreadA, planThreadC}, []string{planThreadA, planThreadB}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Configured || plan.Revision != 1 || !plan.UpdatedAt.Equal(now.UTC()) ||
		!reflect.DeepEqual(plan.OrderedThreadIDs, []string{planThreadB, planThreadA, planThreadC}) ||
		!plan.IsSelected(planThreadA) || plan.IsSelected(planThreadC) || plan.Order(planThreadA) != 1 {
		t.Fatalf("unexpected saved plan: %+v", plan)
	}
	loaded, err := loadTaskPlan(path)
	if err != nil || !reflect.DeepEqual(loaded, plan) {
		t.Fatalf("plan failed to round-trip: %+v, %v", loaded, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "commands")); !os.IsNotExist(err) {
		t.Fatalf("saving a selection must not queue Resume: %v", err)
	}
	cleared, err := saveTaskPlan(path, 1, []string{planThreadB, planThreadA}, []string{}, now.Add(time.Minute))
	if err != nil || !cleared.Configured || cleared.Revision != 2 || len(cleared.SelectedThreadIDs) != 0 {
		t.Fatalf("explicit unselection did not persist: %+v, %v", cleared, err)
	}
}

func TestTaskPlanRejectsStaleAndInvalidUpdatesWithoutOverwriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-plan.json")
	now := time.Now().UTC()
	baseline, err := saveTaskPlan(path, 0, []string{planThreadA, planThreadB}, []string{planThreadA}, now)
	if err != nil {
		t.Fatal(err)
	}
	updates := []struct {
		name     string
		expected int64
		ordered  []string
		selected []string
	}{
		{"stale revision", 0, []string{planThreadB}, []string{planThreadB}},
		{"duplicate", 1, []string{planThreadA, strings.ToUpper(planThreadA)}, nil},
		{"selection outside order", 1, []string{planThreadA}, []string{planThreadB}},
		{"not a UUID", 1, []string{"task-123"}, nil},
	}
	for _, update := range updates {
		t.Run(update.name, func(t *testing.T) {
			if _, err := saveTaskPlan(path, update.expected, update.ordered, update.selected, now); err == nil {
				t.Fatal("expected invalid or stale update to fail")
			}
			current, err := loadTaskPlan(path)
			if err != nil || !reflect.DeepEqual(current, baseline) {
				t.Fatalf("failed update changed plan: %+v, %v", current, err)
			}
		})
	}
}

func TestTaskPlanConcurrentCASAllowsOnlyOneUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-plan.json")
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []string{planThreadA, planThreadB} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			_, err := saveTaskPlan(path, 0, []string{id}, []string{id}, time.Now().UTC())
			results <- err
		}(id)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly one CAS winner, got %d", successes)
	}
	plan, err := loadTaskPlan(path)
	if err != nil || plan.Revision != 1 {
		t.Fatalf("concurrent plan is invalid: %+v, %v", plan, err)
	}
}

func TestTaskPlanPayloadRequiresAllFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task-plan.json")
	for _, input := range []string{`{}`, `{"expected_revision":0,"ordered_thread_ids":[]}`, `{"expected_revision":0,"ordered_thread_ids":[],"selected_thread_ids":[],"extra":true}`} {
		if _, err := saveTaskPlanJSON(path, input, time.Now()); err == nil {
			t.Fatalf("incomplete or unknown-field payload was accepted: %s", input)
		}
	}
	payload, err := json.Marshal(map[string]any{
		"expected_revision": 0, "ordered_thread_ids": []string{planThreadA}, "selected_thread_ids": []string{planThreadA},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := saveTaskPlanJSON(path, string(payload), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestApplyTaskPlanRetainsUnresolvedSelectedTaskSafely(t *testing.T) {
	plan := TaskPlan{
		Version: currentTaskPlanVersion, Revision: 1, Configured: true,
		OrderedThreadIDs:  []string{planThreadB, planThreadA},
		SelectedThreadIDs: []string{planThreadB, planThreadA},
	}
	tasks := applyTaskPlan([]ManagedTask{{ThreadID: planThreadA, Label: "Official title", Status: "running"}}, plan)
	if len(tasks) != 2 || !tasks[0].SelectedForMonitoring || tasks[0].QueueOrder != 2 || tasks[0].InfoUnavailable {
		t.Fatalf("resolved task lost its metadata: %+v", tasks)
	}
	missing := tasks[1]
	if missing.ThreadID != planThreadB || !missing.InfoUnavailable || !missing.SelectedForMonitoring ||
		missing.QueueOrder != 1 || missing.CanResume || missing.Status != "unknown" || missing.ProjectPath != "" {
		t.Fatalf("unresolved selected task is not a safe placeholder: %+v", missing)
	}
}
