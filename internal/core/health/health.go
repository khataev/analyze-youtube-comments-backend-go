// Package health — обработчик /health: проверяет внешние зависимости процесса
// (Postgres, Redis). Инфраструктура без бизнес-смысла, поэтому живёт в core
// и ничего не знает о модулях: что проверять, передаёт сборщик приложения.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"ytcomments/internal/core/httpx"
)

const (
	timeout = 2 * time.Second
	// unavailableMessage — тело 503 по контракту API (совпадает с Python-версией).
	unavailableMessage = "Storage is unavailable"
)

// Pinger проверяет доступность зависимости. *pgxpool.Pool подходит как есть.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PingerFunc позволяет передать функцию как Pinger (тот же приём, что http.HandlerFunc).
type PingerFunc func(ctx context.Context) error

// Ping вызывает саму функцию.
func (f PingerFunc) Ping(ctx context.Context) error { return f(ctx) }

// Check — именованная проверка: имя попадает в лог, если зависимость недоступна.
type Check struct {
	Name   string
	Pinger Pinger
}

// Handler отвечает 200 {"status": "ok"}, если прошли все проверки, иначе 503.
// Проверки идут параллельно: при недоступной зависимости ответ занимает не больше
// timeout, а не сумму таймаутов.
func Handler(checks []Check, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		errs := make([]error, len(checks)) // каждая горутина пишет в свой элемент — гонки нет
		var wg sync.WaitGroup
		for i, check := range checks {
			wg.Go(func() { errs[i] = check.Pinger.Ping(ctx) }) // WaitGroup.Go — Go 1.25+
		}
		wg.Wait()

		healthy := true
		for i, err := range errs {
			if err != nil {
				healthy = false
				logger.ErrorContext(ctx, "health check failed", "dependency", checks[i].Name, "error", err)
			}
		}
		if !healthy {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorBody{Detail: unavailableMessage})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}
