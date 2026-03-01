package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/dneil5648/ductwork/pkg/config"
	"github.com/dneil5648/ductwork/pkg/controlplane"
	"github.com/dneil5648/ductwork/pkg/history"
	"github.com/dneil5648/ductwork/pkg/orchestrator"
	"github.com/dneil5648/ductwork/pkg/scheduler"
	task "github.com/dneil5648/ductwork/pkg/tasks"
	"github.com/dneil5648/ductwork/pkg/worker"
)

// API holds references to the orchestrator and scheduler so handlers can
// trigger tasks and inspect state.
type API struct {
	config       *config.Config
	orchestrator *orchestrator.Orchestrator
	scheduler    *scheduler.Scheduler
	historyStore history.Store

	// Control plane (nil in standalone mode)
	taskQueue       *controlplane.TaskQueue
	resultCollector *controlplane.ResultCollector
	workerRegistry  *controlplane.WorkerRegistry
}

// Option configures the API server.
type Option func(*API)

// WithControlPlane enables worker management endpoints on the API.
// When set, the API registers endpoints for worker polling and result reporting.
func WithControlPlane(queue *controlplane.TaskQueue, results *controlplane.ResultCollector, registry *controlplane.WorkerRegistry) Option {
	return func(a *API) {
		a.taskQueue = queue
		a.resultCollector = results
		a.workerRegistry = registry
	}
}

// SpawnRequest is the JSON body for POST /api/spawn
type SpawnRequest struct {
	Prompt string `json:"prompt"`
}

// SchedulerStatus is a single entry in the GET /api/scheduler/status response
type SchedulerStatus struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	NextRun  string `json:"next_run"`
}

// Start creates the HTTP server and begins listening.
// This blocks — run it in a goroutine.
func Start(cfg *config.Config, orch *orchestrator.Orchestrator, sched *scheduler.Scheduler, historyStore history.Store, opts ...Option) error {
	a := &API{
		config:       cfg,
		orchestrator: orch,
		scheduler:    sched,
		historyStore: historyStore,
	}

	for _, opt := range opts {
		opt(a)
	}

	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /api/health", a.handleHealth)

	// Task endpoints
	mux.HandleFunc("GET /api/tasks", a.handleListTasks)
	mux.HandleFunc("GET /api/tasks/", a.handleGetTask)  // /api/tasks/{name}
	mux.HandleFunc("POST /api/tasks/", a.handleRunTask)  // /api/tasks/{name}/run

	// Spawn endpoint
	mux.HandleFunc("POST /api/spawn", a.handleSpawn)

	// Run history endpoints
	mux.HandleFunc("GET /api/runs", a.handleRecentRuns)
	mux.HandleFunc("GET /api/runs/", a.handleRunsByTask) // /api/runs/{task-name}

	// Scheduler endpoints
	mux.HandleFunc("GET /api/scheduler/status", a.handleSchedulerStatus)
	mux.HandleFunc("POST /api/scheduler/add", a.handleSchedulerAdd)

	// Control plane endpoints (only registered when control plane is enabled)
	if a.taskQueue != nil {
		mux.HandleFunc("POST /api/worker/poll", a.handleWorkerPoll)
		mux.HandleFunc("POST /api/worker/result", a.handleWorkerResult)
		mux.HandleFunc("GET /api/workers", a.handleListWorkers)
		slog.Info("control plane endpoints registered", "module", "api")
	}

	addr := fmt.Sprintf(":%d", cfg.APIPort)
	slog.Info("API server listening", "module", "api", "addr", addr)
	return http.ListenAndServe(addr, mux)
}

// ---------- Core endpoints ----------

// GET /api/health
func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{"status": "ok"}

	// Include control plane info if available
	if a.taskQueue != nil {
		stats := a.taskQueue.Stats()
		resp["queue"] = stats
		resp["workers"] = a.workerRegistry.Count()
	}

	writeJSON(w, http.StatusOK, resp)
}

// GET /api/tasks — list all task definitions
func (a *API) handleListTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := task.LoadTasks(a.config.TasksDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

// GET /api/tasks/{name} — get a specific task definition
func (a *API) handleGetTask(w http.ResponseWriter, r *http.Request) {
	// Only handle GET requests that don't end in /run
	name := extractTaskName(r.URL.Path)
	if name == "" || strings.HasSuffix(r.URL.Path, "/run") {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/run") {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST for /run"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing task name"})
		return
	}

	taskPath := filepath.Join(a.config.TasksDir, name+".json")
	t, err := task.LoadTask(taskPath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("task %q not found", name)})
		return
	}

	writeJSON(w, http.StatusOK, t)
}

// POST /api/tasks/{name}/run — trigger a task immediately
func (a *API) handleRunTask(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/run") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected /api/tasks/{name}/run"})
		return
	}

	path := strings.TrimSuffix(r.URL.Path, "/run")
	name := extractTaskName(path)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing task name"})
		return
	}

	taskPath := filepath.Join(a.config.TasksDir, name+".json")
	t, err := task.LoadTask(taskPath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("task %q not found", name)})
		return
	}

	// Resolve relative paths
	if t.MemoryDir != "" && !filepath.IsAbs(t.MemoryDir) {
		t.MemoryDir = filepath.Join(a.config.RootDir, t.MemoryDir)
	}
	for skillName, skillPath := range t.Skills {
		if !filepath.IsAbs(skillPath) {
			t.Skills[skillName] = filepath.Join(a.config.RootDir, skillPath)
		}
	}

	// Push to orchestrator channel (non-blocking via goroutine)
	go func() {
		a.orchestrator.TaskChan <- t
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "accepted",
		"task":   name,
	})
}

// POST /api/spawn — run an ad-hoc task with a raw prompt
func (a *API) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt is required"})
		return
	}

	go func() {
		if _, err := a.orchestrator.SpawnAdhoc(context.Background(), req.Prompt); err != nil {
			slog.Error("spawn failed", "module", "api", "error", err)
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// GET /api/scheduler/status — list all scheduled tasks with next run times
func (a *API) handleSchedulerStatus(w http.ResponseWriter, r *http.Request) {
	snapshot := a.scheduler.Status()

	statuses := make([]SchedulerStatus, len(snapshot))
	for i, st := range snapshot {
		statuses[i] = SchedulerStatus{
			Name:     st.Task.Name,
			Schedule: st.Task.Schedule,
			NextRun:  st.NextRun.Format("2006-01-02T15:04:05Z07:00"),
		}
	}

	writeJSON(w, http.StatusOK, statuses)
}

// POST /api/scheduler/add — add a new task to the scheduler at runtime
func (a *API) handleSchedulerAdd(w http.ResponseWriter, r *http.Request) {
	var t task.Task
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if t.Name == "" || t.Schedule == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and schedule are required"})
		return
	}

	a.scheduler.AddTask(t)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "added",
		"task":   t.Name,
	})
}

// GET /api/runs — recent run history across all tasks
func (a *API) handleRecentRuns(w http.ResponseWriter, r *http.Request) {
	if a.historyStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history store not configured"})
		return
	}
	records, err := a.historyStore.GetRecent(50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, records)
}

// GET /api/runs/{task-name} — run history for a specific task
func (a *API) handleRunsByTask(w http.ResponseWriter, r *http.Request) {
	if a.historyStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history store not configured"})
		return
	}
	taskName := strings.TrimPrefix(r.URL.Path, "/api/runs/")
	taskName = strings.TrimSuffix(taskName, "/")
	if taskName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing task name"})
		return
	}
	records, err := a.historyStore.GetByTask(taskName, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, records)
}

// ---------- Control plane endpoints ----------

// pollRequest is the JSON body for POST /api/worker/poll
type pollRequest struct {
	WorkerID string `json:"worker_id"`
}

// POST /api/worker/poll — worker polls for a task assignment
func (a *API) handleWorkerPoll(w http.ResponseWriter, r *http.Request) {
	var req pollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.WorkerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id is required"})
		return
	}

	// Update worker heartbeat
	a.workerRegistry.Heartbeat(req.WorkerID)

	// Try to dequeue a task
	assignment := a.taskQueue.Dequeue(req.WorkerID)
	if assignment == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "empty"})
		return
	}

	slog.Info("task assigned to worker",
		"module", "api",
		"worker_id", req.WorkerID,
		"task", assignment.Task.Name,
		"run_id", assignment.RunID)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":     "assigned",
		"assignment": assignment,
	})
}

// resultRequest is the JSON body for POST /api/worker/result
type resultRequest struct {
	WorkerID string            `json:"worker_id"`
	Result   worker.TaskResult `json:"result"`
}

// POST /api/worker/result — worker reports execution result
func (a *API) handleWorkerResult(w http.ResponseWriter, r *http.Request) {
	var req resultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.WorkerID == "" || req.Result.RunID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id and result.run_id are required"})
		return
	}

	// Update heartbeat
	a.workerRegistry.Heartbeat(req.WorkerID)

	// Deliver result to the waiting RemoteWorker.Execute() call
	delivered := a.resultCollector.Deliver(req.Result.RunID, req.Result)

	slog.Info("result received from worker",
		"module", "api",
		"worker_id", req.WorkerID,
		"run_id", req.Result.RunID,
		"task", req.Result.TaskName,
		"status", req.Result.Status,
		"delivered", delivered)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":    "received",
		"delivered": delivered,
	})
}

// GET /api/workers — list all registered workers
func (a *API) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	workers := a.workerRegistry.ActiveWorkers()
	stats := a.taskQueue.Stats()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"workers": workers,
		"queue":   stats,
	})
}

// ---------- Helpers ----------

// extractTaskName pulls the task name from a URL path like /api/tasks/{name}
func extractTaskName(path string) string {
	path = strings.TrimPrefix(path, "/api/tasks/")
	path = strings.TrimSuffix(path, "/")
	if path == "" || strings.Contains(path, "/") {
		return ""
	}
	return path
}

// writeJSON is a helper to write a JSON response with a status code.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
