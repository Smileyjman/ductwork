package logging

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
)

// Context keys for correlation fields
type ctxKey string

const (
	keyTaskName ctxKey = "task_name"
	keyRunID    ctxKey = "run_id"
)

// multiHandler fans out log records to multiple slog.Handlers.
// JSON goes to file, text goes to stdout.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: handlers}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: handlers}
}

// Setup initializes the global slog logger with two outputs:
//   - JSON handler → logsDir/orchestrator.log (all levels)
//   - Text handler → stdout (INFO+ unless debug is true)
//
// Returns a cleanup function to close the log file.
func Setup(logsDir string, debug bool) (func(), error) {
	// Ensure logs directory exists
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return nil, err
	}

	// Open log file (append mode)
	logPath := filepath.Join(logsDir, "orchestrator.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	// JSON handler → file (captures everything at DEBUG level)
	jsonHandler := slog.NewJSONHandler(logFile, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	// Text handler → stdout (INFO+ by default, DEBUG if debug mode)
	stdoutLevel := slog.LevelInfo
	if debug {
		stdoutLevel = slog.LevelDebug
	}
	textHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: stdoutLevel,
	})

	// Multi-handler fans out to both
	multi := &multiHandler{
		handlers: []slog.Handler{jsonHandler, textHandler},
	}

	slog.SetDefault(slog.New(multi))

	cleanup := func() {
		logFile.Close()
	}

	return cleanup, nil
}

// NewContext returns a child context with task correlation fields attached.
func NewContext(ctx context.Context, taskName string, runID string) context.Context {
	ctx = context.WithValue(ctx, keyTaskName, taskName)
	ctx = context.WithValue(ctx, keyRunID, runID)
	return ctx
}

// FromContext extracts a logger with correlation fields from the context.
// Falls back to slog.Default() if no fields are present.
func FromContext(ctx context.Context) *slog.Logger {
	logger := slog.Default()

	if taskName, ok := ctx.Value(keyTaskName).(string); ok {
		logger = logger.With("task_name", taskName)
	}
	if runID, ok := ctx.Value(keyRunID).(string); ok {
		logger = logger.With("run_id", runID)
	}

	return logger
}

// ForModule returns a logger with the "module" attribute pre-set.
func ForModule(module string) *slog.Logger {
	return slog.Default().With("module", module)
}
