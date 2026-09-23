package memory

import (
	"context"
	"testing"
	"time"

	"forge/internal/model"
)

func TestReturnedTasksCannotMutateQueue(t *testing.T) {
	ctx := context.Background()
	s := New()
	root := model.TaskSpec{ID: "root", Steps: 1, StepMS: 1, TokensPerStep: 1, TokenLimit: 10, RuntimeMS: 100, MaxAttempts: 2}
	child := root
	child.ID, child.DependsOn = "child", []string{"root"}
	w, _, err := s.Create(ctx, model.Spec{Name: "isolation", Tasks: []model.TaskSpec{root, child}}, "")
	if err != nil {
		t.Fatal(err)
	}
	child.DependsOn[0] = "changed-input"
	w.Tasks[1].DependsOn[0] = "changed-response"
	first, err := s.Claim(ctx, "worker", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim: %v %v", first, err)
	}
	originalStart := *first.StartedAt
	*first.StartedAt = time.Time{}
	if ok, err := s.Finish(ctx, *first, model.Succeeded, 1, ""); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	snapshot, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *snapshot.Tasks[0].StartedAt != originalStart {
		t.Fatal("claim timestamp aliases queue")
	}
	originalFinish := *snapshot.Tasks[0].FinishedAt
	*snapshot.Tasks[0].StartedAt, *snapshot.Tasks[0].FinishedAt = time.Time{}, time.Time{}
	snapshot.Tasks[1].DependsOn[0] = "changed-get"
	listed, err := s.List(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if *listed[0].Tasks[0].StartedAt != originalStart || *listed[0].Tasks[0].FinishedAt != originalFinish {
		t.Fatal("get timestamps alias queue")
	}
	*listed[0].Tasks[0].FinishedAt = time.Time{}
	second, err := s.Claim(ctx, "worker", time.Minute)
	if err != nil || second == nil || second.ID != "child" {
		t.Fatalf("dependency corrupted: %v %v", second, err)
	}
	second.DependsOn[0] = "changed-claim"
	final, err := s.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Tasks[1].DependsOn[0] != "root" || *final.Tasks[0].FinishedAt != originalFinish {
		t.Fatal("claim or list aliases queue")
	}
}

func TestRetryClearsPreviousAttemptMetadata(t *testing.T) {
	ctx := context.Background()
	s := New()
	w, _, err := s.Create(ctx, model.Spec{Name: "retry", Tasks: []model.TaskSpec{{ID: "one", Steps: 1, StepMS: 1, TokensPerStep: 1, TokenLimit: 10, RuntimeMS: 100, MaxAttempts: 2}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Claim(ctx, "old", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim: %v %v", first, err)
	}
	s.mu.Lock()
	s.workflows[w.ID].Tasks[0].LeaseUntil = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if err := s.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := s.Claim(ctx, "new", time.Minute)
	if err != nil || second == nil || second.Attempt != 2 || second.Reason != "" || second.Claim == first.Claim {
		t.Fatalf("retry: %v %v", second, err)
	}
	if ok, err := s.Finish(ctx, *first, model.Succeeded, 1, ""); err != nil || ok {
		t.Fatalf("stale completion: %v %v", ok, err)
	}
}
