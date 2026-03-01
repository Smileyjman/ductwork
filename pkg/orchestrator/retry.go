package orchestrator

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// RetryConfig holds resolved retry parameters for a single task execution.
type RetryConfig struct {
	MaxRetries  int
	BaseBackoff time.Duration
}

// Backoff returns the wait duration for a given attempt (0-indexed).
// Uses exponential backoff: base * 2^attempt, capped at 60 seconds.
func (rc *RetryConfig) Backoff(attempt int) time.Duration {
	d := rc.BaseBackoff
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

// IsTransient checks whether an error is likely transient and worth retrying.
//
// Returns true for:
//   - HTTP 429 (rate limit)
//   - HTTP 500+ (server errors)
//   - Network timeouts
//   - Connection refused/reset
//
// Returns false for:
//   - Security denials (Enforcer errors)
//   - Tool execution errors
//   - Context cancellation
//   - 400-level client errors (except 429)
func IsTransient(err error) bool {
	if err == nil {
		return false
	}

	// Context cancellation — never retry
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Check for Anthropic API errors with status codes
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 429:
			return true // rate limited
		case apiErr.StatusCode >= 500:
			return true // server error
		default:
			return false // 4xx client errors are not transient
		}
	}

	// Check for network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}

	// Check for connection-level errors via string matching
	errMsg := err.Error()
	if strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "EOF") {
		return true
	}

	// Security errors — never retry
	if strings.Contains(errMsg, "security:") {
		return false
	}

	return false
}
