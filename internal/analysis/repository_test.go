package analysis_test

import (
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"ytcomments/internal/analysis"
	"ytcomments/internal/apperr"
)

// miniredis — Redis-сервер в памяти процесса (аналог fakeredis), внешний Redis не нужен.
func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t) // остановится сам в конце теста
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() }) // аналог teardown / yield-фикстуры
	return mr, rdb
}

func TestRedisRepositoryRoundTrip(t *testing.T) {
	mr, rdb := newRedis(t)
	repo := analysis.NewRedisRepository(rdb, time.Hour)
	task := &analysis.Task{
		ID:        uuid.New(),
		VideoID:   "dQw4w9WgXcQ",
		Status:    analysis.StatusCompleted,
		Result:    analysis.Result{analysis.CategoryThanks: {{ID: "1", Text: "thanks"}}},
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}

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
	key := "analysis:task:" + task.ID.String()
	if ttl := mr.TTL(key); ttl != time.Hour {
		t.Errorf("TTL of %s = %v, want 1h", key, ttl)
	}
}

func TestRedisRepositoryReadsPythonRecord(t *testing.T) {
	// Формат хранения совместим с Python-версией: одну базу могут читать обе.
	mr, rdb := newRedis(t)
	id := uuid.MustParse("2f1a6a49-4c8e-4a0e-9a2b-8f6a1f0d2c11")
	python := `{"task_id":"2f1a6a49-4c8e-4a0e-9a2b-8f6a1f0d2c11","video_id":"dQw4w9WgXcQ",` +
		`"status":"failed","result":null,"error":"Video not found",` +
		`"created_at":"2026-10-16T10:00:00.123456Z","updated_at":"2026-10-16T10:00:01.5Z"}`
	if err := mr.Set("analysis:task:"+id.String(), python); err != nil {
		t.Fatal(err)
	}

	got, err := analysis.NewRedisRepository(rdb, time.Hour).Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != analysis.StatusFailed || got.Error != "Video not found" || got.Result != nil {
		t.Errorf("got %+v", got)
	}
}

func TestRedisRepositoryErrors(t *testing.T) {
	mr, rdb := newRedis(t)
	repo := analysis.NewRedisRepository(rdb, time.Hour)

	if _, err := repo.Get(t.Context(), uuid.New()); !errors.Is(err, analysis.ErrTaskNotFound) {
		t.Errorf("missing key: error = %v, want ErrTaskNotFound", err)
	}

	mr.Close() // Redis «упал»
	_, err := repo.Get(t.Context(), uuid.New())
	if appErr, ok := apperr.As(err); !ok || appErr.Kind != apperr.KindUnavailable {
		t.Errorf("redis down: error = %v, want KindUnavailable", err)
	}
	err = repo.Save(t.Context(), &analysis.Task{ID: uuid.New()})
	if appErr, ok := apperr.As(err); !ok || appErr.Kind != apperr.KindUnavailable {
		t.Errorf("redis down on save: error = %v, want KindUnavailable", err)
	}
}
