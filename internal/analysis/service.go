package analysis

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"ytcomments/internal/logging"
)

// Интерфейсы объявлены здесь, у потребителя, а не рядом с реализацией.
// В Go интерфейсы реализуются неявно (structural typing, как Protocol в Python):
// PostgresRepository не пишет «implements TaskStore», ему достаточно иметь нужные методы.
// Поэтому в тестах их подменяют простыми фейками без моков.

// TaskStore хранит задачи.
// Get возвращает ErrTaskNotFound, если задачи нет, и ErrStorageUnavailable, если хранилище недоступно.
type TaskStore interface {
	Save(ctx context.Context, task *Task) error
	Get(ctx context.Context, id uuid.UUID) (*Task, error)
}

// TaskQueue ставит задачу анализа в очередь фоновой обработки.
type TaskQueue interface {
	EnqueueAnalysis(ctx context.Context, id uuid.UUID) error
}

// staleProcessingAfter — после этого времени без обновлений задача в processing
// считается потерянной. Нужна для одного редкого случая: воркер убит (kill -9, OOM)
// на последней попытке. asynq тогда архивирует задачу, не вызывая наш код, и
// задача навсегда осталась бы в processing. Значение больше TaskTimeout с запасом
// на возврат задачи по истечении аренды и паузу перед повтором.
const staleProcessingAfter = TaskTimeout + 5*time.Minute

// Service — сторона API: поставить задачу и узнать её статус.
// Ничего не знает ни про HTTP, ни про базу и Redis.
type Service struct {
	store  TaskStore
	queue  TaskQueue
	logger *slog.Logger
	now    func() time.Time
}

// NewService собирает сервис из зависимостей.
func NewService(store TaskStore, queue TaskQueue, logger *slog.Logger) *Service {
	return &Service{store: store, queue: queue, logger: logger, now: time.Now}
}

// Start создаёт задачу в статусе pending и ставит её в очередь.
func (s *Service) Start(ctx context.Context, videoID string) (*Task, error) {
	now := s.now().UTC()
	task := &Task{
		ID:        uuid.New(),
		VideoID:   videoID,
		Status:    StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	ctx = logging.With(ctx, "task_id", task.ID)

	if err := s.store.Save(ctx, task); err != nil {
		return nil, err
	}
	s.logger.InfoContext(ctx, "task created", "video_id", videoID)

	if err := s.queue.EnqueueAnalysis(ctx, task.ID); err != nil {
		return nil, err
	}
	// Нет "task received" в логах воркера после этой строки — воркер не слушает очередь.
	s.logger.InfoContext(ctx, "task enqueued", "queue", QueueName)
	return task, nil
}

// Get возвращает задачу или ErrTaskNotFound.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Task, error) {
	task, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if task.Status == StatusProcessing && s.now().Sub(task.UpdatedAt) > staleProcessingAfter {
		// Только для ответа: GET ничего не пишет в базу.
		task.Status = StatusFailed
		task.Error = msgProcessingTimeout
	}
	return task, nil
}
