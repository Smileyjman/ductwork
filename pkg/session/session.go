package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// Checkpoint captures the agent's conversation state at a point in time.
// Persisted to disk after each iteration so that failed runs can resume.
type Checkpoint struct {
	RunID        string                   `json:"run_id"`
	TaskName     string                   `json:"task_name"`      // empty for adhoc tasks
	Iteration    int                      `json:"iteration"`
	Messages     []anthropic.MessageParam `json:"messages"`
	SystemPrompt string                   `json:"system_prompt"`  // stored for debugging context
	InputTokens  int64                    `json:"input_tokens"`
	OutputTokens int64                    `json:"output_tokens"`
	CreatedAt    time.Time                `json:"created_at"`
}

// Store persists and retrieves session checkpoints on the filesystem.
// Each run gets exactly one checkpoint file that is overwritten each iteration.
type Store struct {
	dir string
}

// NewStore creates a new session store backed by the given directory.
// Creates the directory if it doesn't exist.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("session: failed to create directory %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Save persists a checkpoint to disk, overwriting any previous checkpoint
// for the same RunID.
func (s *Store) Save(cp *Checkpoint) error {
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("session: failed to marshal checkpoint: %w", err)
	}

	path := s.path(cp.RunID)

	// Write to temp file first, then rename for atomic write
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("session: failed to write checkpoint: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		// Clean up temp file on rename failure
		os.Remove(tmpPath)
		return fmt.Errorf("session: failed to finalize checkpoint: %w", err)
	}

	return nil
}

// Load reads a checkpoint from disk for the given RunID.
// Returns an error if no checkpoint exists.
func (s *Store) Load(runID string) (*Checkpoint, error) {
	data, err := os.ReadFile(s.path(runID))
	if err != nil {
		return nil, fmt.Errorf("session: failed to read checkpoint for %s: %w", runID, err)
	}

	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("session: failed to parse checkpoint for %s: %w", runID, err)
	}

	return &cp, nil
}

// Delete removes a checkpoint file. Called after successful task completion.
// Returns nil if the file doesn't exist (idempotent).
func (s *Store) Delete(runID string) error {
	err := os.Remove(s.path(runID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("session: failed to delete checkpoint for %s: %w", runID, err)
	}
	return nil
}

// HasCheckpoint returns true if a checkpoint exists for the given RunID.
func (s *Store) HasCheckpoint(runID string) bool {
	_, err := os.Stat(s.path(runID))
	return err == nil
}

// List returns all checkpoints in the store, sorted by creation time (newest first).
// Reads only metadata — does not load full message histories.
func (s *Store) List() ([]Checkpoint, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("session: failed to read sessions dir: %w", err)
	}

	var checkpoints []Checkpoint
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		// Skip temp files
		if strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			continue
		}

		var cp Checkpoint
		if err := json.Unmarshal(data, &cp); err != nil {
			continue
		}

		// Clear messages to save memory when listing
		cp.Messages = nil
		checkpoints = append(checkpoints, cp)
	}

	// Sort newest first
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt)
	})

	return checkpoints, nil
}

// path returns the filesystem path for a checkpoint file.
func (s *Store) path(runID string) string {
	return filepath.Join(s.dir, runID+".json")
}
