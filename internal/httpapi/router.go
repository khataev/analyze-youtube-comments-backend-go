// Package httpapi собирает HTTP-приложение из модулей и core: маршруты модулей,
// /health и middleware. Аналог create_app() в Python-версии и api.module.ts в Node;
// используется и в main, и в интеграционных тестах.
//
// Это единственный пакет internal/, который знает и о core, и о modules.
package httpapi

import (
	"log/slog"
	"net/http"

	"ytcomments/internal/core/health"
	"ytcomments/internal/core/httpx"
	"ytcomments/internal/modules/analysis"
)

// Deps — всё, что нужно HTTP-приложению.
type Deps struct {
	Analysis    *analysis.Handler
	Health      []health.Check // /health отвечает 200, только если прошли все
	CORSOrigins []string
	Logger      *slog.Logger
}

// NewRouter возвращает готовый http.Handler.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	d.Analysis.Register(mux, "/api/v1")
	mux.Handle("GET /health", health.Handler(d.Health, d.Logger))

	return httpx.Chain(mux,
		httpx.Recover(d.Logger),
		httpx.LogRequests(d.Logger),
		httpx.CORS(d.CORSOrigins),
	)
}
