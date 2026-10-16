package httpx

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Middleware в Go — это просто функция http.Handler → http.Handler
// (аналог Rack middleware: оборачивает следующий обработчик).
type Middleware func(http.Handler) http.Handler

// Chain применяет middleware так, что первая в списке оказывается внешней.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for _, mw := range slices.Backward(mws) {
		h = mw(h)
	}
	return h
}

// Recover превращает panic в обработчике в 500, а не в обрыв соединения.
// net/http и сам ловит panic, но тогда клиент не получает ответ, а лог неструктурный.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler { //nolint:errorlint // так требует документация net/http
						panic(v)
					}
					logger.ErrorContext(r.Context(), "panic in handler", "path", r.URL.Path, "panic", v)
					WriteJSON(w, http.StatusInternalServerError, ErrorBody{Detail: "Internal error"})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// LogRequests пишет одну строку на запрос: метод, путь, статус, длительность.
func LogRequests(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.InfoContext(r.Context(), "http request",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration", time.Since(started).Round(time.Millisecond))
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// CORS разрешает запросы с перечисленных origin (как CORSMiddleware в FastAPI:
// любые методы и заголовки). "*" в списке разрешает любой origin.
func CORS(allowedOrigins []string) Middleware {
	allowAll := slices.Contains(allowedOrigins, "*")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			allowed := allowAll || slices.Contains(allowedOrigins, origin)
			if origin == "" || !allowed {
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")

			isPreflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
			if !isPreflight {
				next.ServeHTTP(w, r)
				return
			}
			h.Set("Access-Control-Allow-Methods", strings.Join([]string{
				http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
			}, ", "))
			if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
				h.Set("Access-Control-Allow-Headers", reqHeaders)
			}
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
		})
	}
}
