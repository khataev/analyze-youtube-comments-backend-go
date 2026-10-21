// Package httpx — общие HTTP-утилиты: JSON-ответы, перевод ошибок в коды и middleware.
//
// Это единственное место, где apperr.Kind превращается в HTTP-статус
// (аналог register_exception_handlers в Python-версии).
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"ytcomments/internal/core/apperr"
)

// maxBodyBytes — ограничение размера тела запроса: в API приходит только URL.
const maxBodyBytes = 64 << 10 // 64 KiB

// ErrorBody — формат ошибки, совместимый с FastAPI: {"detail": "..."}.
type ErrorBody struct {
	Detail string `json:"detail"`
}

// WriteJSON пишет value как JSON с указанным статусом.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Ошибку записи игнорируем сознательно: заголовки уже отправлены, клиент, скорее всего, ушёл.
	_ = json.NewEncoder(w).Encode(value)
}

// WriteError переводит ошибку в HTTP-ответ. Внутренние детали не уходят клиенту:
// для неизвестных ошибок клиент видит только "Internal error", а причина пишется в лог.
func WriteError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	status, detail := http.StatusInternalServerError, "Internal error"
	if appErr, ok := apperr.As(err); ok {
		status, detail = statusFor(appErr.Kind), appErr.Message
	}
	if status >= http.StatusInternalServerError {
		logger.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "status", status, "error", err)
	}
	WriteJSON(w, status, ErrorBody{Detail: detail})
}

func statusFor(kind apperr.Kind) int {
	switch kind {
	case apperr.KindInvalid:
		return http.StatusUnprocessableEntity
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindUnavailable:
		return http.StatusServiceUnavailable
	case apperr.KindInternal:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

// DecodeJSON читает тело запроса в dst. Ошибки разбора — это ошибки клиента (422).
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return apperr.Wrap(apperr.KindInvalid, "Request body is too large", err)
		}
		return apperr.Wrap(apperr.KindInvalid, fmt.Sprintf("Invalid JSON body: %v", err), err)
	}
	return nil
}
