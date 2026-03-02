package worker

import (
	"context"
	"fmt"

	"github.com/dneil5648/ductwork/pkg/agent"
	"github.com/dneil5648/ductwork/pkg/config"
	"github.com/dneil5648/ductwork/pkg/security"
)

// LocalWorker executes tasks in the same process using a real Agent.
// This is the default worker used in standalone mode — same code path
// as the original executeTask(), just behind the Worker interface.
type LocalWorker struct {
	cfg            *config.Config
	securityConfig *security.SecurityConfig
}

// NewLocalWorker creates a worker that executes tasks in-process.
func NewLocalWorker(cfg *config.Config, secCfg *security.SecurityConfig) *LocalWorker {
	return &LocalWorker{cfg: cfg, securityConfig: secCfg}
}

// Execute runs a task locally using a fresh Agent instance.
func (w *LocalWorker) Execute(ctx context.Context, assignment TaskAssignment) TaskResult {
	a := &agent.Agent{
		SystemPrompt:       assignment.SystemPrompt,
		Model:              assignment.Model,
		DependenciesPrompt: assignment.DependenciesPrompt,
		TasksDir:           w.cfg.TasksDir,
		ScriptsDir:         w.cfg.ScriptsDir,
		SkillsDir:          w.cfg.SkillsDir,
		ToolsFile:          w.cfg.ToolsFile,
	}

	// Create per-task Enforcer if security config exists
	if w.securityConfig != nil {
		enforcer, err := security.NewEnforcer(w.securityConfig, assignment.Task.Name)
		if err != nil {
			return TaskResult{
				RunID:    assignment.RunID,
				TaskName: assignment.Task.Name,
				Status:   "failed",
				Error:    fmt.Sprintf("failed to create enforcer: %v", err),
			}
		}
		a.Enforcer = enforcer
	}

	// LocalWorker calls RunTask directly — it has filesystem access
	// for skills and memory loading
	result, err := a.RunTask(ctx, assignment.Task)

	tr := TaskResult{
		RunID:    assignment.RunID,
		TaskName: assignment.Task.Name,
	}

	if err != nil {
		tr.Status = "failed"
		tr.Error = err.Error()
	} else {
		tr.Status = "completed"
	}

	if result != nil {
		tr.InputTokens = result.InputTokens
		tr.OutputTokens = result.OutputTokens
		tr.Iterations = result.Iterations
	}

	return tr
}
