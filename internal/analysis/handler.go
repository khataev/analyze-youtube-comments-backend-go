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

// Схемы API отделены от доменных Task и Comment: формат ответа — контракт с фронтендом,
// он не должен меняться вместе с доменом или хранилищем. Имена — по роли, единым
// шаблоном <что><Request|Response> (как AnalyzeRequest / TaskStatusResponse в Python).
// Типы неэкспортируемые: за пределами хендлера они никому не нужны.

type analyzeRequest struct {
	URL string `json:"url"`
}

type taskCreatedResponse struct {
	TaskID uuid.UUID `json:"task_id"`
}

type taskStatusResponse struct {
	TaskID uuid.UUID                      `json:"task_id"`
	Status Status                         `json:"status"`
	Result map[Category][]commentResponse `json:"result"` // nil → null, как в Python-версии
	Error  *string                        `json:"error"`  // указатель: пустая строка → null
}

type commentResponse struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func newTaskStatusResponse(task *Task) taskStatusResponse {
	resp := taskStatusResponse{TaskID: task.ID, Status: task.Status}
	if task.Result != nil { // результата нет (ещё не готов или задача упала) — null
		resp.Result = make(map[Category][]commentResponse, len(task.Result))
		for category, comments := range task.Result {
			items := make([]commentResponse, len(comments))
			for i, c := range comments {
				items[i] = commentResponse(c) // поля совпадают; появится новое в домене — компилятор заставит решить, отдавать ли его
			}
			resp.Result[category] = items
		}
	}
	if task.Error != "" {
		resp.Error = &task.Error
	}
	return resp
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	var req analyzeRequest
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
	httpx.WriteJSON(w, http.StatusAccepted, taskCreatedResponse{TaskID: task.ID})
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
	httpx.WriteJSON(w, http.StatusOK, newTaskStatusResponse(task))
}
