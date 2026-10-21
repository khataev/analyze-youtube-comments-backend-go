package analysis_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"ytcomments/internal/analysis"
	"ytcomments/internal/apperr"
	"ytcomments/internal/postgres"
	"ytcomments/internal/postgres/pgtest"
)

// Тесты работают с настоящим Postgres (база analysis_go_test), каждый — в своей
// транзакции, которая откатывается в конце: тесты не видят данных друг друга.

func newTask(status analysis.Status) *analysis.Task {
	return &analysis.Task{
		ID:        uuid.New(),
		VideoID:   "dQw4w9WgXcQ",
		Status:    status,
		CreatedAt: time.Now().UTC(),
	}
}

func TestPostgresRepositoryRoundTrip(t *testing.T) {
	repo := analysis.NewPostgresRepository(pgtest.Tx(t))
	completed := newTask(analysis.StatusCompleted)
	completed.Result = analysis.Result{
		analysis.CategoryThanks:    {{ID: "1", Text: "thanks"}, {ID: "2", Text: "спасибо"}},
		analysis.CategoryQuestions: {{ID: "3", Text: `how? "quoted" <b>`}},
	}
	failed := newTask(analysis.StatusFailed)
	failed.Error = "Video not found"
	empty := newTask(analysis.StatusCompleted)
	empty.Result = analysis.Result{} // комментариев нет: {} в базе, а не NULL

	for name, task := range map[string]*analysis.Task{"completed": completed, "failed": failed, "empty": empty} {
		t.Run(name, func(t *testing.T) {
			if err := repo.Save(t.Context(), task); err != nil {
				t.Fatal(err)
			}
			got, err := repo.Get(t.Context(), task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(task, got); diff != "" {
				t.Errorf("round trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPostgresRepositoryUpdatesExistingTask(t *testing.T) {
	tx := pgtest.Tx(t)
	repo := analysis.NewPostgresRepository(tx)
	task := newTask(analysis.StatusPending)
	if err := repo.Save(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	createdAt, firstUpdate := task.CreatedAt, task.UpdatedAt

	task.Status, task.Result = analysis.StatusCompleted, analysis.Result{analysis.CategoryBots: {{ID: "b", Text: "buy"}}}
	if err := repo.Save(t.Context(), task); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != analysis.StatusCompleted || !got.CreatedAt.Equal(createdAt) || !got.UpdatedAt.After(firstUpdate) {
		t.Errorf("after update: status %s, created_at %v (want %v), updated_at %v (want after %v)",
			got.Status, got.CreatedAt, createdAt, got.UpdatedAt, firstUpdate)
	}
	var rows int
	if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM analysis_tasks WHERE id = $1", task.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("rows for task = %d, want 1 (upsert, not insert)", rows)
	}
}

// Формат колонок совпадает с Python-версией: строку, записанную ей, читает Go, и наоборот.
func TestPostgresRepositoryStorageFormat(t *testing.T) {
	tx := pgtest.Tx(t)
	repo := analysis.NewPostgresRepository(tx)

	t.Run("written by Go", func(t *testing.T) {
		task := newTask(analysis.StatusCompleted)
		task.Result = analysis.Result{analysis.CategoryThanks: {{ID: "1", Text: "thanks"}}}
		if err := repo.Save(t.Context(), task); err != nil {
			t.Fatal(err)
		}
		var status, text string
		var errorIsNull bool
		err := tx.QueryRow(t.Context(),
			`SELECT status, result->'Thanks'->0->>'text', error IS NULL FROM analysis_tasks WHERE id = $1`,
			task.ID).Scan(&status, &text, &errorIsNull)
		if err != nil {
			t.Fatal(err)
		}
		if status != "completed" || text != "thanks" || !errorIsNull {
			t.Errorf("stored status=%q text=%q error IS NULL=%v", status, text, errorIsNull)
		}
	})

	t.Run("written by Python", func(t *testing.T) {
		id := uuid.New()
		_, err := tx.Exec(t.Context(), `INSERT INTO analysis_tasks VALUES
			($1, 'dQw4w9WgXcQ', 'failed', NULL, 'Video not found', now(), now())`, id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := repo.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != analysis.StatusFailed || got.Error != "Video not found" || got.Result != nil {
			t.Errorf("got %+v", got)
		}
	})
}

func TestPostgresRepositoryNotFound(t *testing.T) {
	repo := analysis.NewPostgresRepository(pgtest.Tx(t))
	if _, err := repo.Get(t.Context(), uuid.New()); !errors.Is(err, analysis.ErrTaskNotFound) {
		t.Errorf("error = %v, want ErrTaskNotFound", err)
	}
}

func TestPostgresRepositoryUnavailable(t *testing.T) {
	// Пул к закрытому порту — «база упала». Соединение открывается при первом запросе.
	pool, err := postgres.NewPool("postgres://app:app@127.0.0.1:1/x?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repo := analysis.NewPostgresRepository(pool)

	_, getErr := repo.Get(t.Context(), uuid.New())
	saveErr := repo.Save(t.Context(), newTask(analysis.StatusPending))
	for op, err := range map[string]error{"get": getErr, "save": saveErr} {
		if appErr, ok := apperr.As(err); !ok || appErr.Kind != apperr.KindUnavailable {
			t.Errorf("%s: error = %v, want KindUnavailable", op, err)
		}
	}
}

// Ошибка самого запроса — не недоступность: клиент получит 500, а не 503.
func TestPostgresRepositoryQueryErrorIsInternal(t *testing.T) {
	repo := analysis.NewPostgresRepository(pgtest.Tx(t))
	task := newTask(analysis.StatusPending)
	task.VideoID = "longer-than-eleven-chars" // колонка varchar(11)

	err := repo.Save(t.Context(), task)
	if err == nil {
		t.Fatal("want an error for a value too long")
	}
	if appErr, ok := apperr.As(err); ok && appErr.Kind == apperr.KindUnavailable {
		t.Errorf("error = %v; a query error must not be reported as unavailable", err)
	}
}
