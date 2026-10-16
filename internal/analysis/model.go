// Package analysis — домен «анализ комментариев к видео».
//
// В пакете всё, что относится к домену: модели, сервис (сторона API), анализатор
// (сторона воркера), хранилище, очередь и HTTP-хендлер. Это организация «по доменам»:
// новый домен — новый пакет рядом, а не новые файлы в handlers/, services/, models/.
//
// Внутри пакета зависимости всё равно направлены в одну сторону:
// handler → Service/Analyzer → интерфейсы (TaskStore, TaskQueue, CommentSource, Classifier).
// Конкретные реализации (Redis, asynq, YouTube, LLM) подставляет main.
package analysis

import (
	"slices"
	"time"

	"github.com/google/uuid"

	"ytcomments/internal/apperr"
)

// Status — статус задачи. В Go нет enum: принято объявлять именованный тип и константы.
type Status string

// Статусы задачи: pending → processing → completed | failed.
const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// Final — задача завершена и больше не изменится.
func (s Status) Final() bool { return s == StatusCompleted || s == StatusFailed }

// Category — категория комментария. Строки совпадают с Python-версией и фронтендом.
type Category string

// Категории комментариев.
const (
	CategoryVideoSuggestion Category = "Video suggestion"
	CategoryQuestions       Category = "Questions"
	CategoryThanks          Category = "Thanks"
	CategoryAdvice          Category = "Advice/tips"
	CategoryCorrections     Category = "Corrections"
	CategorySummaries       Category = "Summaries"
	CategoryBots            Category = "Bots"
	CategoryFeedback        Category = "Feedback"
)

// Categories — все категории в порядке объявления (для промпта и JSON-схемы).
var Categories = []Category{
	CategoryVideoSuggestion, CategoryQuestions, CategoryThanks, CategoryAdvice,
	CategoryCorrections, CategorySummaries, CategoryBots, CategoryFeedback,
}

// Valid — значение входит в список категорий.
func (c Category) Valid() bool { return slices.Contains(Categories, c) }

// CommentOut — комментарий в результате анализа.
type CommentOut struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Result — комментарии, сгруппированные по категориям. Пустые категории отсутствуют.
type Result map[Category][]CommentOut

// Classification — ответ классификатора для одного комментария.
type Classification struct {
	ID        string   `json:"id"`
	Category  Category `json:"category"`
	Reasoning string   `json:"reasoning"`
}

// Task — запись о задаче. Хранится в Redis как JSON; формат совместим с Python-версией.
type Task struct {
	ID        uuid.UUID `json:"task_id"`
	VideoID   string    `json:"video_id"`
	Status    Status    `json:"status"`
	Result    Result    `json:"result"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Доменные ошибки. Вид ошибки (а не HTTP-код!) задаётся здесь, код выбирает httpx.
var (
	ErrTaskNotFound       = apperr.New(apperr.KindNotFound, "Task not found")
	ErrInvalidVideoURL    = apperr.New(apperr.KindInvalid, "Not a valid YouTube video URL")
	ErrInvalidTaskID      = apperr.New(apperr.KindInvalid, "Invalid task_id: must be a UUID")
	ErrStorageUnavailable = apperr.New(apperr.KindUnavailable, "Storage is unavailable")
)

// Сообщения для пользователя у упавших задач.
const (
	msgInternalError     = "Internal error"
	msgProcessingTimeout = "Processing timed out"
)
