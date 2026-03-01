package controlplane

import (
	"sync"
	"time"

	"github.com/dneil5648/ductwork/pkg/worker"
)

// PendingTask wraps an assignment with queue metadata.
type PendingTask struct {
	Assignment worker.TaskAssignment
	QueuedAt   time.Time
	AssignedTo string // worker ID, empty if unassigned
	AssignedAt time.Time
}

// QueueStats provides visibility into the queue state.
type QueueStats struct {
	Pending  int `json:"pending"`
	Inflight int `json:"inflight"`
}

// TaskQueue is a thread-safe FIFO queue of task assignments.
// The control plane enqueues tasks; workers dequeue them via HTTP polling.
type TaskQueue struct {
	mu       sync.Mutex
	pending  []PendingTask
	inflight map[string]*PendingTask // runID → assigned task
}

// NewTaskQueue creates an empty task queue.
func NewTaskQueue() *TaskQueue {
	return &TaskQueue{
		inflight: make(map[string]*PendingTask),
	}
}

// Enqueue adds a task assignment to the pending queue.
func (q *TaskQueue) Enqueue(assignment worker.TaskAssignment) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.pending = append(q.pending, PendingTask{
		Assignment: assignment,
		QueuedAt:   time.Now(),
	})
}

// Dequeue removes and returns the next pending task for a worker.
// Returns nil if the queue is empty.
func (q *TaskQueue) Dequeue(workerID string) *worker.TaskAssignment {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.pending) == 0 {
		return nil
	}

	// Pop the first pending task (FIFO)
	pt := q.pending[0]
	q.pending = q.pending[1:]

	// Track as inflight
	pt.AssignedTo = workerID
	pt.AssignedAt = time.Now()
	q.inflight[pt.Assignment.RunID] = &pt

	return &pt.Assignment
}

// Complete removes a task from inflight tracking.
func (q *TaskQueue) Complete(runID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.inflight, runID)
}

// Stats returns current queue statistics.
func (q *TaskQueue) Stats() QueueStats {
	q.mu.Lock()
	defer q.mu.Unlock()

	return QueueStats{
		Pending:  len(q.pending),
		Inflight: len(q.inflight),
	}
}
