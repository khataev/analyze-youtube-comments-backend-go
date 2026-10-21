// Package httpapi собирает HTTP-приложение: маршруты всех доменов, /health и middleware.
// Аналог create_app() в Python-версии; используется и в main, и в интеграционных тестах.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"ytcomments/internal/analysis"
	"ytcomments/internal/httpx"
)

const healthTimeout = 2 * time.Second

// Pinger проверяет доступность зависимости для /health. *pgxpool.Pool подходит как есть.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PingerFunc позволяет передать функцию как Pinger (тот же приём, что http.HandlerFunc).
type PingerFunc func(ctx context.Context) error

// Ping вызывает саму функцию.
func (f PingerFunc) Ping(ctx context.Context) error { return f(ctx) }

// HealthCheck — именованная проверка: имя попадает в лог, если зависимость недоступна.
type HealthCheck struct {
	Name   string
	Pinger Pinger
}

// Deps — всё, что нужно HTTP-приложению.
type Deps struct {
	Analysis    *analysis.Handler
	Health      []HealthCheck // /health отвечает 200, только если прошли все
	CORSOrigins []string
	Logger      *slog.Logger
}

// NewRouter возвращает готовый http.Handler.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	d.Analysis.Register(mux, "/api/v1")
	mux.HandleFunc("GET /health", health(d.Health, d.Logger))

	return httpx.Chain(mux,
		httpx.Recover(d.Logger),
		httpx.LogRequests(d.Logger),
		httpx.CORS(d.CORSOrigins),
	)
}

// health проверяет все зависимости (Postgres и Redis). Проверки идут параллельно:
// при недоступной зависимости ответ занимает не больше healthTimeout, а не сумму таймаутов.
func health(checks []HealthCheck, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
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
			httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorBody{Detail: analysis.ErrStorageUnavailable.Message})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
