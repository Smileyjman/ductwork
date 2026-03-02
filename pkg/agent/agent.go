package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dneil5648/ductwork/pkg/logging"
	"github.com/dneil5648/ductwork/pkg/security"
	"github.com/dneil5648/ductwork/pkg/session"
	"github.com/dneil5648/ductwork/pkg/taskbuilder"
	task "github.com/dneil5648/ductwork/pkg/tasks"
	"github.com/anthropics/anthropic-sdk-go"
)

//go:embed tools.json
var defaultToolsJSON []byte

// Tool types — these mirror the structure in tools.json

type Property struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required"`
}

type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema InputSchema `json:"input_schema"`
}

// Agent is the core runtime that talks to the Anthropic API and executes tools

type Agent struct {
	SystemPrompt       string
	Model              string
	Enforcer           *security.Enforcer // nil = no restrictions (backward compat)
	TasksDir           string             // path to .agent/tasks/ for create_task tool
	ScriptsDir         string             // path to .agent/scripts/ for save_script tool
	SkillsDir          string             // path to .agent/skills/ for save_skill tool
	DependenciesPrompt string             // dependency info injected into system prompt
	ToolsFile          string             // path to .agent/tools.json (empty = use embedded default)
	TestTaskFn         func(ctx context.Context, taskName string) (string, error) // closure for test_task tool
	SessionStore       *session.Store     // nil = no checkpointing (backward compat)
	RunID              string             // checkpoint key — set by caller
	TaskName           string             // stored in checkpoint for resume
	MaxIterations              int  // 0 = unlimited (default); >0 = hard cap on loop iterations
	ContextCompactionThreshold int  // 0=default(100k), -1=disabled, >0=custom token threshold
}

// RunResult holds the outcome of an agent execution, including token usage.
type RunResult struct {
	InputTokens  int64
	OutputTokens int64
	Iterations   int
	Compactions  int // number of context compactions performed
}

// Spawn runs the agent loop with a raw task string.
// Use this for ad-hoc, one-off tasks.
func (a *Agent) Spawn(ctx context.Context, prompt string) (*RunResult, error) {
	runID := fmt.Sprintf("adhoc-%d", time.Now().UnixMilli())
	ctx = logging.NewContext(ctx, "adhoc", runID)
	return a.runLoop(ctx, a.SystemPrompt, prompt)
}

// RunTask runs the agent loop for a defined, repeatable task.
// It loads skill files into the system prompt and memory from previous runs
// into the user message, then delegates to the core loop.
func (a *Agent) RunTask(ctx context.Context, t task.Task) (*RunResult, error) {
	logger := logging.FromContext(ctx)

	// Use task-specific model if provided, otherwise fall back to agent default
	model := a.Model
	if t.Model != "" {
		model = t.Model
	}

	// Compose system prompt: base prompt + skills
	systemPrompt := a.SystemPrompt
	skills, err := t.LoadSkills()
	if err != nil {
		return nil, fmt.Errorf("failed to load skills: %w", err)
	}
	if skills != "" {
		systemPrompt += "\n" + skills
		logger.Debug("loaded skills into prompt")
	}

	// Compose user message: memory context + task prompt
	userMessage := t.Prompt
	memory, err := t.LoadMemory()
	if err != nil {
		return nil, fmt.Errorf("failed to load memory: %w", err)
	}
	if memory != "" {
		userMessage = memory + "\n---\n\n" + t.Prompt
		logger.Debug("loaded memory from previous runs")
	}

	// Tell the agent where its memory dir is so it can write to it
	if t.MemoryDir != "" {
		systemPrompt += fmt.Sprintf("\n\nYour memory directory is: %s\nYou can write files there to persist information for future runs of this task.", t.MemoryDir)
	}

	// Temporarily override model if task specifies one
	originalModel := a.Model
	a.Model = model
	defer func() { a.Model = originalModel }()

	return a.runLoop(ctx, systemPrompt, userMessage)
}

// RunTaskWithPreloaded is like RunTask but accepts pre-loaded skills and memory
// content instead of reading from the filesystem. Used by remote workers that
// don't have access to the .agent/ directory.
func (a *Agent) RunTaskWithPreloaded(ctx context.Context, t task.Task, skillsContent, memoryContent string) (*RunResult, error) {
	model := a.Model
	if t.Model != "" {
		model = t.Model
	}

	systemPrompt := a.SystemPrompt
	if skillsContent != "" {
		systemPrompt += "\n" + skillsContent
	}

	userMessage := t.Prompt
	if memoryContent != "" {
		userMessage = memoryContent + "\n---\n\n" + t.Prompt
	}

	if t.MemoryDir != "" {
		systemPrompt += fmt.Sprintf("\n\nYour memory directory is: %s\nYou can write files there to persist information for future runs of this task.", t.MemoryDir)
	}

	originalModel := a.Model
	a.Model = model
	defer func() { a.Model = originalModel }()

	return a.runLoop(ctx, systemPrompt, userMessage)
}

// runLoop is the core agent loop. It sends the task to Claude, executes any
// tool calls, feeds results back, and repeats until Claude responds with just text.
func (a *Agent) runLoop(ctx context.Context, systemPrompt string, userMessage string) (*RunResult, error) {
	logger := logging.FromContext(ctx)

	// Inject dependency info into system prompt if available
	if a.DependenciesPrompt != "" {
		systemPrompt += "\n" + a.DependenciesPrompt
	}

	// Inject available scripts into system prompt
	if a.ScriptsDir != "" {
		scripts := loadScriptsList(a.ScriptsDir)
		if scripts != "" {
			systemPrompt += "\n" + scripts
		}
	}

	// Load tool definitions — prefer ToolsFile if set, fall back to embedded default
	toolsData := defaultToolsJSON
	if a.ToolsFile != "" {
		fileData, err := os.ReadFile(a.ToolsFile)
		if err != nil {
			logger.Warn("failed to read tools file, using embedded default", "path", a.ToolsFile, "error", err)
		} else {
			toolsData = fileData
		}
	}

	var tools []Tool
	if err := json.Unmarshal(toolsData, &tools); err != nil {
		return nil, fmt.Errorf("failed to load tools: %w", err)
	}

	// Convert our tool definitions to SDK tool params
	sdkTools := make([]anthropic.ToolUnionParam, len(tools))
	for i, t := range tools {
		sdkTools[i] = anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: t.InputSchema.Properties,
					Required:   t.InputSchema.Required,
				},
			},
		}
	}

	logger.Info("starting agent loop", "model", a.Model, "tools", len(tools))

	// Initialize the Anthropic client (reads ANTHROPIC_API_KEY from env)
	client := anthropic.NewClient()

	// Start the conversation with the user's task
	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(userMessage)),
	}

	// Token usage accumulators
	var totalInputTokens, totalOutputTokens int64

	// Agent loop — runs until the model stops calling tools
	iteration := 0

	// Restore from checkpoint if available (session persistence)
	if a.SessionStore != nil && a.RunID != "" {
		if cp, err := a.SessionStore.Load(a.RunID); err == nil {
			messages = cp.Messages
			totalInputTokens = cp.InputTokens
			totalOutputTokens = cp.OutputTokens
			iteration = cp.Iteration
			logger.Info("resuming from checkpoint",
				"run_id", a.RunID,
				"iteration", iteration,
				"messages", len(messages),
				"tokens_used", totalInputTokens+totalOutputTokens)
		}
		// err just means no checkpoint exists — start fresh
	}

	// Context compaction setup
	compactionThreshold := resolveCompactionThreshold(a.ContextCompactionThreshold)
	compactions := 0

	consecutiveErrors := 0
	const maxConsecutiveErrors = 3

	for {
		iteration++

		// Hard iteration cap — prevents runaway loops
		if a.MaxIterations > 0 && iteration > a.MaxIterations {
			logger.Warn("max iterations reached", "max", a.MaxIterations)
			// Save checkpoint before bailing so the run can be resumed
			if a.SessionStore != nil && a.RunID != "" {
				cp := &session.Checkpoint{
					RunID:        a.RunID,
					TaskName:     a.TaskName,
					Iteration:    iteration - 1,
					Messages:     messages,
					SystemPrompt: systemPrompt,
					InputTokens:  totalInputTokens,
					OutputTokens: totalOutputTokens,
					CreatedAt:    time.Now(),
				}
				_ = a.SessionStore.Save(cp)
			}
			return &RunResult{
				InputTokens:  totalInputTokens,
				OutputTokens: totalOutputTokens,
				Iterations:   iteration - 1,
				Compactions:  compactions,
			}, fmt.Errorf("agent reached max iterations (%d) without completing", a.MaxIterations)
		}

		logger.Debug("API call", "iteration", iteration)

		// Call the Anthropic messages API
		response, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(a.Model),
			MaxTokens: 4096,
			System: []anthropic.TextBlockParam{
				{Text: systemPrompt},
			},
			Messages: messages,
			Tools:    sdkTools,
		})
		if err != nil {
			return nil, fmt.Errorf("API error: %w", err)
		}

		// Accumulate token usage
		totalInputTokens += response.Usage.InputTokens
		totalOutputTokens += response.Usage.OutputTokens
		logger.Debug("token usage", "iteration", iteration,
			"input_tokens", response.Usage.InputTokens,
			"output_tokens", response.Usage.OutputTokens)

		// Context compaction: if input tokens exceed threshold, summarize old messages
		if compactionThreshold > 0 && response.Usage.InputTokens > int64(compactionThreshold) {
			oldLen := len(messages)
			compacted, sumIn, sumOut, compErr := compactContext(ctx, client, a.Model, messages, userMessage)
			if compErr != nil {
				logger.Warn("context compaction failed, continuing with full context", "error", compErr)
			} else if len(compacted) < oldLen {
				messages = compacted
				totalInputTokens += sumIn
				totalOutputTokens += sumOut
				compactions++
				logger.Info("context compacted",
					"old_messages", oldLen,
					"new_messages", len(messages),
					"input_tokens_before", response.Usage.InputTokens)
			}
		}

		// Walk through the response content blocks.
		// Each block is either text (Claude talking) or tool_use (Claude wants to run a tool).
		var assistantBlocks []anthropic.ContentBlockParamUnion
		var toolCalls []anthropic.ToolUseBlock

		for _, block := range response.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				fmt.Println(v.Text) // user-facing output stays on stdout
				logger.Debug("assistant text", "text", truncate(v.Text, 200))
				assistantBlocks = append(assistantBlocks, anthropic.NewTextBlock(v.Text))
			case anthropic.ToolUseBlock:
				logger.Info("tool call", "tool", v.Name)
				assistantBlocks = append(assistantBlocks, anthropic.NewToolUseBlock(v.ID, v.Input, v.Name))
				toolCalls = append(toolCalls, v)
			}
		}

		// Append the assistant's full response to the conversation history
		messages = append(messages, anthropic.NewAssistantMessage(assistantBlocks...))

		// If there were no tool calls, the agent is done
		if len(toolCalls) == 0 {
			// Clean up checkpoint on successful completion
			if a.SessionStore != nil && a.RunID != "" {
				if err := a.SessionStore.Delete(a.RunID); err != nil {
					logger.Warn("failed to delete checkpoint", "error", err)
				}
			}

			result := &RunResult{
				InputTokens:  totalInputTokens,
				OutputTokens: totalOutputTokens,
				Iterations:   iteration,
				Compactions:  compactions,
			}
			logger.Info("agent loop complete", "iterations", iteration,
				"total_input_tokens", totalInputTokens,
				"total_output_tokens", totalOutputTokens)
			return result, nil
		}

		// Execute each tool call and build tool_result blocks
		var toolResults []anthropic.ContentBlockParamUnion
		iterationHadError := false

		for _, call := range toolCalls {
			// Parse the tool input JSON
			var input map[string]interface{}
			if err := json.Unmarshal(call.Input, &input); err != nil {
				toolResults = append(toolResults, anthropic.NewToolResultBlock(
					call.ID, fmt.Sprintf("error parsing input: %v", err), true,
				))
				iterationHadError = true
				continue
			}

			// Execute the tool
			result, err := a.executeTool(ctx, call.Name, input)
			if err != nil {
				logger.Error("tool failed", "tool", call.Name, "error", err)
				toolResults = append(toolResults, anthropic.NewToolResultBlock(
					call.ID, fmt.Sprintf("error: %v", err), true,
				))
				iterationHadError = true
			} else {
				logger.Debug("tool result", "tool", call.Name, "result", truncate(result, 100))
				toolResults = append(toolResults, anthropic.NewToolResultBlock(
					call.ID, result, false,
				))
			}
		}

		// Track consecutive error iterations for early bail-out
		if iterationHadError {
			consecutiveErrors++
			if consecutiveErrors >= maxConsecutiveErrors {
				logger.Warn("too many consecutive tool errors, stopping agent",
					"consecutive_errors", consecutiveErrors)
				// Append tool results so conversation is consistent
				messages = append(messages, anthropic.NewUserMessage(toolResults...))
				// Add a nudge message so the model knows it must wrap up
				messages = append(messages, anthropic.NewUserMessage(
					anthropic.NewTextBlock("SYSTEM: You have hit the error budget — too many consecutive tool errors. Summarize what you accomplished and what failed, then stop. Do not call any more tools.")))
				// One final API call to get a wrap-up response
				wrapResp, wrapErr := client.Messages.New(ctx, anthropic.MessageNewParams{
					Model:     anthropic.Model(a.Model),
					MaxTokens: 2048,
					System: []anthropic.TextBlockParam{
						{Text: systemPrompt},
					},
					Messages: messages,
					Tools:    sdkTools,
				})
				if wrapErr == nil {
					totalInputTokens += wrapResp.Usage.InputTokens
					totalOutputTokens += wrapResp.Usage.OutputTokens
					for _, block := range wrapResp.Content {
						if v, ok := block.AsAny().(anthropic.TextBlock); ok {
							fmt.Println(v.Text)
						}
					}
				}
				return &RunResult{
					InputTokens:  totalInputTokens,
					OutputTokens: totalOutputTokens,
					Iterations:   iteration,
					Compactions:  compactions,
				}, fmt.Errorf("agent stopped after %d consecutive iterations with tool errors", consecutiveErrors)
			}
		} else {
			consecutiveErrors = 0 // reset on any successful iteration
		}

		// Tool results go back as a "user" message (that's how the API expects it)
		messages = append(messages, anthropic.NewUserMessage(toolResults...))

		// Save checkpoint after each iteration (non-fatal on failure)
		if a.SessionStore != nil && a.RunID != "" {
			cp := &session.Checkpoint{
				RunID:        a.RunID,
				TaskName:     a.TaskName,
				Iteration:    iteration,
				Messages:     messages,
				SystemPrompt: systemPrompt,
				InputTokens:  totalInputTokens,
				OutputTokens: totalOutputTokens,
				CreatedAt:    time.Now(),
			}
			if err := a.SessionStore.Save(cp); err != nil {
				logger.Warn("failed to save checkpoint", "iteration", iteration, "error", err)
			} else {
				logger.Debug("checkpoint saved", "iteration", iteration)
			}
		}
	}
}

// executeTool dispatches a tool call to the right function based on name.
// Security checks are enforced before each tool execution.
func (a *Agent) executeTool(ctx context.Context, name string, input map[string]interface{}) (string, error) {
	// Security gate: check tool permission
	if a.Enforcer != nil {
		if err := a.Enforcer.CheckTool(name); err != nil {
			return "", err
		}
	}

	switch name {
	case "bash":
		cmd, ok := input["command"].(string)
		if !ok {
			return "", fmt.Errorf("bash: missing or invalid 'command' field")
		}
		// Security gate: check bash command
		if a.Enforcer != nil {
			if err := a.Enforcer.CheckBashCommand(cmd); err != nil {
				return "", err
			}
		}
		return executeBash(cmd)

	case "read_file":
		path, ok := input["path"].(string)
		if !ok {
			return "", fmt.Errorf("read_file: missing or invalid 'path' field")
		}
		// Security gate: check read path
		if a.Enforcer != nil {
			if err := a.Enforcer.CheckPath(path, "read"); err != nil {
				return "", err
			}
		}
		return readFile(path)

	case "write_file":
		path, ok := input["path"].(string)
		if !ok {
			return "", fmt.Errorf("write_file: missing or invalid 'path' field")
		}
		// Security gate: check write path
		if a.Enforcer != nil {
			if err := a.Enforcer.CheckPath(path, "write"); err != nil {
				return "", err
			}
		}
		content, ok := input["content"].(string)
		if !ok {
			return "", fmt.Errorf("write_file: missing or invalid 'content' field")
		}
		err := writeFile(path, content)
		if err != nil {
			return "", err
		}
		return "file written successfully", nil

	case "create_task":
		return a.executeCreateTask(input)

	case "save_script":
		return a.executeSaveScript(input)

	case "save_skill":
		return a.executeSaveSkill(input)

	case "test_task":
		return a.executeTestTask(ctx, input)

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// executeCreateTask delegates to the taskbuilder package to validate and create
// a new task definition file.
func (a *Agent) executeCreateTask(input map[string]interface{}) (string, error) {
	if a.TasksDir == "" {
		return "", fmt.Errorf("create_task: tasks directory not configured")
	}

	t, path, err := taskbuilder.ValidateAndCreate(input, a.TasksDir)
	if err != nil {
		return "", fmt.Errorf("create_task: %w", err)
	}

	return fmt.Sprintf("Task %q created at %s (run_mode: %s)", t.Name, path, t.RunMode), nil
}

// executeSaveScript writes a reusable script to the scripts directory with
// executable permissions. Prepends a description comment header if provided.
func (a *Agent) executeSaveScript(input map[string]interface{}) (string, error) {
	if a.ScriptsDir == "" {
		return "", fmt.Errorf("save_script: scripts directory not configured")
	}

	name, ok := input["name"].(string)
	if !ok || name == "" {
		return "", fmt.Errorf("save_script: missing or invalid 'name' field")
	}

	content, ok := input["content"].(string)
	if !ok || content == "" {
		return "", fmt.Errorf("save_script: missing or invalid 'content' field")
	}

	// Prepend description as comment if provided
	if desc, ok := input["description"].(string); ok && desc != "" {
		content = "# " + desc + "\n\n" + content
	}

	// Ensure scripts directory exists
	if err := os.MkdirAll(a.ScriptsDir, 0755); err != nil {
		return "", fmt.Errorf("save_script: failed to create scripts dir: %w", err)
	}

	scriptPath := filepath.Join(a.ScriptsDir, name)
	if err := os.WriteFile(scriptPath, []byte(content), 0755); err != nil {
		return "", fmt.Errorf("save_script: failed to write script: %w", err)
	}

	return fmt.Sprintf("Script saved to %s (executable)", scriptPath), nil
}

// executeSaveSkill writes a skill file (markdown) to the skills directory.
// Skills are reusable knowledge that gets injected into task system prompts.
func (a *Agent) executeSaveSkill(input map[string]interface{}) (string, error) {
	if a.SkillsDir == "" {
		return "", fmt.Errorf("save_skill: skills directory not configured")
	}

	name, ok := input["name"].(string)
	if !ok || name == "" {
		return "", fmt.Errorf("save_skill: missing or invalid 'name' field")
	}

	content, ok := input["content"].(string)
	if !ok || content == "" {
		return "", fmt.Errorf("save_skill: missing or invalid 'content' field")
	}

	// Ensure skills directory exists
	if err := os.MkdirAll(a.SkillsDir, 0755); err != nil {
		return "", fmt.Errorf("save_skill: failed to create skills dir: %w", err)
	}

	skillPath := filepath.Join(a.SkillsDir, name)
	if err := os.WriteFile(skillPath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("save_skill: failed to write skill: %w", err)
	}

	return fmt.Sprintf("Skill saved to %s", skillPath), nil
}

// executeTestTask runs a task through the real execution path and returns
// the result. Used by the build agent to validate tasks after creation.
func (a *Agent) executeTestTask(ctx context.Context, input map[string]interface{}) (string, error) {
	if a.TestTaskFn == nil {
		return "", fmt.Errorf("test_task: not available in this context")
	}

	taskName, ok := input["task_name"].(string)
	if !ok || taskName == "" {
		return "", fmt.Errorf("test_task: missing or invalid 'task_name' field")
	}

	return a.TestTaskFn(ctx, taskName)
}

// Tool implementations

func executeBash(command string) (string, error) {
	cmd := exec.Command("bash", "-c", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), err
	}
	return string(output), nil
}

func readFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

// truncate shortens a string for log output
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// loadScriptsList reads the scripts directory and returns a formatted list
// of available scripts for injection into the system prompt.
// Returns empty string if no scripts exist or directory doesn't exist.
func loadScriptsList(scriptsDir string) string {
	entries, err := os.ReadDir(scriptsDir)
	if err != nil {
		return ""
	}

	if len(entries) == 0 {
		return ""
	}

	var result string
	result = "\n## Available Scripts\n\n"
	result += fmt.Sprintf("Scripts directory: %s\n\n", scriptsDir)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Read first line to check for description comment
		content, err := os.ReadFile(filepath.Join(scriptsDir, entry.Name()))
		if err != nil {
			result += fmt.Sprintf("- `%s`\n", entry.Name())
			continue
		}

		// Extract description from comment header (first line starting with #)
		lines := string(content)
		desc := ""
		for _, line := range splitLines(lines) {
			if len(line) > 2 && line[0] == '#' && line[1] == ' ' {
				desc = line[2:]
				break
			}
			break // only check first line
		}

		if desc != "" {
			result += fmt.Sprintf("- `%s` — %s\n", entry.Name(), desc)
		} else {
			result += fmt.Sprintf("- `%s`\n", entry.Name())
		}
	}

	return result
}

// splitLines splits a string into lines.
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// --- Context compaction ---

const (
	defaultCompactionThreshold = 100000 // 100k tokens (~50% of 200k context window)
	compactionKeepMessages     = 6      // keep last 6 messages (3 iteration cycles) verbatim
	maxSummarizationInput      = 50000  // max chars of old conversation to send for summarization
	maxToolResultChars         = 500    // truncate individual tool results in summarization input
)

// resolveCompactionThreshold returns the effective token threshold.
// 0 → default (100k), -1 → disabled, >0 → custom value.
func resolveCompactionThreshold(configured int) int {
	switch {
	case configured < 0:
		return -1 // disabled
	case configured == 0:
		return defaultCompactionThreshold
	default:
		return configured
	}
}

// compactContext summarizes old messages to reduce context size.
// Keeps the last K messages verbatim and replaces everything before them
// with a single UserMessage containing the original prompt + a summary.
// Returns the compacted messages array and token usage from the summarization call.
func compactContext(
	ctx context.Context,
	client anthropic.Client,
	model string,
	messages []anthropic.MessageParam,
	originalPrompt string,
) ([]anthropic.MessageParam, int64, int64, error) {
	// Need enough messages to make compaction worthwhile
	minMessages := compactionKeepMessages + 2
	if len(messages) <= minMessages {
		return messages, 0, 0, nil // not enough to compact
	}

	// Split into old (to summarize) and recent (to keep verbatim)
	splitIdx := len(messages) - compactionKeepMessages

	// Ensure split point lands on an assistant message boundary
	// (messages alternate: user, assistant, user, assistant, ...)
	// We want recentMessages to start with an assistant message so that
	// when prepended with a user summary message, alternation is valid.
	if splitIdx > 0 && messages[splitIdx].Role != anthropic.MessageParamRoleAssistant {
		splitIdx--
	}
	if splitIdx <= 0 {
		return messages, 0, 0, nil
	}

	oldMessages := messages[:splitIdx]
	recentMessages := messages[splitIdx:]

	// Extract text from old messages for summarization
	conversationText := extractMessagesText(oldMessages)
	if len(conversationText) > maxSummarizationInput {
		conversationText = conversationText[:maxSummarizationInput] + "\n... (truncated)"
	}

	// Summarize via API call
	summaryPrompt := fmt.Sprintf(`Summarize this agent conversation history concisely. Preserve:
- Key decisions and reasoning
- Files created, modified, or read (with paths)
- Errors encountered and how they were resolved
- Current state and what remains to be done

Be brief — this summary replaces the detailed history.

CONVERSATION:
%s`, conversationText)

	resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: 2048,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(summaryPrompt)),
		},
	})
	if err != nil {
		return nil, 0, 0, fmt.Errorf("summarization API call failed: %w", err)
	}

	// Extract summary text from response
	var summary string
	for _, block := range resp.Content {
		if v, ok := block.AsAny().(anthropic.TextBlock); ok {
			summary += v.Text
		}
	}

	if summary == "" {
		return nil, resp.Usage.InputTokens, resp.Usage.OutputTokens,
			fmt.Errorf("summarization returned empty response")
	}

	// Build compacted messages array:
	// [1] UserMessage with original prompt + summary (replaces all old messages)
	// [K] Recent messages kept verbatim
	compactedPrompt := fmt.Sprintf("Original task: %s\n\n--- CONTEXT SUMMARY (from previous iterations) ---\n\n%s",
		originalPrompt, summary)

	compacted := make([]anthropic.MessageParam, 0, 1+len(recentMessages))
	compacted = append(compacted, anthropic.NewUserMessage(anthropic.NewTextBlock(compactedPrompt)))
	compacted = append(compacted, recentMessages...)

	return compacted, resp.Usage.InputTokens, resp.Usage.OutputTokens, nil
}

// extractMessagesText walks the messages array and extracts human-readable text
// for the summarization prompt. Tool results are truncated to keep the input compact.
func extractMessagesText(messages []anthropic.MessageParam) string {
	var sb fmt.Stringer = &messagesTextExtractor{messages: messages}
	return sb.String()
}

type messagesTextExtractor struct {
	messages []anthropic.MessageParam
}

func (e *messagesTextExtractor) String() string {
	var result string
	for _, msg := range e.messages {
		role := string(msg.Role)
		for _, block := range msg.Content {
			if block.OfText != nil {
				text := block.OfText.Text
				if len(text) > maxToolResultChars*2 {
					text = text[:maxToolResultChars*2] + "..."
				}
				result += fmt.Sprintf("[%s] %s\n", role, text)
			} else if block.OfToolUse != nil {
				result += fmt.Sprintf("[%s] tool_call: %s\n", role, block.OfToolUse.Name)
			} else if block.OfToolResult != nil {
				// Extract text from tool result content
				toolText := ""
				for _, c := range block.OfToolResult.Content {
					if c.OfText != nil {
						toolText += c.OfText.Text
					}
				}
				if len(toolText) > maxToolResultChars {
					toolText = toolText[:maxToolResultChars] + "..."
				}
				isErr := ""
				if block.OfToolResult.IsError.Value {
					isErr = " (ERROR)"
				}
				result += fmt.Sprintf("[%s] tool_result%s: %s\n", role, isErr, toolText)
			}
		}
	}
	return result
}
