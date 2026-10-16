package analysis

import (
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"ytcomments/internal/httpx"
)

// Handler — HTTP-слой домена: разобрать запрос, вызвать сервис, отдать JSON.
// Бизнес-логики здесь нет.
type Handler struct {
	service *Service
	logger  *slog.Logger
}

// NewHandler создаёт хендлер.
func NewHandler(service *Service, logger *slog.Logger) *Handler {
	return &Handler{service: service, logger: logger}
}

// Register добавляет маршруты домена. Паттерны с методом и {параметром} —
// возможность стандартного http.ServeMux начиная с Go 1.22.
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("POST "+prefix+"/analysis", h.start)
	mux.HandleFunc("GET "+prefix+"/analysis/{task_id}", h.get)
}

// DTO API отделены от Task: формат ответа не меняется, если меняется формат хранения.

type startRequest struct {
	URL string `json:"url"`
}

type startResponse struct {
	TaskID uuid.UUID `json:"task_id"`
}

type statusResponse struct {
	TaskID uuid.UUID `json:"task_id"`
	Status Status    `json:"status"`
	Result Result    `json:"result"` // nil → null, как в Python-версии
	Error  *string   `json:"error"`  // указатель: пустая строка → null
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	videoID, err := ParseVideoID(req.URL)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	task, err := h.service.Start(r.Context(), videoID)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, startResponse{TaskID: task.ID})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("task_id"))
	if err != nil {
		httpx.WriteError(w, r, h.logger, ErrInvalidTaskID)
		return
	}
	task, err := h.service.Get(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, h.logger, err)
		return
	}
	resp := statusResponse{TaskID: task.ID, Status: task.Status, Result: task.Result}
	if task.Error != "" {
		resp.Error = &task.Error
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}
