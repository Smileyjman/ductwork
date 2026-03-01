package dependencies

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// DependencyConfig describes available language runtimes and packages.
// Loaded from .agent/dependencies.json and injected into the agent's system prompt.
type DependencyConfig struct {
	Runtimes []Runtime `json:"runtimes"`
}

// Runtime describes a single language runtime available to the agent.
type Runtime struct {
	Language string   `json:"language"` // "python", "go", "node", "ruby", etc.
	Version  string   `json:"version"`  // "3.12", "1.23", "20"
	Packages []string `json:"packages"` // ["requests", "pandas", "numpy"]
	RunCmd   string   `json:"run_cmd"`  // "poetry run python", "go run", "npx"
	Notes    string   `json:"notes"`    // freeform notes for the system prompt
}

// LoadDependencies reads .agent/dependencies.json.
// Returns an empty config if the file doesn't exist.
func LoadDependencies(path string) (*DependencyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &DependencyConfig{}, nil
		}
		return nil, fmt.Errorf("failed to read dependencies config: %w", err)
	}

	var cfg DependencyConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse dependencies config: %w", err)
	}

	return &cfg, nil
}

// ToSystemPrompt formats the dependency config as a markdown section
// for injection into the agent's system prompt.
// Returns empty string if no runtimes are configured.
func (d *DependencyConfig) ToSystemPrompt() string {
	if len(d.Runtimes) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n## Available Runtimes & Dependencies\n\n")

	for _, r := range d.Runtimes {
		sb.WriteString(fmt.Sprintf("### %s %s\n", r.Language, r.Version))
		sb.WriteString(fmt.Sprintf("Run command: `%s`\n", r.RunCmd))
		if len(r.Packages) > 0 {
			sb.WriteString(fmt.Sprintf("Available packages: %s\n", strings.Join(r.Packages, ", ")))
		}
		if r.Notes != "" {
			sb.WriteString(fmt.Sprintf("Notes: %s\n", r.Notes))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}
