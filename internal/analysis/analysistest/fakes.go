// Package analysistest — фейки интерфейсов пакета analysis для тестов
// (по образцу стандартного net/http/httptest).
//
// Это не моки: фейк — простая рабочая реализация в памяти. Тест проверяет
// результат (что лежит в хранилище, что попало в очередь), а не последовательность вызовов.
// Все фейки безопасны для горутин: в e2e-тесте их вызывает воркер asynq.
package analysistest

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"ytcomments/internal/analysis"
	"ytcomments/internal/youtube"
)

// Store — TaskStore в памяти. Запоминает историю статусов каждой задачи.
type Store struct {
	mu      sync.Mutex
	tasks   map[uuid.UUID]analysis.Task
	history map[uuid.UUID][]analysis.Status
	// SaveErr, если задан, возвращается из Save.
	SaveErr error
}

// NewStore создаёт пустое хранилище.
func NewStore() *Store {
	return &Store{tasks: map[uuid.UUID]analysis.Task{}, history: map[uuid.UUID][]analysis.Status{}}
}

// Save реализует analysis.TaskStore. В отличие от Redis-хранилища, UpdatedAt не трогает:
// так тест может положить «старую» задачу.
func (s *Store) Save(_ context.Context, task *analysis.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SaveErr != nil {
		return s.SaveErr
	}
	s.tasks[task.ID] = *task // копия: дальнейшие изменения task не влияют на «хранилище»
	s.history[task.ID] = append(s.history[task.ID], task.Status)
	return nil
}

// Get реализует analysis.TaskStore.
func (s *Store) Get(_ context.Context, id uuid.UUID) (*analysis.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, analysis.ErrTaskNotFound
	}
	return &task, nil
}

// History — последовательность сохранённых статусов задачи.
func (s *Store) History(id uuid.UUID) []analysis.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]analysis.Status(nil), s.history[id]...)
}

// Queue — TaskQueue, запоминающая поставленные задачи.
type Queue struct {
	mu  sync.Mutex
	ids []uuid.UUID
	// Err, если задан, возвращается из EnqueueAnalysis.
	Err error
}

// EnqueueAnalysis реализует analysis.TaskQueue.
func (q *Queue) EnqueueAnalysis(_ context.Context, id uuid.UUID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.Err != nil {
		return q.Err
	}
	q.ids = append(q.ids, id)
	return nil
}

// Enqueued — ID поставленных задач.
func (q *Queue) Enqueued() []uuid.UUID {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]uuid.UUID(nil), q.ids...)
}

// Source — CommentSource с заранее заданным ответом.
type Source struct {
	Comments []youtube.Comment
	Err      error
}

// FetchComments реализует analysis.CommentSource.
func (s *Source) FetchComments(context.Context, string) ([]youtube.Comment, error) {
	return s.Comments, s.Err
}

// Classifier относит все комментарии к одной категории (по умолчанию Thanks).
type Classifier struct {
	Category analysis.Category
	Err      error

	mu    sync.Mutex
	calls int
}

// Classify реализует analysis.Classifier.
func (c *Classifier) Classify(_ context.Context, comments []youtube.Comment) ([]analysis.Classification, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.Err != nil {
		return nil, c.Err
	}
	category := c.Category
	if category == "" {
		category = analysis.CategoryThanks
	}
	out := make([]analysis.Classification, len(comments))
	for i, comment := range comments {
		out[i] = analysis.Classification{ID: comment.ID, Category: category, Reasoning: "test"}
	}
	return out, nil
}

// Calls — сколько раз вызывался Classify.
func (c *Classifier) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}
