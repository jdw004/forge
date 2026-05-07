// Package memory provides a single-process demo store with the same lease rules
// as PostgreSQL. It is intentionally ephemeral.
package memory

import (
	"context"
	"encoding/json"
	"forge/internal/model"
	"sort"
	"sync"
	"time"
)

type entry struct{ ID, Body string }
type Store struct {
	mu        sync.Mutex
	workflows map[string]*model.Workflow
	order     []string
	keys      map[string]entry
	nodes     map[string]model.Node
}

func New() *Store {
	return &Store{workflows: map[string]*model.Workflow{}, keys: map[string]entry{}, nodes: map[string]model.Node{}}
}
func clone(w *model.Workflow) model.Workflow {
	copy := *w
	copy.Tasks = append([]model.Task{}, w.Tasks...)
	for i := range copy.Tasks {
		copy.Tasks[i].DependsOn = append([]string{}, copy.Tasks[i].DependsOn...)
	}
	copy.State = model.Aggregate(copy.Tasks)
	return copy
}
func (s *Store) Create(_ context.Context, spec model.Spec, key string) (model.Workflow, bool, error) {
	if err := model.Validate(spec); err != nil {
		return model.Workflow{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := json.Marshal(spec)
	if existing, ok := s.keys[key]; key != "" && ok {
		if existing.Body != string(body) {
			return model.Workflow{}, false, model.ErrConflict
		}
		return clone(s.workflows[existing.ID]), false, nil
	}
	w := &model.Workflow{ID: model.ID(), Name: spec.Name, CreatedAt: time.Now().UTC()}
	for _, t := range spec.Tasks {
		t.DependsOn = append([]string{}, t.DependsOn...)
		w.Tasks = append(w.Tasks, model.Task{TaskSpec: t, WorkflowID: w.ID, State: model.Pending})
	}
	s.workflows[w.ID] = w
	s.order = append(s.order, w.ID)
	if key != "" {
		s.keys[key] = entry{w.ID, string(body)}
	}
	return clone(w), true, nil
}
func (s *Store) Get(_ context.Context, id string) (model.Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workflows[id]
	if !ok {
		return model.Workflow{}, model.ErrNotFound
	}
	return clone(w), nil
}
func (s *Store) List(_ context.Context, limit int) ([]model.Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []model.Workflow{}
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, clone(s.workflows[s.order[i]]))
	}
	return out, nil
}
func (s *Store) Claim(_ context.Context, worker string, lease time.Duration) (*model.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		w := s.workflows[id]
		states := map[string]model.State{}
		for _, t := range w.Tasks {
			states[t.ID] = t.State
		}
		for i := range w.Tasks {
			t := &w.Tasks[i]
			if t.State != model.Pending {
				continue
			}
			ready := true
			for _, dep := range t.DependsOn {
				if states[dep] != model.Succeeded {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			now := time.Now().UTC()
			t.State = model.Running
			t.WorkerID = worker
			t.Claim = model.ID()
			t.Attempt++
			t.StartedAt = &now
			t.FinishedAt = nil
			t.LeaseUntil = now.Add(lease)
			copy := *t
			return &copy, nil
		}
	}
	return nil, nil
}
func (s *Store) current(task model.Task) *model.Task {
	w := s.workflows[task.WorkflowID]
	if w == nil {
		return nil
	}
	for i := range w.Tasks {
		t := &w.Tasks[i]
		if t.ID == task.ID && t.State == model.Running && t.Claim == task.Claim && time.Now().Before(t.LeaseUntil) {
			return t
		}
	}
	return nil
}
func (s *Store) Renew(_ context.Context, task model.Task, lease time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.current(task)
	if t == nil {
		return false, nil
	}
	t.LeaseUntil = time.Now().Add(lease)
	return true, nil
}
func (s *Store) Finish(_ context.Context, task model.Task, state model.State, tokens int, reason string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.current(task)
	if t == nil {
		return false, nil
	}
	now := time.Now().UTC()
	t.State = state
	t.Tokens = tokens
	t.Reason = reason
	t.FinishedAt = &now
	s.propagate(s.workflows[task.WorkflowID])
	return true, nil
}
func (s *Store) propagate(w *model.Workflow) {
	for changed := true; changed; {
		changed = false
		states := map[string]model.State{}
		for _, t := range w.Tasks {
			states[t.ID] = t.State
		}
		for i := range w.Tasks {
			t := &w.Tasks[i]
			if t.State != model.Pending {
				continue
			}
			for _, dep := range t.DependsOn {
				if states[dep].Terminal() && states[dep] != model.Succeeded {
					now := time.Now().UTC()
					t.State = model.Skipped
					t.Reason = "dependency_failed"
					t.FinishedAt = &now
					changed = true
					break
				}
			}
		}
	}
}
func (s *Store) Cancel(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workflows[id]
	if !ok {
		return model.ErrNotFound
	}
	for i := range w.Tasks {
		t := &w.Tasks[i]
		if !t.State.Terminal() {
			now := time.Now().UTC()
			t.State = model.Cancelled
			t.Reason = "user_cancelled"
			t.FinishedAt = &now
		}
	}
	return nil
}
func (s *Store) Reap(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for _, w := range s.workflows {
		for i := range w.Tasks {
			t := &w.Tasks[i]
			if t.State != model.Running || now.Before(t.LeaseUntil) {
				continue
			}
			t.State = model.Pending
			t.Reason = "lease_expired"
			t.Claim = ""
			if t.Attempt >= t.MaxAttempts {
				t.State = model.Failed
				t.FinishedAt = &now
			}
		}
		s.propagate(w)
	}
	return nil
}
func (s *Store) Heartbeat(_ context.Context, n model.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n.LastSeen = time.Now().UTC()
	s.nodes[n.ID] = n
	return nil
}
func (s *Store) Nodes(_ context.Context) ([]model.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []model.Node{}
	for _, n := range s.nodes {
		if time.Since(n.LastSeen) < 10*time.Second {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *Store) Ping(context.Context) error { return nil }
