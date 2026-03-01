package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dneil5648/ductwork/pkg/agent"
	"github.com/dneil5648/ductwork/pkg/config"
	"github.com/dneil5648/ductwork/pkg/dependencies"
	"github.com/dneil5648/ductwork/pkg/history"
	"github.com/dneil5648/ductwork/pkg/logging"
	"github.com/dneil5648/ductwork/pkg/security"
	task "github.com/dneil5648/ductwork/pkg/tasks"
	"github.com/dneil5648/ductwork/pkg/worker"
)

// Orchestrator owns the task channel and dispatches tasks to a Worker.
// It is the single consumer of the channel — both the scheduler and
// immediate pushes (CLI, API) write to it.
//
// The Worker interface abstracts whether execution is local (in-process)
// or remote (enqueued for a remote worker to pick up via HTTP).
type Orchestrator struct {
	TaskChan       chan task.Task
	cfg            *config.Config
	securityConfig *security.SecurityConfig
	depPrompt      string        // pre-rendered dependency info for system prompt
	historyStore   history.Store // nil = no history recording
	sem            chan struct{} // concurrency semaphore
	worker         worker.Worker // executes tasks (local or remote)
}

// NewOrchestrator creates an orchestrator with a buffered task channel.
// securityCfg, depCfg, and historyStore can be nil for backward compatibility.
// w is the Worker implementation (LocalWorker for standalone, RemoteWorker for control plane).
func NewOrchestrator(cfg *config.Config, securityCfg *security.SecurityConfig, depCfg *dependencies.DependencyConfig, historyStore history.Store, w worker.Worker, bufferSize int) *Orchestrator {
	depPrompt := ""
	if depCfg != nil {
		depPrompt = depCfg.ToSystemPrompt()
	}

	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 5
	}

	return &Orchestrator{
		TaskChan:       make(chan task.Task, bufferSize),
		cfg:            cfg,
		securityConfig: securityCfg,
		depPrompt:      depPrompt,
		historyStore:   historyStore,
		sem:            make(chan struct{}, maxConcurrent),
		worker:         w,
	}
}

// newAgent creates a fresh Agent configured for a specific task.
// Used by RunImmediate and SpawnAdhoc which always execute locally.
func (o *Orchestrator) newAgent(taskName string) (*agent.Agent, error) {
	a := &agent.Agent{
		SystemPrompt:       o.cfg.SystemPrompt,
		Model:              o.cfg.DefaultModel,
		TasksDir:           o.cfg.TasksDir,
		ScriptsDir:         o.cfg.ScriptsDir,
		DependenciesPrompt: o.depPrompt,
		ToolsFile:          o.cfg.ToolsFile,
	}

	// Create per-task Enforcer if security config exists
	if o.securityConfig != nil {
		enforcer, err := security.NewEnforcer(o.securityConfig, taskName)
		if err != nil {
			return nil, fmt.Errorf("failed to create enforcer for task %q: %w", taskName, err)
		}
		a.Enforcer = enforcer
	}

	return a, nil
}

// Run is the main orchestrator loop. It should be run as a goroutine.
// It reads tasks from the channel and dispatches them to the Worker.
// The semaphore limits concurrent executions to MaxConcurrent.
func (o *Orchestrator) Run(ctx context.Context) {
	slog.Info("orchestrator started", "module", "orchestrator",
		"max_concurrent", cap(o.sem))

	for {
		select {
		case t := <-o.TaskChan:
			o.sem <- struct{}{} // acquire — blocks if at capacity
			go func(t task.Task) {
				defer func() { <-o.sem }() // release
				o.executeTask(t)
			}(t)
		case <-ctx.Done():
			slog.Info("orchestrator shutting down", "module", "orchestrator")
			return
		}
	}
}

// executeTask builds a TaskAssignment, dispatches it through the Worker,
// and records the result in history.
func (o *Orchestrator) executeTask(t task.Task) {
	runID := fmt.Sprintf("%s-%d", t.Name, time.Now().UnixMilli())
	ctx := logging.NewContext(context.Background(), t.Name, runID)
	logger := logging.FromContext(ctx)

	start := time.Now()
	logger.Info("executing task", "module", "orchestrator")

	// Create initial run record
	record := &history.RunRecord{
		RunID:     runID,
		TaskName:  t.Name,
		Status:    history.StatusRunning,
		StartedAt: start,
	}
	o.saveRecord(record)

	// Build the assignment with pre-loaded content
	assignment, err := o.buildAssignment(runID, t)
	if err != nil {
		logger.Error("failed to build assignment", "module", "orchestrator", "error", err)
		o.finalizeRecord(record, start, nil, err, 0)
		return
	}

	// Resolve retry config
	retryCfg := o.resolveRetryConfig(t)

	var lastResult worker.TaskResult
	retriesDone := 0

	for attempt := 0; attempt <= retryCfg.MaxRetries; attempt++ {
		if attempt > 0 {
			retriesDone++
			wait := retryCfg.Backoff(attempt - 1)
			logger.Info("retrying task", "attempt", attempt,
				"max_retries", retryCfg.MaxRetries, "backoff", wait)
			time.Sleep(wait)
		}

		lastResult = o.worker.Execute(ctx, assignment)
		if lastResult.Status == "completed" {
			break // success
		}

		if !isTransientResult(lastResult) {
			logger.Info("non-transient error, not retrying", "error", lastResult.Error)
			break
		}

		logger.Warn("transient error", "attempt", attempt, "error", lastResult.Error)
	}

	// Convert TaskResult to RunResult for history finalization
	var runResult *agent.RunResult
	var runErr error
	if lastResult.Status == "failed" {
		runErr = fmt.Errorf("%s", lastResult.Error)
	}
	runResult = &agent.RunResult{
		InputTokens:  lastResult.InputTokens,
		OutputTokens: lastResult.OutputTokens,
		Iterations:   lastResult.Iterations,
	}

	o.finalizeRecord(record, start, runResult, runErr, retriesDone)

	elapsed := time.Since(start)
	if runErr != nil {
		logger.Error("task failed", "module", "orchestrator",
			"elapsed", elapsed, "retries", retriesDone, "error", runErr)
	} else {
		logger.Info("task completed", "module", "orchestrator",
			"elapsed", elapsed, "retries", retriesDone,
			"input_tokens", lastResult.InputTokens,
			"output_tokens", lastResult.OutputTokens,
			"iterations", lastResult.Iterations)
	}
}

// buildAssignment constructs a TaskAssignment with pre-loaded skills, memory,
// and resolved model/security settings.
func (o *Orchestrator) buildAssignment(runID string, t task.Task) (worker.TaskAssignment, error) {
	// Pre-load skills content
	skillsContent, err := t.LoadSkills()
	if err != nil {
		return worker.TaskAssignment{}, fmt.Errorf("failed to load skills: %w", err)
	}

	// Pre-load memory content
	memoryContent, err := t.LoadMemory()
	if err != nil {
		return worker.TaskAssignment{}, fmt.Errorf("failed to load memory: %w", err)
	}

	// Resolve model
	model := o.cfg.DefaultModel
	if t.Model != "" {
		model = t.Model
	}

	// Resolve allowed tools from security config
	allowedTools := o.resolveAllowedTools(t.Name)

	return worker.TaskAssignment{
		RunID:              runID,
		Task:               t,
		SystemPrompt:       o.cfg.SystemPrompt,
		Model:              model,
		DependenciesPrompt: o.depPrompt,
		AllowedTools:       allowedTools,
		SkillsContent:      skillsContent,
		MemoryContent:      memoryContent,
	}, nil
}

// resolveAllowedTools returns the effective tool list for a task,
// merging global defaults with task-specific overrides.
func (o *Orchestrator) resolveAllowedTools(taskName string) []string {
	if o.securityConfig == nil {
		return nil
	}

	tools := o.securityConfig.DefaultToolPermissions.AllowedTools

	if override, ok := o.securityConfig.TaskOverrides[taskName]; ok {
		if len(override.AllowedTools) > 0 {
			tools = override.AllowedTools
		}
	}

	return tools
}

// RunImmediate runs a defined task directly, bypassing the channel.
// Blocks until the task completes. Used by CLI `ductwork run` and the API.
// Does NOT go through the semaphore or Worker interface (user-initiated, always local).
func (o *Orchestrator) RunImmediate(ctx context.Context, t task.Task) (*agent.RunResult, error) {
	logger := logging.FromContext(ctx)
	start := time.Now()
	logger.Info("running task immediately", "module", "orchestrator", "task", t.Name)

	a, err := o.newAgent(t.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to create agent: %w", err)
	}

	result, err := a.RunTask(ctx, t)

	elapsed := time.Since(start)
	if err != nil {
		logger.Error("task failed", "module", "orchestrator", "elapsed", elapsed, "error", err)
		return result, err
	}

	logger.Info("task completed", "module", "orchestrator", "elapsed", elapsed)
	return result, nil
}

// SpawnAdhoc runs an ad-hoc task with a raw prompt string.
// Blocks until complete. Used by CLI `ductwork spawn` and the API.
func (o *Orchestrator) SpawnAdhoc(ctx context.Context, prompt string) (*agent.RunResult, error) {
	slog.Info("spawning ad-hoc agent", "module", "orchestrator")

	a, err := o.newAgent("adhoc")
	if err != nil {
		return nil, fmt.Errorf("failed to create agent: %w", err)
	}

	return a.Spawn(ctx, prompt)
}

// saveRecord persists a run record if history store is available.
func (o *Orchestrator) saveRecord(record *history.RunRecord) {
	if o.historyStore != nil {
		if err := o.historyStore.Save(record); err != nil {
			slog.Error("failed to save run record", "error", err)
		}
	}
}

// finalizeRecord updates a run record with completion data and persists it.
func (o *Orchestrator) finalizeRecord(record *history.RunRecord, start time.Time, result *agent.RunResult, err error, retries int) {
	now := time.Now()
	record.CompletedAt = &now
	record.Duration = time.Since(start).Round(time.Millisecond).String()
	record.Retries = retries

	if result != nil {
		record.InputTokens = result.InputTokens
		record.OutputTokens = result.OutputTokens
		record.Iterations = result.Iterations
	}

	if err != nil {
		record.Status = history.StatusFailed
		record.Error = err.Error()
	} else {
		record.Status = history.StatusCompleted
	}

	o.saveRecord(record)
}

// resolveRetryConfig merges task-level retry settings with config defaults.
func (o *Orchestrator) resolveRetryConfig(t task.Task) RetryConfig {
	maxRetries := o.cfg.DefaultMaxRetries
	if t.MaxRetries > 0 {
		maxRetries = t.MaxRetries
	}

	backoffStr := o.cfg.DefaultRetryBackoff
	if t.RetryBackoff != "" {
		backoffStr = t.RetryBackoff
	}

	baseBackoff, err := time.ParseDuration(backoffStr)
	if err != nil {
		baseBackoff = 2 * time.Second
	}

	return RetryConfig{
		MaxRetries:  maxRetries,
		BaseBackoff: baseBackoff,
	}
}

// isTransientResult checks if a failed TaskResult looks like a transient error
// worth retrying. Uses string matching since the error crossed a boundary
// (Worker interface or HTTP).
func isTransientResult(r worker.TaskResult) bool {
	if r.Status != "failed" {
		return false
	}
	errMsg := r.Error
	return strings.Contains(errMsg, "429") ||
		strings.Contains(errMsg, "500") ||
		strings.Contains(errMsg, "502") ||
		strings.Contains(errMsg, "503") ||
		strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "timeout") ||
		strings.Contains(errMsg, "EOF")
}
