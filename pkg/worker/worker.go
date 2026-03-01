package worker

import (
	"context"

	task "github.com/dneil5648/ductwork/pkg/tasks"
)

// Worker is the abstraction the orchestrator uses to execute a task.
// Implementations:
//   - LocalWorker: in-process execution (default, standalone mode)
//   - RemoteWorker: enqueues for remote workers (control plane mode)
type Worker interface {
	Execute(ctx context.Context, assignment TaskAssignment) TaskResult
}

// TaskAssignment contains everything a worker needs to execute a task.
// The control plane constructs this by pre-loading skills, memory, and
// resolving security rules — so workers don't need .agent/ filesystem access.
type TaskAssignment struct {
	RunID              string    `json:"run_id"`
	Task               task.Task `json:"task"`
	SystemPrompt       string    `json:"system_prompt"`
	Model              string    `json:"model"`
	DependenciesPrompt string    `json:"dependencies_prompt"`
	AllowedTools       []string  `json:"allowed_tools"`
	SkillsContent      string    `json:"skills_content"` // pre-loaded skill text
	MemoryContent      string    `json:"memory_content"` // pre-loaded memory text
}

// TaskResult is what a worker produces after executing a task.
type TaskResult struct {
	RunID        string `json:"run_id"`
	TaskName     string `json:"task_name"`
	Status       string `json:"status"` // "completed" or "failed"
	Error        string `json:"error,omitempty"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Iterations   int    `json:"iterations"`
}
