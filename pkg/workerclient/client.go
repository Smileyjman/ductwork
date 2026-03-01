package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/dneil5648/ductwork/pkg/agent"
	"github.com/dneil5648/ductwork/pkg/security"
	"github.com/dneil5648/ductwork/pkg/worker"
)

// Client is the worker-side poll loop. It connects to the control plane,
// polls for task assignments, executes them locally, and reports results.
type Client struct {
	workerID     string
	controlURL   string
	pollInterval time.Duration
	httpClient   *http.Client
}

// NewClient creates a worker client that polls the given control plane URL.
func NewClient(workerID, controlURL string, pollInterval time.Duration) *Client {
	return &Client{
		workerID:     workerID,
		controlURL:   controlURL,
		pollInterval: pollInterval,
		httpClient:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Run starts the poll loop. It blocks until the context is cancelled.
func (c *Client) Run(ctx context.Context) error {
	slog.Info("worker client started",
		"worker_id", c.workerID,
		"control", c.controlURL,
		"poll_interval", c.pollInterval)

	for {
		select {
		case <-ctx.Done():
			slog.Info("worker client shutting down", "worker_id", c.workerID)
			return nil
		default:
		}

		// Poll for a task assignment
		assignment, err := c.poll(ctx)
		if err != nil {
			slog.Warn("poll failed", "worker_id", c.workerID, "error", err)
			select {
			case <-time.After(c.pollInterval):
			case <-ctx.Done():
				return nil
			}
			continue
		}

		// No task available — wait and poll again
		if assignment == nil {
			select {
			case <-time.After(c.pollInterval):
			case <-ctx.Done():
				return nil
			}
			continue
		}

		// Execute the task
		slog.Info("received task",
			"worker_id", c.workerID,
			"task", assignment.Task.Name,
			"run_id", assignment.RunID)

		result := c.executeTask(ctx, *assignment)

		slog.Info("task executed",
			"worker_id", c.workerID,
			"task", assignment.Task.Name,
			"status", result.Status,
			"input_tokens", result.InputTokens,
			"output_tokens", result.OutputTokens)

		// Report result back to the control plane
		if err := c.reportResult(ctx, result); err != nil {
			slog.Error("failed to report result",
				"worker_id", c.workerID,
				"run_id", assignment.RunID,
				"error", err)
		}
	}
}

// pollResponse is the JSON structure returned by POST /api/worker/poll.
type pollResponse struct {
	Status     string                 `json:"status"`
	Assignment *worker.TaskAssignment `json:"assignment,omitempty"`
}

// poll sends a poll request to the control plane and returns any assigned task.
// Returns nil assignment (no error) if the queue is empty.
func (c *Client) poll(ctx context.Context) (*worker.TaskAssignment, error) {
	body, err := json.Marshal(map[string]string{"worker_id": c.workerID})
	if err != nil {
		return nil, fmt.Errorf("marshal poll request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.controlURL+"/api/worker/poll", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create poll request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected poll status: %d", resp.StatusCode)
	}

	var pr pollResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("decode poll response: %w", err)
	}

	if pr.Status == "empty" {
		return nil, nil
	}

	return pr.Assignment, nil
}

// executeTask creates a local Agent and runs the assigned task.
// The agent uses pre-loaded skills/memory from the assignment —
// no filesystem access to .agent/ needed.
func (c *Client) executeTask(ctx context.Context, assignment worker.TaskAssignment) worker.TaskResult {
	// Create enforcer from the pre-resolved allowed tools list
	var enforcer *security.Enforcer
	if len(assignment.AllowedTools) > 0 {
		enforcer = security.NewStaticEnforcer(assignment.AllowedTools)
	}

	a := &agent.Agent{
		SystemPrompt:       assignment.SystemPrompt,
		Model:              assignment.Model,
		DependenciesPrompt: assignment.DependenciesPrompt,
		Enforcer:           enforcer,
	}

	// Use RunTaskWithPreloaded — receives skills/memory content directly
	result, err := a.RunTaskWithPreloaded(ctx,
		assignment.Task,
		assignment.SkillsContent,
		assignment.MemoryContent)

	tr := worker.TaskResult{
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

// resultPayload is the JSON body for POST /api/worker/result.
type resultPayload struct {
	WorkerID string            `json:"worker_id"`
	Result   worker.TaskResult `json:"result"`
}

// reportResult sends the execution result back to the control plane.
func (c *Client) reportResult(ctx context.Context, result worker.TaskResult) error {
	payload := resultPayload{
		WorkerID: c.workerID,
		Result:   result,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.controlURL+"/api/worker/result", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create result request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("result request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected result status: %d", resp.StatusCode)
	}

	return nil
}
