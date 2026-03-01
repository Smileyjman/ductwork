package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

//go:embed default_tools.json
var defaultToolsJSON []byte

// Config holds the global agent orchestrator configuration.
// Loaded from .agent/config.json.
type Config struct {
	DefaultModel string `json:"default_model"` // default: "claude-sonnet-4-6"
	SystemPrompt string `json:"system_prompt"` // base system prompt for all agents
	TasksDir     string `json:"tasks_dir"`     // default: "tasks"
	SkillsDir    string `json:"skills_dir"`    // default: "skills"
	MemoryDir    string `json:"memory_dir"`    // default: "memory"
	LogsDir      string `json:"logs_dir"`      // default: "logs"
	ScriptsDir   string `json:"scripts_dir"`   // default: "scripts"
	HistoryDir   string `json:"history_dir"`   // default: "history"
	APIPort      int    `json:"api_port"`      // default: 8080
	Debug        bool   `json:"debug"`         // default: false

	// Concurrency and retry
	MaxConcurrent       int    `json:"max_concurrent"`        // default: 5
	DefaultMaxRetries   int    `json:"default_max_retries"`   // default: 2
	DefaultRetryBackoff string `json:"default_retry_backoff"` // default: "2s"

	// Config file paths (resolved at load time)
	ToolsFile        string `json:"tools_file"`        // default: "tools.json"
	SecurityFile     string `json:"security_file"`     // default: "security.json"
	DependenciesFile string `json:"dependencies_file"` // default: "dependencies.json"

	// RootDir is the absolute path to the .agent/ directory.
	// Not serialized — set at load time.
	RootDir string `json:"-"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		DefaultModel: "claude-sonnet-4-6",
		SystemPrompt: `You are an autonomous AI agent with access to the following tools:
- bash: Execute shell commands
- read_file: Read file contents
- write_file: Write content to files
- create_task: Create new task definitions
- save_script: Save reusable scripts

Use these tools to accomplish your tasks. Be methodical and verify your work.`,
		TasksDir:         "tasks",
		SkillsDir:        "skills",
		MemoryDir:        "memory",
		LogsDir:          "logs",
		ScriptsDir:          "scripts",
		HistoryDir:          "history",
		APIPort:             8080,
		Debug:               false,
		MaxConcurrent:       5,
		DefaultMaxRetries:   2,
		DefaultRetryBackoff: "2s",
		ToolsFile:           "tools.json",
		SecurityFile:     "security.json",
		DependenciesFile: "dependencies.json",
	}
}

// EnsureDir creates the .agent/ directory structure if it doesn't exist.
// Called on every boot so the system bootstraps itself on first run.
// If the directory already exists, this is a no-op.
func EnsureDir(agentDir string) error {
	absDir, err := filepath.Abs(agentDir)
	if err != nil {
		return fmt.Errorf("failed to resolve path %s: %w", agentDir, err)
	}

	configPath := filepath.Join(absDir, "config.json")

	// If config.json already exists, the directory is set up
	if _, err := os.Stat(configPath); err == nil {
		return nil
	}

	return Init(absDir)
}

// Init explicitly creates the .agent/ directory structure with default config.
func Init(dir string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("failed to resolve path %s: %w", dir, err)
	}

	// Create all subdirectories
	dirs := []string{
		absDir,
		filepath.Join(absDir, "tasks"),
		filepath.Join(absDir, "skills"),
		filepath.Join(absDir, "scripts"),
		filepath.Join(absDir, "history"),
		filepath.Join(absDir, "memory"),
		filepath.Join(absDir, "logs"),
	}

	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", d, err)
		}
	}

	// Write default config.json
	cfg := DefaultConfig()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	configPath := filepath.Join(absDir, "config.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	// Write default tools.json
	toolsPath := filepath.Join(absDir, "tools.json")
	if err := os.WriteFile(toolsPath, defaultToolsJSON, 0644); err != nil {
		return fmt.Errorf("failed to write tools.json: %w", err)
	}

	// Write default security.json
	secData := []byte(`{
  "default_tool_permissions": {
    "allowed_tools": ["bash", "read_file", "write_file", "create_task", "save_script"]
  },
  "task_overrides": {}
}
`)
	secPath := filepath.Join(absDir, "security.json")
	if err := os.WriteFile(secPath, secData, 0644); err != nil {
		return fmt.Errorf("failed to write security.json: %w", err)
	}

	// Write default dependencies.json
	depData := []byte(`{
  "runtimes": []
}
`)
	depPath := filepath.Join(absDir, "dependencies.json")
	if err := os.WriteFile(depPath, depData, 0644); err != nil {
		return fmt.Errorf("failed to write dependencies.json: %w", err)
	}

	slog.Info("initialized .agent/ directory", "module", "config", "path", absDir)
	return nil
}

// LoadConfig ensures the .agent/ directory exists, then loads config.json.
// All relative directory paths are resolved to absolute paths relative to the .agent/ root.
func LoadConfig(agentDir string) (*Config, error) {
	absDir, err := filepath.Abs(agentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve path %s: %w", agentDir, err)
	}

	// Auto-create if missing
	if err := EnsureDir(absDir); err != nil {
		return nil, fmt.Errorf("failed to ensure .agent/ dir: %w", err)
	}

	// Read config.json
	configPath := filepath.Join(absDir, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Resolve relative paths to absolute
	cfg.RootDir = absDir
	cfg.TasksDir = filepath.Join(absDir, cfg.TasksDir)
	cfg.SkillsDir = filepath.Join(absDir, cfg.SkillsDir)
	cfg.MemoryDir = filepath.Join(absDir, cfg.MemoryDir)
	cfg.LogsDir = filepath.Join(absDir, cfg.LogsDir)
	cfg.ScriptsDir = filepath.Join(absDir, cfg.ScriptsDir)
	cfg.HistoryDir = filepath.Join(absDir, cfg.HistoryDir)
	cfg.ToolsFile = filepath.Join(absDir, cfg.ToolsFile)
	cfg.SecurityFile = filepath.Join(absDir, cfg.SecurityFile)
	cfg.DependenciesFile = filepath.Join(absDir, cfg.DependenciesFile)

	return &cfg, nil
}
