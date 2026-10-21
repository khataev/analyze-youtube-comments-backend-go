// Package logging настраивает log/slog и умеет переносить поля лога через context.
//
// Идея: в начале обработки задачи кладём task_id в context
// (ctx = logging.With(ctx, "task_id", id)), а дальше любой код, который пишет
// logger.InfoContext(ctx, ...), автоматически добавит это поле. Так клиенту LLM не
// нужно знать про task_id, а все строки задачи в логе всё равно с ним.
// Похоже на Rails.logger.tagged / contextvars в Python.
package logging

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// New создаёт текстовый логгер с поддержкой полей из context.
func New(w io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(contextHandler{handler})
}

// Discard — логгер для тестов.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// ctxKey — приватный тип ключа: так чужой пакет не перезапишет наше значение в context.
type ctxKey struct{}

// With возвращает context, в котором к каждой записи лога добавятся args
// (пары ключ-значение, как у slog.Logger.With).
func With(ctx context.Context, args ...any) context.Context {
	attrs := append(attrsFrom(ctx), argsToAttrs(args)...)
	return context.WithValue(ctx, ctxKey{}, attrs)
}

func attrsFrom(ctx context.Context) []slog.Attr {
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	// Копия, чтобы два дочерних context не писали в один массив.
	return append([]slog.Attr(nil), attrs...)
}

func argsToAttrs(args []any) []slog.Attr {
	record := slog.NewRecord(time.Time{}, 0, "", 0)
	record.Add(args...)
	attrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	return attrs
}

// contextHandler оборачивает обычный slog.Handler и добавляет поля из context.
// Встраивание (embedding) slog.Handler даёт все методы интерфейса «по наследству»,
// переопределяем только нужные.
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := attrsFrom(ctx); len(attrs) > 0 {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
