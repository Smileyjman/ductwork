package security

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SecurityConfig is the top-level structure loaded from .agent/security.json.
type SecurityConfig struct {
	// Global defaults — applied to any task that doesn't have an override
	DefaultToolPermissions ToolPermissions        `json:"default_tool_permissions"`
	TaskOverrides          map[string]TaskSecurity `json:"task_overrides"`
}

// ToolPermissions defines which tools are allowed globally.
type ToolPermissions struct {
	AllowedTools []string `json:"allowed_tools"`
}

// TaskSecurity holds security overrides for a specific task.
type TaskSecurity struct {
	AllowedTools   []string       `json:"allowed_tools,omitempty"`
	PathBoundaries PathBoundaries `json:"path_boundaries,omitempty"`
	BashRules      BashRules      `json:"bash_rules,omitempty"`
}

// PathBoundaries defines where the agent can read and write.
type PathBoundaries struct {
	AllowedReadPaths  []string `json:"allowed_read_paths,omitempty"`  // glob patterns
	AllowedWritePaths []string `json:"allowed_write_paths,omitempty"` // glob patterns
}

// BashRules restricts what bash commands can be run.
type BashRules struct {
	AllowPatterns []string `json:"allow_patterns,omitempty"` // regex — if set, command must match at least one
	BlockPatterns []string `json:"block_patterns,omitempty"` // regex — checked first, any match = deny
}

// DefaultSecurityConfig returns a fully-open config (backward compatible).
func DefaultSecurityConfig() *SecurityConfig {
	return &SecurityConfig{
		DefaultToolPermissions: ToolPermissions{
			AllowedTools: []string{"bash", "read_file", "write_file", "create_task", "save_script", "save_skill", "test_task"},
		},
		TaskOverrides: map[string]TaskSecurity{},
	}
}

// LoadSecurityConfig reads and parses .agent/security.json.
// Returns a permissive default if the file doesn't exist.
func LoadSecurityConfig(path string) (*SecurityConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultSecurityConfig(), nil
		}
		return nil, fmt.Errorf("failed to read security config: %w", err)
	}

	var cfg SecurityConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse security config: %w", err)
	}

	if cfg.TaskOverrides == nil {
		cfg.TaskOverrides = map[string]TaskSecurity{}
	}

	return &cfg, nil
}

// Enforcer checks tool calls against security rules before execution.
// Created once per task execution with the resolved rules for that task.
type Enforcer struct {
	allowedTools  map[string]bool
	readGlobs     []string
	writeGlobs    []string
	blockPatterns []*regexp.Regexp
	allowPatterns []*regexp.Regexp
}

// NewEnforcer creates an Enforcer for a specific task by merging global
// defaults with any task-specific overrides from security.json.
func NewEnforcer(cfg *SecurityConfig, taskName string) (*Enforcer, error) {
	e := &Enforcer{
		allowedTools: make(map[string]bool),
	}

	// Start with global defaults
	for _, t := range cfg.DefaultToolPermissions.AllowedTools {
		e.allowedTools[t] = true
	}

	// Apply task-specific overrides if they exist
	if override, ok := cfg.TaskOverrides[taskName]; ok {
		// If task specifies allowed tools, replace the global set
		if len(override.AllowedTools) > 0 {
			e.allowedTools = make(map[string]bool)
			for _, t := range override.AllowedTools {
				e.allowedTools[t] = true
			}
		}

		// Path boundaries
		e.readGlobs = override.PathBoundaries.AllowedReadPaths
		e.writeGlobs = override.PathBoundaries.AllowedWritePaths

		// Compile bash rules
		for _, pattern := range override.BashRules.BlockPatterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid block pattern %q: %w", pattern, err)
			}
			e.blockPatterns = append(e.blockPatterns, re)
		}
		for _, pattern := range override.BashRules.AllowPatterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("invalid allow pattern %q: %w", pattern, err)
			}
			e.allowPatterns = append(e.allowPatterns, re)
		}
	}

	return e, nil
}

// NewStaticEnforcer creates an enforcer with just a tool whitelist.
// Convenience for hardcoded restrictions (e.g. the build command).
func NewStaticEnforcer(allowedTools []string) *Enforcer {
	toolSet := make(map[string]bool, len(allowedTools))
	for _, t := range allowedTools {
		toolSet[t] = true
	}
	return &Enforcer{allowedTools: toolSet}
}

// CheckTool verifies that a tool name is in the allowed set.
func (e *Enforcer) CheckTool(toolName string) error {
	if len(e.allowedTools) == 0 {
		return nil // no restrictions
	}
	if !e.allowedTools[toolName] {
		return fmt.Errorf("security: tool %q is not allowed for this task", toolName)
	}
	return nil
}

// CheckPath verifies that a file path is within allowed boundaries.
// operation is "read" or "write".
func (e *Enforcer) CheckPath(filePath string, operation string) error {
	var globs []string
	if operation == "read" {
		globs = e.readGlobs
	} else {
		globs = e.writeGlobs
	}

	// No restrictions defined = allow all
	if len(globs) == 0 {
		return nil
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Errorf("security: cannot resolve path %q: %w", filePath, err)
	}

	for _, glob := range globs {
		if matchesPathBoundary(absPath, glob) {
			return nil
		}
	}

	return fmt.Errorf("security: %s access denied for path %q — not within allowed boundaries", operation, absPath)
}

// CheckBashCommand verifies a bash command against block/allow patterns.
// Block patterns are checked first; if any match, the command is denied.
// If allow patterns are defined and none match, the command is denied.
func (e *Enforcer) CheckBashCommand(command string) error {
	// Check block patterns first (any match = deny)
	for _, re := range e.blockPatterns {
		if re.MatchString(command) {
			return fmt.Errorf("security: bash command blocked by pattern %q", re.String())
		}
	}

	// If allow patterns are defined, command must match at least one
	if len(e.allowPatterns) > 0 {
		for _, re := range e.allowPatterns {
			if re.MatchString(command) {
				return nil
			}
		}
		return fmt.Errorf("security: bash command not in allowed patterns")
	}

	return nil
}

// matchesPathBoundary checks if a path matches a boundary pattern.
// Supports /** suffix for directory prefix matching, otherwise uses filepath.Match.
func matchesPathBoundary(absPath, pattern string) bool {
	// Handle /** directory prefix pattern
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return strings.HasPrefix(absPath, prefix)
	}

	// Handle /* single-level wildcard
	matched, _ := filepath.Match(pattern, absPath)
	return matched
}
