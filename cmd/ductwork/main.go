package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dneil5648/ductwork/pkg/agent"
	"github.com/dneil5648/ductwork/pkg/api"
	"github.com/dneil5648/ductwork/pkg/config"
	"github.com/dneil5648/ductwork/pkg/controlplane"
	"github.com/dneil5648/ductwork/pkg/dependencies"
	"github.com/dneil5648/ductwork/pkg/history"
	"github.com/dneil5648/ductwork/pkg/logging"
	"github.com/dneil5648/ductwork/pkg/orchestrator"
	"github.com/dneil5648/ductwork/pkg/scheduler"
	"github.com/dneil5648/ductwork/pkg/security"
	task "github.com/dneil5648/ductwork/pkg/tasks"
	"github.com/dneil5648/ductwork/pkg/worker"
	"github.com/dneil5648/ductwork/pkg/workerclient"

	"github.com/spf13/cobra"
)

// agentDir is the default .agent/ directory path (relative to cwd)
const agentDir = ".agent"

func main() {
	rootCmd := &cobra.Command{
		Use:   "ductwork",
		Short: "AI Agent Orchestrator",
		Long:  "A Go-based platform for running AI agents on schedules with tasks, skills, and memory.",
	}

	rootCmd.AddCommand(initCmd())
	rootCmd.AddCommand(startCmd())
	rootCmd.AddCommand(runCmd())
	rootCmd.AddCommand(spawnCmd())
	rootCmd.AddCommand(listCmd())
	rootCmd.AddCommand(buildCmd())
	rootCmd.AddCommand(historyCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// initCmd creates the .agent/ directory structure explicitly
func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialize the .agent/ directory structure",
		RunE: func(cmd *cobra.Command, args []string) error {
			absDir, err := filepath.Abs(agentDir)
			if err != nil {
				return err
			}
			return config.Init(absDir)
		},
	}
}

// startCmd boots the system in one of three modes:
//   - standalone (default): scheduler + orchestrator + API, all in-process
//   - control: control plane with task queue, workers poll via HTTP
//   - worker: polls a control plane for tasks, executes locally
func startCmd() *cobra.Command {
	var mode, controlURL, workerID, model string
	var pollInterval time.Duration

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start ductwork in standalone, control plane, or worker mode",
		Long: `Starts ductwork in one of three modes:

  standalone (default):  Everything in one process — scheduler, orchestrator, API server.
                         This is the simple, zero-config mode.

  control:               Control plane mode — API server, scheduler, and task queue.
                         Workers connect via HTTP to pick up and execute tasks.

  worker:                Worker mode — polls a control plane for task assignments,
                         executes them locally, reports results back.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch mode {
			case "standalone", "":
				return startStandalone(model)
			case "control":
				return startControlPlane(model)
			case "worker":
				if controlURL == "" {
					return fmt.Errorf("--control is required in worker mode")
				}
				if workerID == "" {
					hostname, _ := os.Hostname()
					workerID = fmt.Sprintf("worker-%s-%d", hostname, os.Getpid())
				}
				return startWorker(controlURL, workerID, pollInterval)
			default:
				return fmt.Errorf("unknown mode: %q (use standalone, control, or worker)", mode)
			}
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "standalone", "Run mode: standalone, control, or worker")
	cmd.Flags().StringVar(&model, "model", "", "Override default model (e.g. claude-sonnet-4-6, claude-haiku-3)")
	cmd.Flags().StringVar(&controlURL, "control", "", "Control plane URL (required for worker mode)")
	cmd.Flags().StringVar(&workerID, "worker-id", "", "Worker ID (auto-generated if empty)")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 5*time.Second, "Poll interval for worker mode")

	return cmd
}

// startStandalone runs everything in one process (default mode).
// Identical to the original single-node behavior.
func startStandalone(modelOverride string) error {
	cfg, err := config.LoadConfig(agentDir)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// CLI --model flag takes highest priority
	if modelOverride != "" {
		cfg.DefaultModel = modelOverride
	}

	cleanup, err := logging.Setup(cfg.LogsDir, cfg.Debug)
	if err != nil {
		return fmt.Errorf("failed to setup logging: %w", err)
	}
	defer cleanup()

	secCfg, err := security.LoadSecurityConfig(cfg.SecurityFile)
	if err != nil {
		return fmt.Errorf("failed to load security config: %w", err)
	}
	slog.Info("loaded security config", "file", cfg.SecurityFile)

	depCfg, err := dependencies.LoadDependencies(cfg.DependenciesFile)
	if err != nil {
		return fmt.Errorf("failed to load dependencies config: %w", err)
	}
	slog.Info("loaded dependencies config", "runtimes", len(depCfg.Runtimes))

	historyStore, err := history.NewFileStore(cfg.HistoryDir)
	if err != nil {
		return fmt.Errorf("failed to create history store: %w", err)
	}
	slog.Info("loaded history store", "dir", cfg.HistoryDir)

	// Standalone mode: LocalWorker executes tasks in-process
	localWorker := worker.NewLocalWorker(cfg, secCfg)
	orch := orchestrator.NewOrchestrator(cfg, secCfg, depCfg, historyStore, localWorker, 10)
	sched := scheduler.NewScheduler(orch.TaskChan)

	tasks, err := task.LoadTasks(cfg.TasksDir)
	if err != nil {
		slog.Warn("could not load tasks", "error", err)
		tasks = []task.Task{}
	}
	if len(tasks) > 0 {
		if err := sched.LoadTasks(tasks); err != nil {
			return fmt.Errorf("failed to load tasks into scheduler: %w", err)
		}
	}
	slog.Info("loaded tasks", "count", len(tasks), "dir", cfg.TasksDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go orch.Run(ctx)
	go sched.Run(ctx)
	go func() {
		if err := api.Start(cfg, orch, sched, historyStore); err != nil {
			slog.Error("API server error", "error", err)
		}
	}()

	slog.Info("ductwork running", "mode", "standalone", "api_port", cfg.APIPort)
	fmt.Println("Press Ctrl+C to stop")

	return waitForShutdown(cancel)
}

// startControlPlane runs the API, scheduler, and task queue.
// Workers connect via HTTP to poll for tasks and report results.
func startControlPlane(modelOverride string) error {
	cfg, err := config.LoadConfig(agentDir)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// CLI --model flag takes highest priority
	if modelOverride != "" {
		cfg.DefaultModel = modelOverride
	}

	cleanup, err := logging.Setup(cfg.LogsDir, cfg.Debug)
	if err != nil {
		return fmt.Errorf("failed to setup logging: %w", err)
	}
	defer cleanup()

	secCfg, err := security.LoadSecurityConfig(cfg.SecurityFile)
	if err != nil {
		return fmt.Errorf("failed to load security config: %w", err)
	}

	depCfg, err := dependencies.LoadDependencies(cfg.DependenciesFile)
	if err != nil {
		return fmt.Errorf("failed to load dependencies config: %w", err)
	}

	historyStore, err := history.NewFileStore(cfg.HistoryDir)
	if err != nil {
		return fmt.Errorf("failed to create history store: %w", err)
	}

	// Control plane infrastructure
	taskQueue := controlplane.NewTaskQueue()
	resultCollector := controlplane.NewResultCollector()
	workerRegistry := controlplane.NewWorkerRegistry(30 * time.Second)

	// RemoteWorker enqueues tasks → workers poll via HTTP → results flow back
	remoteWorker := controlplane.NewRemoteWorker(taskQueue, resultCollector)

	orch := orchestrator.NewOrchestrator(cfg, secCfg, depCfg, historyStore, remoteWorker, 10)
	sched := scheduler.NewScheduler(orch.TaskChan)

	tasks, err := task.LoadTasks(cfg.TasksDir)
	if err != nil {
		slog.Warn("could not load tasks", "error", err)
		tasks = []task.Task{}
	}
	if len(tasks) > 0 {
		if err := sched.LoadTasks(tasks); err != nil {
			return fmt.Errorf("failed to load tasks into scheduler: %w", err)
		}
	}
	slog.Info("loaded tasks", "count", len(tasks), "dir", cfg.TasksDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go orch.Run(ctx)
	go sched.Run(ctx)
	go func() {
		if err := api.Start(cfg, orch, sched, historyStore,
			api.WithControlPlane(taskQueue, resultCollector, workerRegistry)); err != nil {
			slog.Error("API server error", "error", err)
		}
	}()

	slog.Info("ductwork running", "mode", "control", "api_port", cfg.APIPort)
	fmt.Println("Control plane running. Workers can connect via HTTP.")
	fmt.Println("Press Ctrl+C to stop")

	return waitForShutdown(cancel)
}

// startWorker runs a worker that polls the control plane for tasks.
func startWorker(controlURL, workerID string, pollInterval time.Duration) error {
	// Workers still use .agent/ for logging (if available)
	cfg, err := config.LoadConfig(agentDir)
	if err != nil {
		// Worker mode can run without .agent/ directory
		slog.Info("no .agent/ directory found, using defaults for logging")
		cfg = &config.Config{
			LogsDir: "logs",
			Debug:   false,
		}
	}

	cleanup, err := logging.Setup(cfg.LogsDir, cfg.Debug)
	if err != nil {
		// Non-fatal: just log to stderr
		slog.Warn("failed to setup file logging, using stderr only", "error", err)
		cleanup = func() {}
	}
	defer cleanup()

	client := workerclient.NewClient(workerID, controlURL, pollInterval)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := client.Run(ctx); err != nil {
			slog.Error("worker client error", "error", err)
		}
	}()

	slog.Info("ductwork running", "mode", "worker",
		"worker_id", workerID,
		"control", controlURL,
		"poll_interval", pollInterval)
	fmt.Printf("Worker %s connected to %s\n", workerID, controlURL)
	fmt.Println("Press Ctrl+C to stop")

	return waitForShutdown(cancel)
}

// waitForShutdown blocks until SIGINT or SIGTERM, then calls cancel.
func waitForShutdown(cancel context.CancelFunc) error {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan

	slog.Info("received shutdown signal", "signal", sig)
	cancel()

	return nil
}

// runCmd loads and runs a specific named task (immediate mode)
func runCmd() *cobra.Command {
	var model string

	cmd := &cobra.Command{
		Use:   "run [task-name]",
		Short: "Run a defined task by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskName := args[0]

			cfg, err := config.LoadConfig(agentDir)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// CLI --model flag takes highest priority
			if model != "" {
				cfg.DefaultModel = model
			}

			// Setup logging
			cleanup, err := logging.Setup(cfg.LogsDir, cfg.Debug)
			if err != nil {
				return fmt.Errorf("failed to setup logging: %w", err)
			}
			defer cleanup()

			// Load security config
			secCfg, err := security.LoadSecurityConfig(cfg.SecurityFile)
			if err != nil {
				return fmt.Errorf("failed to load security config: %w", err)
			}

			// Load dependency config
			depCfg, err := dependencies.LoadDependencies(cfg.DependenciesFile)
			if err != nil {
				return fmt.Errorf("failed to load dependencies config: %w", err)
			}

			// Load the specific task
			taskPath := filepath.Join(cfg.TasksDir, taskName+".json")
			t, err := task.LoadTask(taskPath)
			if err != nil {
				return fmt.Errorf("failed to load task %q: %w", taskName, err)
			}

			// Resolve memory_dir relative to .agent/ root
			if t.MemoryDir != "" && !filepath.IsAbs(t.MemoryDir) {
				t.MemoryDir = filepath.Join(cfg.RootDir, t.MemoryDir)
			}

			// Resolve skill paths relative to .agent/ root
			for name, path := range t.Skills {
				if !filepath.IsAbs(path) {
					t.Skills[name] = filepath.Join(cfg.RootDir, path)
				}
			}

			// Create per-task enforcer
			enforcer, err := security.NewEnforcer(secCfg, taskName)
			if err != nil {
				return fmt.Errorf("failed to create enforcer: %w", err)
			}

			a := &agent.Agent{
				SystemPrompt:       cfg.SystemPrompt,
				Model:              cfg.DefaultModel,
				Enforcer:           enforcer,
				TasksDir:           cfg.TasksDir,
				ScriptsDir:         cfg.ScriptsDir,
				SkillsDir:          cfg.SkillsDir,
				DependenciesPrompt: depCfg.ToSystemPrompt(),
				ToolsFile:          cfg.ToolsFile,
			}

			ctx := context.Background()
			slog.Info("running task", "task", t.Name)
			result, err := a.RunTask(ctx, t)
			if err != nil {
				return fmt.Errorf("task failed: %w", err)
			}

			logFields := []any{"task", t.Name}
			if result != nil {
				logFields = append(logFields,
					"input_tokens", result.InputTokens,
					"output_tokens", result.OutputTokens,
					"iterations", result.Iterations)
			}
			slog.Info("task finished", logFields...)
			return nil
		},
	}

	cmd.Flags().StringVar(&model, "model", "", "Override default model (e.g. claude-sonnet-4-6, claude-haiku-3)")
	return cmd
}

// spawnCmd runs an ad-hoc one-off task with a raw prompt
func spawnCmd() *cobra.Command {
	var model string

	cmd := &cobra.Command{
		Use:   "spawn [prompt]",
		Short: "Run an ad-hoc agent with a raw prompt",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := args[0]

			cfg, err := config.LoadConfig(agentDir)
			if err != nil {
				return fmt.Errorf("failed to setup config: %w", err)
			}

			// CLI --model flag takes highest priority
			if model != "" {
				cfg.DefaultModel = model
			}

			// Setup logging
			cleanup, err := logging.Setup(cfg.LogsDir, cfg.Debug)
			if err != nil {
				return fmt.Errorf("failed to setup logging: %w", err)
			}
			defer cleanup()

			// Load security config
			secCfg, err := security.LoadSecurityConfig(cfg.SecurityFile)
			if err != nil {
				return fmt.Errorf("failed to load security config: %w", err)
			}

			// Load dependency config
			depCfg, err := dependencies.LoadDependencies(cfg.DependenciesFile)
			if err != nil {
				return fmt.Errorf("failed to load dependencies config: %w", err)
			}

			// Create enforcer for ad-hoc tasks
			enforcer, err := security.NewEnforcer(secCfg, "adhoc")
			if err != nil {
				return fmt.Errorf("failed to create enforcer: %w", err)
			}

			a := &agent.Agent{
				SystemPrompt:       cfg.SystemPrompt,
				Model:              cfg.DefaultModel,
				Enforcer:           enforcer,
				TasksDir:           cfg.TasksDir,
				ScriptsDir:         cfg.ScriptsDir,
				SkillsDir:          cfg.SkillsDir,
				DependenciesPrompt: depCfg.ToSystemPrompt(),
				ToolsFile:          cfg.ToolsFile,
			}

			ctx := context.Background()
			slog.Info("spawning agent")
			result, err := a.Spawn(ctx, prompt)
			if err != nil {
				return fmt.Errorf("agent failed: %w", err)
			}

			logFields := []any{}
			if result != nil {
				logFields = append(logFields,
					"input_tokens", result.InputTokens,
					"output_tokens", result.OutputTokens,
					"iterations", result.Iterations)
			}
			slog.Info("agent finished", logFields...)
			return nil
		},
	}

	cmd.Flags().StringVar(&model, "model", "", "Override default model (e.g. claude-sonnet-4-6, claude-haiku-3)")
	return cmd
}

// listCmd lists all defined tasks from .agent/tasks/
func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all defined tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig(agentDir)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			tasks, err := task.LoadTasks(cfg.TasksDir)
			if err != nil {
				return fmt.Errorf("failed to load tasks: %w", err)
			}

			if len(tasks) == 0 {
				fmt.Println("No tasks defined. Add task JSON files to .agent/tasks/")
				return nil
			}

			fmt.Printf("%-20s %-12s %-10s %s\n", "NAME", "RUN MODE", "SCHEDULE", "DESCRIPTION")
			fmt.Printf("%-20s %-12s %-10s %s\n", "----", "--------", "--------", "-----------")
			for _, t := range tasks {
				schedule := t.Schedule
				if schedule == "" {
					schedule = "-"
				}
				fmt.Printf("%-20s %-12s %-10s %s\n", t.Name, t.RunMode, schedule, t.Description)
			}

			return nil
		},
	}
}

// buildCmd starts a build-test-iterate pipeline for task creation.
// The build agent writes scripts for deterministic logic, creates the task,
// test-runs it through the real execution path, fixes failures, and saves skills.
func buildCmd() *cobra.Command {
	var model string
	var maxTestRounds int

	cmd := &cobra.Command{
		Use:   "build [description]",
		Short: "Build, test, and optimize a new task definition using an AI agent",
		Long: `Starts an AI agent that builds a task from a natural language description.

The build agent follows a build-test-iterate workflow:
  1. Analyzes the requirement and writes scripts for deterministic logic
  2. Creates a task definition referencing those scripts
  3. Test-runs the task through the real execution pipeline
  4. If the test fails, fixes the task/scripts and retries
  5. Saves reusable skills learned during the process

This produces validated, token-efficient tasks that work on first run.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			description := args[0]

			cfg, err := config.LoadConfig(agentDir)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// CLI --model flag takes highest priority
			if model != "" {
				cfg.DefaultModel = model
			}

			// Setup logging
			cleanup, err := logging.Setup(cfg.LogsDir, cfg.Debug)
			if err != nil {
				return fmt.Errorf("failed to setup logging: %w", err)
			}
			defer cleanup()

			// Load security config for test agent
			secCfg, err := security.LoadSecurityConfig(cfg.SecurityFile)
			if err != nil {
				return fmt.Errorf("failed to load security config: %w", err)
			}

			// Load dependency config
			depCfg, err := dependencies.LoadDependencies(cfg.DependenciesFile)
			if err != nil {
				return fmt.Errorf("failed to load dependencies config: %w", err)
			}

			// TestTaskFn closure — runs a task through the real execution path
			// and captures the output for the build agent to review.
			testTaskFn := func(ctx context.Context, taskName string) (string, error) {
				// Load task fresh from disk (picks up any overwrites)
				taskPath := filepath.Join(cfg.TasksDir, taskName+".json")
				t, err := task.LoadTask(taskPath)
				if err != nil {
					return "", fmt.Errorf("failed to load task %q: %w", taskName, err)
				}

				// Resolve memory_dir relative to .agent/ root
				if t.MemoryDir != "" && !filepath.IsAbs(t.MemoryDir) {
					t.MemoryDir = filepath.Join(cfg.RootDir, t.MemoryDir)
				}

				// Resolve skill paths relative to .agent/ root
				for name, path := range t.Skills {
					if !filepath.IsAbs(path) {
						t.Skills[name] = filepath.Join(cfg.RootDir, path)
					}
				}

				// Create per-task enforcer for the test run
				enforcer, err := security.NewEnforcer(secCfg, taskName)
				if err != nil {
					return "", fmt.Errorf("failed to create enforcer: %w", err)
				}

				// Test agent — no TestTaskFn (prevents recursive test_task calls)
				testAgent := &agent.Agent{
					SystemPrompt:       cfg.SystemPrompt,
					Model:              cfg.DefaultModel,
					Enforcer:           enforcer,
					TasksDir:           cfg.TasksDir,
					ScriptsDir:         cfg.ScriptsDir,
					SkillsDir:          cfg.SkillsDir,
					DependenciesPrompt: depCfg.ToSystemPrompt(),
					ToolsFile:          cfg.ToolsFile,
				}

				// Capture stdout during test run via os.Pipe
				oldStdout := os.Stdout
				r, w, err := os.Pipe()
				if err != nil {
					return "", fmt.Errorf("failed to create pipe: %w", err)
				}
				os.Stdout = w

				// 5-minute timeout for test runs
				testCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()

				result, runErr := testAgent.RunTask(testCtx, t)

				// Restore stdout and read captured output
				w.Close()
				os.Stdout = oldStdout
				capturedBytes := make([]byte, 0)
				buf := make([]byte, 4096)
				for {
					n, err := r.Read(buf)
					if n > 0 {
						capturedBytes = append(capturedBytes, buf[:n]...)
					}
					if err != nil {
						break
					}
					// Cap captured output at 10KB to avoid huge context
					if len(capturedBytes) > 10*1024 {
						capturedBytes = append(capturedBytes[:10*1024], []byte("\n... (output truncated)")...)
						break
					}
				}
				r.Close()
				captured := string(capturedBytes)

				// Format result
				var sb fmt.Stringer = &testResultFormatter{
					taskName: taskName,
					result:   result,
					err:      runErr,
					output:   captured,
				}

				return sb.String(), nil
			}

			// Build agent system prompt
			buildPrompt := fmt.Sprintf(`You are a task builder agent. Your job is to create well-structured, tested task definitions from natural language descriptions.

You have access to these tools:
- bash: Execute shell commands
- read_file: Read files for reference
- write_file: Write content to files
- create_task: Create/update task definitions (use overwrite: "true" to update)
- save_script: Save reusable scripts to .agent/scripts/ (executable, invokable via bash)
- save_skill: Save reusable knowledge/patterns to .agent/skills/
- test_task: Test-run a task through the real execution pipeline

## Workflow

Follow this build-test-iterate process:

1. **Analyze** the user's description to understand what the task needs to do
2. **Write scripts first** for any deterministic logic (API calls, data parsing, file operations).
   Scripts go in .agent/scripts/ and are referenced in the task prompt by full path: %s/<script-name>
   This saves tokens on future runs since the agent just calls the script instead of writing code each time.
3. **Create the task** with create_task, writing a clear prompt that tells the executing agent exactly what to do.
   Reference any scripts by their full path.
4. **Test the task** with test_task to verify it works end-to-end
5. **If the test fails**, read the error output, fix the scripts or task prompt, use create_task with overwrite: "true", and test again
6. **Repeat** up to %d test rounds until the task passes
7. **Save skills** for any reusable patterns or knowledge learned during the process

## Guidelines

- Scripts should have shebangs (#!/bin/bash or #!/usr/bin/env python3)
- Task prompts should be clear and actionable — tell the agent what to do step by step
- If a task needs to run on a schedule, set run_mode to "scheduled" with an appropriate interval
- If it's on-demand, use run_mode "immediate"
- Be concise. Create the task and test it. Don't over-explain.`, cfg.ScriptsDir, maxTestRounds)

			// Build agent gets ALL tools
			enforcer := security.NewStaticEnforcer([]string{
				"bash", "read_file", "write_file", "create_task",
				"save_script", "save_skill", "test_task",
			})

			a := &agent.Agent{
				SystemPrompt: buildPrompt,
				Model:        cfg.DefaultModel,
				Enforcer:     enforcer,
				TasksDir:     cfg.TasksDir,
				ScriptsDir:   cfg.ScriptsDir,
				SkillsDir:    cfg.SkillsDir,
				ToolsFile:    cfg.ToolsFile,
				TestTaskFn:   testTaskFn,
			}

			ctx := context.Background()
			slog.Info("starting task builder", "description", description, "max_test_rounds", maxTestRounds)

			prompt := fmt.Sprintf("Build a task for the following: %s", description)
			result, err := a.Spawn(ctx, prompt)
			if err != nil {
				return fmt.Errorf("build failed: %w", err)
			}

			if result != nil {
				slog.Info("task builder finished",
					"input_tokens", result.InputTokens,
					"output_tokens", result.OutputTokens,
					"iterations", result.Iterations)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&model, "model", "", "Override default model (e.g. claude-sonnet-4-6, claude-haiku-3)")
	cmd.Flags().IntVar(&maxTestRounds, "max-test-rounds", 3, "Maximum number of test-fix-retest iterations")
	return cmd
}

// testResultFormatter formats test run results for the build agent.
type testResultFormatter struct {
	taskName string
	result   *agent.RunResult
	err      error
	output   string
}

func (f *testResultFormatter) String() string {
	var sb strings.Builder

	if f.err != nil {
		sb.WriteString(fmt.Sprintf("## Test Result: FAILED\n\nTask: %s\nError: %s\n", f.taskName, f.err))
	} else {
		sb.WriteString(fmt.Sprintf("## Test Result: PASSED\n\nTask: %s\n", f.taskName))
	}

	if f.result != nil {
		sb.WriteString(fmt.Sprintf("\nToken Usage: %d input, %d output (%d iterations)\n",
			f.result.InputTokens, f.result.OutputTokens, f.result.Iterations))
	}

	if f.output != "" {
		sb.WriteString(fmt.Sprintf("\n### Agent Output\n\n%s\n", f.output))
	}

	return sb.String()
}

// historyCmd shows recent run history
func historyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history [task-name]",
		Short: "Show recent run history",
		Long:  "Shows the most recent task runs with status, duration, and token usage. Optionally filter by task name.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig(agentDir)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			store, err := history.NewFileStore(cfg.HistoryDir)
			if err != nil {
				return fmt.Errorf("failed to open history store: %w", err)
			}

			var records []history.RunRecord
			if len(args) == 1 {
				records, err = store.GetByTask(args[0], 20)
			} else {
				records, err = store.GetRecent(20)
			}
			if err != nil {
				return fmt.Errorf("failed to load history: %w", err)
			}

			if len(records) == 0 {
				fmt.Println("No run history found.")
				return nil
			}

			fmt.Printf("%-36s %-20s %-10s %-12s %-8s %-8s %s\n",
				"RUN ID", "TASK", "STATUS", "DURATION", "IN TOK", "OUT TOK", "ERROR")
			fmt.Printf("%-36s %-20s %-10s %-12s %-8s %-8s %s\n",
				"------", "----", "------", "--------", "------", "-------", "-----")
			for _, r := range records {
				errStr := r.Error
				if len(errStr) > 30 {
					errStr = errStr[:30] + "..."
				}
				fmt.Printf("%-36s %-20s %-10s %-12s %-8d %-8d %s\n",
					r.RunID, r.TaskName, r.Status, r.Duration,
					r.InputTokens, r.OutputTokens, errStr)
			}

			return nil
		},
	}
}
