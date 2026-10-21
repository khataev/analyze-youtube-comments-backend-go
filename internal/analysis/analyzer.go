package analysis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"ytcomments/internal/logging"
)

// CommentSource загружает комментарии к видео.
type CommentSource interface {
	FetchComments(ctx context.Context, videoID string) ([]Comment, error)
}

// Classifier относит каждый комментарий к одной из Categories.
type Classifier interface {
	Classify(ctx context.Context, comments []Comment) ([]Classification, error)
}

// ErrPermanent оборачивает ошибки, после которых повтор бессмыслен
// (видео не найдено, комментарии отключены). Задача уже помечена failed.
var ErrPermanent = errors.New("permanent failure")

// failureSaveTimeout — сколько ждать сохранения статуса failed, если context задачи уже отменён.
const failureSaveTimeout = 5 * time.Second

// Analyzer — сторона воркера: сама обработка задачи.
// Про asynq не знает: о том, последняя ли это попытка, ему сообщает вызывающий.
type Analyzer struct {
	store      TaskStore
	source     CommentSource
	classifier Classifier
	logger     *slog.Logger
}

// NewAnalyzer собирает анализатор из зависимостей.
func NewAnalyzer(store TaskStore, source CommentSource, classifier Classifier, logger *slog.Logger) *Analyzer {
	return &Analyzer{store: store, source: source, classifier: classifier, logger: logger}
}

// Run обрабатывает задачу. lastAttempt — последняя ли это попытка.
//
// Контракт с очередью:
//   - nil — задача завершена (completed или failed по доменной причине — тогда
//     возвращается ErrPermanent) или её больше нет;
//   - другая ошибка — временная, задачу стоит повторить. На последней попытке
//     задача перед этим помечается failed, так что в processing она не зависнет.
func (a *Analyzer) Run(ctx context.Context, id uuid.UUID, lastAttempt bool) error {
	ctx = logging.With(ctx, "task_id", id)

	task, err := a.store.Get(ctx, id)
	if errors.Is(err, ErrTaskNotFound) {
		a.logger.WarnContext(ctx, "task not found (expired?), skipping")
		return nil
	}
	if err != nil {
		return fmt.Errorf("load task: %w", err)
	}
	if task.Status.Final() {
		// Очередь доставляет «как минимум один раз»: повторная доставка уже готовой задачи — норма.
		a.logger.InfoContext(ctx, "task already finished, skipping", "status", task.Status)
		return nil
	}

	task.Status, task.Error = StatusProcessing, ""
	if err := a.store.Save(ctx, task); err != nil {
		return fmt.Errorf("mark processing: %w", err)
	}
	a.logger.InfoContext(ctx, "processing video", "video_id", task.VideoID)
	started := time.Now()

	result, err := a.process(ctx, task.VideoID)
	if err != nil {
		return a.handleFailure(ctx, task, err, lastAttempt)
	}

	a.logger.InfoContext(ctx, "step 3/3: saving result")
	task.Status, task.Result = StatusCompleted, result
	if err := a.store.Save(ctx, task); err != nil {
		return a.handleFailure(ctx, task, fmt.Errorf("save result: %w", err), lastAttempt)
	}
	a.logger.InfoContext(ctx, "task completed",
		"duration", time.Since(started).Round(time.Millisecond), "categories", countByCategory(result))
	return nil
}

func (a *Analyzer) process(ctx context.Context, videoID string) (Result, error) {
	a.logger.InfoContext(ctx, "step 1/3: fetching comments from YouTube")
	comments, err := a.source.FetchComments(ctx, videoID)
	if err != nil {
		return nil, fmt.Errorf("fetch comments: %w", err)
	}
	a.logger.InfoContext(ctx, "comments fetched", "count", len(comments))

	var classifications []Classification
	if len(comments) > 0 {
		a.logger.InfoContext(ctx, "step 2/3: classifying with LLM")
		if classifications, err = a.classifier.Classify(ctx, comments); err != nil {
			return nil, fmt.Errorf("classify: %w", err)
		}
		a.logger.InfoContext(ctx, "classifications received", "count", len(classifications))
	}
	return GroupByCategory(comments, classifications), nil
}

func (a *Analyzer) handleFailure(ctx context.Context, task *Task, err error, lastAttempt bool) error {
	var sourceErr *CommentSourceError
	switch {
	case errors.As(err, &sourceErr):
		a.logger.WarnContext(ctx, "task failed: comment source error", "error", err)
		a.markFailed(ctx, task, sourceErr.Message)
		return fmt.Errorf("%w: %w", ErrPermanent, err)

	case errors.Is(err, context.Canceled):
		// Воркер останавливается: asynq вернёт задачу в очередь, failed ставить нельзя.
		a.logger.WarnContext(ctx, "task interrupted, will be re-queued", "error", err)
		return err

	case lastAttempt:
		message := msgInternalError
		if errors.Is(err, context.DeadlineExceeded) {
			message = msgProcessingTimeout
		}
		a.logger.ErrorContext(ctx, "task failed, no attempts left", "error", err)
		a.markFailed(ctx, task, message)
		return err

	default:
		a.logger.WarnContext(ctx, "attempt failed, will retry", "error", err)
		return err
	}
}

// markFailed сохраняет статус failed даже если context задачи уже отменён
// (таймаут задачи): context.WithoutCancel сохраняет значения (task_id для логов),
// но не отмену.
func (a *Analyzer) markFailed(ctx context.Context, task *Task, message string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureSaveTimeout)
	defer cancel()

	task.Status, task.Error, task.Result = StatusFailed, message, nil
	if err := a.store.Save(ctx, task); err != nil {
		a.logger.ErrorContext(ctx, "could not save failed status", "error", err)
	}
}

func countByCategory(result Result) map[Category]int {
	counts := make(map[Category]int, len(result))
	for category, items := range result {
		counts[category] = len(items)
	}
	return counts
}
