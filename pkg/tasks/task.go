package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RunMode determines how a task gets triggered
type RunMode string

const (
	RunModeScheduled RunMode = "scheduled" // runs on cron via scheduler → channel → orchestrator
	RunModeImmediate RunMode = "immediate" // runs on demand, bypasses scheduler (CLI, orchestrator agent)
)

type Task struct {
	Name        string            `json:"name"`         // unique identifier, e.g. "defi-health-check"
	Description string            `json:"description"`  // human-readable
	Prompt      string            `json:"prompt"`        // the instruction for the agent
	Skills      map[string]string `json:"skills"`        // skill name → file path
	MemoryDir   string            `json:"memory_dir"`    // directory for persistent memory across runs
	RunMode     RunMode           `json:"run_mode"`      // "scheduled" or "immediate"
	Model       string            `json:"model"`         // model override (optional, empty = use agent default)
	Schedule     string            `json:"schedule"`               // e.g. "30m", "1h", "24h" — parsed by time.ParseDuration
	AllowedTools []string          `json:"allowed_tools,omitempty"` // optional per-task tool whitelist
	MaxRetries   int               `json:"max_retries,omitempty"`   // 0 = use config default
	RetryBackoff string            `json:"retry_backoff,omitempty"` // base duration, e.g. "2s"
}

// LoadTask reads a single task definition from a JSON file
func LoadTask(path string) (Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Task{}, fmt.Errorf("failed to read task file %s: %w", path, err)
	}

	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return Task{}, fmt.Errorf("failed to parse task file %s: %w", path, err)
	}

	return t, nil
}

// LoadTasks reads all JSON task definitions from a directory
func LoadTasks(dir string) ([]Task, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read tasks directory %s: %w", dir, err)
	}

	var tasks []Task
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		t, err := LoadTask(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}

	return tasks, nil
}

// LoadSkills reads all skill files and returns their combined content,
// ready to inject into the system prompt.
func (t *Task) LoadSkills() (string, error) {
	if len(t.Skills) == 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString("\n## Skills\n\n")

	for name, path := range t.Skills {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("failed to read skill file %s (%s): %w", name, path, err)
		}
		sb.WriteString(fmt.Sprintf("### %s\n\n", name))
		sb.WriteString(string(content))
		sb.WriteString("\n\n")
	}

	return sb.String(), nil
}

// LoadMemory reads all files from the task's memory directory and returns
// their combined content. Returns empty string on first run (no memory yet).
// The agent writes its own memory via write_file during execution.
func (t *Task) LoadMemory() (string, error) {
	if t.MemoryDir == "" {
		return "", nil
	}

	// Create memory dir if it doesn't exist (first run)
	if err := os.MkdirAll(t.MemoryDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create memory dir %s: %w", t.MemoryDir, err)
	}

	entries, err := os.ReadDir(t.MemoryDir)
	if err != nil {
		return "", fmt.Errorf("failed to read memory dir %s: %w", t.MemoryDir, err)
	}

	if len(entries) == 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString("\n## Memory from previous runs\n\n")

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(t.MemoryDir, entry.Name()))
		if err != nil {
			return "", fmt.Errorf("failed to read memory file %s: %w", entry.Name(), err)
		}
		sb.WriteString(fmt.Sprintf("### %s\n\n", entry.Name()))
		sb.WriteString(string(content))
		sb.WriteString("\n\n")
	}

	return sb.String(), nil
}
