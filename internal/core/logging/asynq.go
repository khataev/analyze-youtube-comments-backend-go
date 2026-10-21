package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

// AsynqLogger адаптирует *slog.Logger к интерфейсу asynq.Logger, чтобы логи
// очереди шли в тот же поток и формат, что и наши.
// Интерфейс asynq.Logger здесь не импортируется: Go проверит совпадение методов
// в момент передачи значения в asynq.Config (неявная реализация интерфейса).
type AsynqLogger struct {
	logger *slog.Logger
}

// NewAsynqLogger создаёт адаптер.
func NewAsynqLogger(logger *slog.Logger) AsynqLogger {
	return AsynqLogger{logger: logger.With("component", "asynq")}
}

func (l AsynqLogger) log(level slog.Level, args []any) {
	l.logger.Log(context.Background(), level, fmt.Sprint(args...))
}

// Debug реализует asynq.Logger.
func (l AsynqLogger) Debug(args ...any) { l.log(slog.LevelDebug, args) }

// Info реализует asynq.Logger.
func (l AsynqLogger) Info(args ...any) { l.log(slog.LevelInfo, args) }

// Warn реализует asynq.Logger.
func (l AsynqLogger) Warn(args ...any) { l.log(slog.LevelWarn, args) }

// Error реализует asynq.Logger.
func (l AsynqLogger) Error(args ...any) { l.log(slog.LevelError, args) }

// Fatal реализует asynq.Logger: asynq ожидает, что после него процесс завершится.
func (l AsynqLogger) Fatal(args ...any) {
	l.log(slog.LevelError, args)
	os.Exit(1)
}
