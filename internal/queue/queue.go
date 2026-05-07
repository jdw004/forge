package queue

import (
	"context"
	"forge/internal/model"
	"time"
)

// Store owns claims. Every completion and renewal must match the current claim
// token, so a worker from an expired lease cannot overwrite a newer attempt.
type Store interface {
	Create(context.Context, model.Spec, string) (model.Workflow, bool, error)
	Get(context.Context, string) (model.Workflow, error)
	List(context.Context, int) ([]model.Workflow, error)
	Claim(context.Context, string, time.Duration) (*model.Task, error)
	Renew(context.Context, model.Task, time.Duration) (bool, error)
	Finish(context.Context, model.Task, model.State, int, string) (bool, error)
	Cancel(context.Context, string) error
	Reap(context.Context) error
	Heartbeat(context.Context, model.Node) error
	Nodes(context.Context) ([]model.Node, error)
	Ping(context.Context) error
}
