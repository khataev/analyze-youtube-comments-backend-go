package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"ytcomments/internal/core/logging"
)

// Очередь на asynq — аналог Sidekiq для Go.
//
// Почему задача не теряется при падении воркера: asynq не удаляет сообщение при
// получении (как BRPOP в Python-версии). Lua-скрипт атомарно переносит его в
// список active и выдаёт аренду (lease) на 30 с, которую воркер продлевает, пока
// работает. Если процесс убит, аренда истекает, и recoverer возвращает задачу в очередь.
//
// Про таймауты чтения Redis: asynq не использует блокирующие команды (BRPOP и т.п.) —
// он опрашивает очереди Lua-скриптом и спит TaskCheckInterval, если они пусты.
// Поэтому ReadTimeout go-redis (3 с по умолчанию) не рвёт ожидание, как это было
// с redis-py 8. Тест TestWorkerSurvivesIdleLongerThanReadTimeout в test/e2e это проверяет.

const (
	// QueueName — имя очереди (как в Python-версии).
	QueueName = "analysis"
	// TypeAnalyzeVideo — тип задачи asynq (аналог имени класса Sidekiq-воркера).
	TypeAnalyzeVideo = "analysis:analyze_video"

	// TaskTimeout — сколько может длиться одна попытка обработки.
	TaskTimeout = 10 * time.Minute
	// MaxRetry — число повторов после первой попытки при временных ошибках.
	MaxRetry = 3
	// retryBaseDelay — пауза перед первым повтором, дальше удваивается: 10 с, 20 с, 40 с.
	retryBaseDelay = 10 * time.Second
)

type analyzePayload struct {
	TaskID uuid.UUID `json:"task_id"`
}

// AsynqQueue ставит задачи в asynq. Реализует TaskQueue (сторона API).
type AsynqQueue struct {
	client *asynq.Client
}

// NewAsynqQueue создаёт очередь поверх клиента asynq.
func NewAsynqQueue(client *asynq.Client) *AsynqQueue {
	return &AsynqQueue{client: client}
}

// EnqueueAnalysis ставит задачу анализа в очередь.
func (q *AsynqQueue) EnqueueAnalysis(ctx context.Context, id uuid.UUID) error {
	payload, err := json.Marshal(analyzePayload{TaskID: id})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	task := asynq.NewTask(TypeAnalyzeVideo, payload,
		asynq.Queue(QueueName),
		asynq.TaskID(id.String()), // наш UUID и в asynq: дубликат не поставится, задачу легко найти в asynqmon
		asynq.MaxRetry(MaxRetry),
		asynq.Timeout(TaskTimeout),
	)
	if _, err := q.client.EnqueueContext(ctx, task); err != nil {
		return unavailable(fmt.Errorf("enqueue: %w", err))
	}
	return nil
}

// TaskHandler — обработчик задач asynq на стороне воркера (аналог perform у Sidekiq).
// Переводит контракт Analyzer.Run на язык asynq: попытки, SkipRetry.
type TaskHandler struct {
	analyzer *Analyzer
	logger   *slog.Logger
}

// NewTaskHandler создаёт обработчик.
func NewTaskHandler(analyzer *Analyzer, logger *slog.Logger) *TaskHandler {
	return &TaskHandler{analyzer: analyzer, logger: logger}
}

// ProcessTask реализует asynq.Handler.
func (h *TaskHandler) ProcessTask(ctx context.Context, t *asynq.Task) error {
	var p analyzePayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		// Битое сообщение не починится повтором.
		return fmt.Errorf("decode payload: %w: %w", err, asynq.SkipRetry)
	}
	retried, _ := asynq.GetRetryCount(ctx)
	maxRetry, _ := asynq.GetMaxRetry(ctx)

	h.logger.InfoContext(logging.With(ctx, "task_id", p.TaskID), "task received",
		"attempt", retried+1, "max_attempts", maxRetry+1)

	err := h.analyzer.Run(ctx, p.TaskID, retried >= maxRetry)
	if errors.Is(err, ErrPermanent) {
		return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
	}
	return err
}

// RetryDelay — пауза перед повтором: 10 с, 20 с, 40 с. Для asynq.Config.RetryDelayFunc.
func RetryDelay(retried int, _ error, _ *asynq.Task) time.Duration {
	return retryBaseDelay << retried
}
