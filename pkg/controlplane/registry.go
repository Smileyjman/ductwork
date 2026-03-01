package controlplane

import (
	"sync"
	"time"
)

// WorkerInfo tracks a registered remote worker.
type WorkerInfo struct {
	ID       string    `json:"id"`
	LastSeen time.Time `json:"last_seen"`
	Active   bool      `json:"active"`
}

// WorkerRegistry tracks connected workers via their poll heartbeats.
// Workers register implicitly on their first poll — no explicit registration needed.
type WorkerRegistry struct {
	mu      sync.Mutex
	workers map[string]*WorkerInfo
	timeout time.Duration // how long before a worker is considered dead
}

// NewWorkerRegistry creates a registry with the given worker timeout.
func NewWorkerRegistry(timeout time.Duration) *WorkerRegistry {
	return &WorkerRegistry{
		workers: make(map[string]*WorkerInfo),
		timeout: timeout,
	}
}

// Heartbeat updates or registers a worker. Called on each poll.
func (r *WorkerRegistry) Heartbeat(workerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.workers[workerID]
	if !ok {
		w = &WorkerInfo{ID: workerID}
		r.workers[workerID] = w
	}
	w.LastSeen = time.Now()
	w.Active = true
}

// ActiveWorkers returns the list of all workers with current active status.
func (r *WorkerRegistry) ActiveWorkers() []WorkerInfo {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	result := make([]WorkerInfo, 0, len(r.workers))
	for _, w := range r.workers {
		info := *w
		info.Active = now.Sub(w.LastSeen) < r.timeout
		result = append(result, info)
	}
	return result
}

// Count returns the number of currently active workers.
func (r *WorkerRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	count := 0
	for _, w := range r.workers {
		if now.Sub(w.LastSeen) < r.timeout {
			count++
		}
	}
	return count
}
