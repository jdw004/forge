// Package model defines the bounded, declarative workflows accepted by Forge.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type State string

const (
	Pending   State = "pending"
	Running   State = "running"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Cancelled State = "cancelled"
	Skipped   State = "skipped"
)

func (s State) Terminal() bool { return s != Pending && s != Running }

type Spec struct {
	Name  string     `json:"name"`
	Tasks []TaskSpec `json:"tasks"`
}

// TaskSpec describes a deterministic simulated agent, never arbitrary executable code.
type TaskSpec struct {
	ID            string   `json:"id"`
	DependsOn     []string `json:"depends_on,omitempty"`
	Steps         int      `json:"steps"`
	StepMS        int      `json:"step_ms"`
	TokensPerStep int      `json:"tokens_per_step"`
	TokenLimit    int      `json:"token_limit"`
	RuntimeMS     int      `json:"runtime_ms"`
	MaxAttempts   int      `json:"max_attempts"`
	Runaway       bool     `json:"runaway,omitempty"`
}

type Task struct {
	TaskSpec
	WorkflowID string     `json:"workflow_id"`
	State      State      `json:"state"`
	WorkerID   string     `json:"worker_id,omitempty"`
	Claim      string     `json:"-"`
	Attempt    int        `json:"attempt"`
	Tokens     int        `json:"tokens"`
	Reason     string     `json:"reason,omitempty"`
	LeaseUntil time.Time  `json:"lease_until,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type Workflow struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     State     `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	Tasks     []Task    `json:"tasks"`
}

type Node struct {
	ID       string    `json:"id"`
	Capacity int       `json:"capacity"`
	Active   int       `json:"active"`
	LastSeen time.Time `json:"last_seen"`
}

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("idempotency key reused with different content")
var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func Validate(s Spec) error {
	if len(s.Name) < 1 || len(s.Name) > 120 {
		return errors.New("name must contain 1–120 bytes")
	}
	if len(s.Tasks) < 1 || len(s.Tasks) > 5000 {
		return errors.New("workflow must contain 1–5000 tasks")
	}
	byID := make(map[string]TaskSpec, len(s.Tasks))
	for _, t := range s.Tasks {
		if !validID.MatchString(t.ID) {
			return fmt.Errorf("invalid task ID %q", t.ID)
		}
		if _, ok := byID[t.ID]; ok {
			return fmt.Errorf("duplicate task %q", t.ID)
		}
		if t.Steps < 1 || t.Steps > 100000 || t.StepMS < 1 || t.StepMS > 1000 || t.TokensPerStep < 1 || t.TokensPerStep > 100000 || t.TokenLimit < 1 || t.TokenLimit > 10000000 || t.RuntimeMS < 10 || t.RuntimeMS > 300000 || t.MaxAttempts < 1 || t.MaxAttempts > 5 {
			return fmt.Errorf("task %q: steps 1–100000, step_ms 1–1000, tokens_per_step 1–100000, token_limit 1–10000000, runtime_ms 10–300000, max_attempts 1–5 required", t.ID)
		}
		if len(t.DependsOn) > 64 {
			return fmt.Errorf("task %q has too many dependencies", t.ID)
		}
		byID[t.ID] = t
	}
	visited := make(map[string]int, len(s.Tasks))
	var visit func(string) error
	visit = func(id string) error {
		if visited[id] == 1 {
			return errors.New("dependency cycle detected")
		}
		if visited[id] == 2 {
			return nil
		}
		t, ok := byID[id]
		if !ok {
			return fmt.Errorf("unknown dependency %q", id)
		}
		visited[id] = 1
		for _, dep := range t.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visited[id] = 2
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func Aggregate(tasks []Task) State {
	result := Succeeded
	for _, t := range tasks {
		if t.State == Running || t.State == Pending {
			return Running
		}
		if t.State == Failed || t.State == Skipped {
			result = Failed
		}
		if t.State == Cancelled && result != Failed {
			result = Cancelled
		}
	}
	return result
}
