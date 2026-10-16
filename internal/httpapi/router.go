// Package httpapi собирает HTTP-приложение: маршруты всех доменов, /health и middleware.
// Аналог create_app() в Python-версии; используется и в main, и в интеграционных тестах.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"ytcomments/internal/analysis"
	"ytcomments/internal/httpx"
)

const healthTimeout = 2 * time.Second

// Pinger проверяет доступность хранилища для /health.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PingerFunc позволяет передать функцию как Pinger (тот же приём, что http.HandlerFunc).
type PingerFunc func(ctx context.Context) error

// Ping вызывает саму функцию.
func (f PingerFunc) Ping(ctx context.Context) error { return f(ctx) }

// Deps — всё, что нужно HTTP-приложению.
type Deps struct {
	Analysis    *analysis.Handler
	Storage     Pinger
	CORSOrigins []string
	Logger      *slog.Logger
}

// NewRouter возвращает готовый http.Handler.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	d.Analysis.Register(mux, "/api/v1")
	mux.HandleFunc("GET /health", health(d.Storage, d.Logger))

	return httpx.Chain(mux,
		httpx.Recover(d.Logger),
		httpx.LogRequests(d.Logger),
		httpx.CORS(d.CORSOrigins),
	)
}

func health(storage Pinger, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
		defer cancel()
		if err := storage.Ping(ctx); err != nil {
			logger.ErrorContext(ctx, "health check failed", "error", err)
			httpx.WriteJSON(w, http.StatusServiceUnavailable, httpx.ErrorBody{Detail: analysis.ErrStorageUnavailable.Message})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
