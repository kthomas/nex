package agentapi

import (
	"disorder.dev/shandler"
	"log/slog"
)

const (
	LogLevelPanic slog.Level = 12
	LogLevelFatal slog.Level = 10
	LogLevelError slog.Level = slog.LevelError // 8
	LogLevelWarn  slog.Level = slog.LevelWarn  // 4
	LogLevelInfo  slog.Level = slog.LevelInfo  // 0
	LogLevelDebug slog.Level = slog.LevelDebug // -4
	LogLevelTrace slog.Level = shandler.LevelTrace
)
