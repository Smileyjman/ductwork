package controlplane

import (
	"sync"

	"github.com/dneil5648/ductwork/pkg/worker"
)

// ResultCollector routes results from remote workers back to the waiting
// RemoteWorker.Execute() calls via per-RunID channels.
type ResultCollector struct {
	mu       sync.Mutex
	channels map[string]chan worker.TaskResult
}

// NewResultCollector creates a new result collector.
func NewResultCollector() *ResultCollector {
	return &ResultCollector{
		channels: make(map[string]chan worker.TaskResult),
	}
}

// Register creates a buffered result channel for a run ID.
// The caller should defer Unregister after receiving the result.
func (rc *ResultCollector) Register(runID string) chan worker.TaskResult {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	ch := make(chan worker.TaskResult, 1)
	rc.channels[runID] = ch
	return ch
}

// Deliver sends a result to the waiting channel for the given run ID.
// Returns false if no waiter is registered (e.g., timed out).
func (rc *ResultCollector) Deliver(runID string, result worker.TaskResult) bool {
	rc.mu.Lock()
	ch, ok := rc.channels[runID]
	rc.mu.Unlock()

	if !ok {
		return false
	}

	// Non-blocking send (channel is buffered with capacity 1)
	select {
	case ch <- result:
		return true
	default:
		return false
	}
}

// Unregister cleans up a result channel.
func (rc *ResultCollector) Unregister(runID string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	delete(rc.channels, runID)
}
