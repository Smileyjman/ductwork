package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Status represents the lifecycle state of a run.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// RunRecord captures metadata about a single task execution.
type RunRecord struct {
	RunID        string     `json:"run_id"`
	TaskName     string     `json:"task_name"`
	Status       Status     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	Duration     string     `json:"duration,omitempty"`
	Error        string     `json:"error,omitempty"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	Iterations   int        `json:"iterations"`
	Retries      int        `json:"retries"`
}

// Store defines the interface for persisting run records.
type Store interface {
	Save(record *RunRecord) error
	GetByTask(taskName string, limit int) ([]RunRecord, error)
	GetRecent(limit int) ([]RunRecord, error)
}

// FileStore is a file-based Store implementation.
// Each run is stored as a separate JSON file: {dir}/{runID}.json
type FileStore struct {
	dir string
}

// NewFileStore creates a FileStore, ensuring the directory exists.
func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create history dir: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

// Save writes or updates a RunRecord to disk.
func (s *FileStore) Save(record *RunRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal run record: %w", err)
	}
	path := filepath.Join(s.dir, record.RunID+".json")
	return os.WriteFile(path, data, 0644)
}

// GetByTask returns records for a specific task, sorted by StartedAt descending.
func (s *FileStore) GetByTask(taskName string, limit int) ([]RunRecord, error) {
	all, err := s.loadAll()
	if err != nil {
		return nil, err
	}

	var filtered []RunRecord
	for _, r := range all {
		if r.TaskName == taskName {
			filtered = append(filtered, r)
		}
	}

	sortByTime(filtered)
	return head(filtered, limit), nil
}

// GetRecent returns the most recent runs across all tasks.
func (s *FileStore) GetRecent(limit int) ([]RunRecord, error) {
	all, err := s.loadAll()
	if err != nil {
		return nil, err
	}
	sortByTime(all)
	return head(all, limit), nil
}

// loadAll reads all run records from the directory.
func (s *FileStore) loadAll() ([]RunRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}

	var records []RunRecord
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			continue // skip unreadable files
		}
		var r RunRecord
		if err := json.Unmarshal(data, &r); err != nil {
			continue // skip corrupt files
		}
		records = append(records, r)
	}
	return records, nil
}

// sortByTime sorts records by StartedAt descending (newest first).
func sortByTime(records []RunRecord) {
	sort.Slice(records, func(i, j int) bool {
		return records[i].StartedAt.After(records[j].StartedAt)
	})
}

// head returns the first n records (or all if limit <= 0 or >= len).
func head(records []RunRecord, limit int) []RunRecord {
	if limit <= 0 || limit >= len(records) {
		return records
	}
	return records[:limit]
}
