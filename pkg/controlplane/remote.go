package controlplane

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/dneil5648/ductwork/pkg/worker"
)

// RemoteWorker implements worker.Worker by enqueuing tasks into the TaskQueue
// for remote workers to pick up via HTTP. It blocks until a result is delivered
// back through the ResultCollector.
//
// This is the Worker implementation used in control plane mode.
type RemoteWorker struct {
	queue   *TaskQueue
	results *ResultCollector
}

// NewRemoteWorker creates a worker that delegates execution to remote workers.
func NewRemoteWorker(queue *TaskQueue, results *ResultCollector) *RemoteWorker {
	return &RemoteWorker{queue: queue, results: results}
}

// Execute enqueues a task assignment and blocks until a remote worker reports
// the result or the context is cancelled.
func (rw *RemoteWorker) Execute(ctx context.Context, assignment worker.TaskAssignment) worker.TaskResult {
	// Register a result channel before enqueuing (prevents race condition)
	ch := rw.results.Register(assignment.RunID)
	defer rw.results.Unregister(assignment.RunID)

	// Enqueue the assignment for a remote worker to pick up
	rw.queue.Enqueue(assignment)
	slog.Info("task enqueued for remote worker",
		"run_id", assignment.RunID,
		"task", assignment.Task.Name)

	// Block until result arrives or context cancels
	select {
	case result := <-ch:
		rw.queue.Complete(assignment.RunID)
		return result
	case <-ctx.Done():
		rw.queue.Complete(assignment.RunID)
		return worker.TaskResult{
			RunID:    assignment.RunID,
			TaskName: assignment.Task.Name,
			Status:   "failed",
			Error:    fmt.Sprintf("context cancelled: %v", ctx.Err()),
		}
	}
}
