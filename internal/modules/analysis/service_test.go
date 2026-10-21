package analysis_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"ytcomments/internal/core/logging"
	"ytcomments/internal/modules/analysis"
	"ytcomments/internal/modules/analysis/analysistest"
)

func TestServiceStart(t *testing.T) {
	store, queue := analysistest.NewStore(), &analysistest.Queue{}
	svc := analysis.NewService(store, queue, logging.Discard())

	task, err := svc.Start(t.Context(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if diff := cmp.Diff([]uuid.UUID{task.ID}, queue.Enqueued()); diff != "" {
		t.Errorf("enqueued mismatch (-want +got):\n%s", diff)
	}
	saved, err := store.Get(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("task not saved: %v", err)
	}
	if saved.Status != analysis.StatusPending || saved.VideoID != "dQw4w9WgXcQ" {
		t.Errorf("saved task = %+v, want pending for dQw4w9WgXcQ", saved)
	}
}

func TestServiceStartPropagatesQueueError(t *testing.T) {
	queueErr := errors.New("queue is down")
	svc := analysis.NewService(analysistest.NewStore(), &analysistest.Queue{Err: queueErr}, logging.Discard())

	if _, err := svc.Start(t.Context(), "dQw4w9WgXcQ"); !errors.Is(err, queueErr) {
		t.Fatalf("Start error = %v, want %v", err, queueErr)
	}
}

func TestServiceGet(t *testing.T) {
	store := analysistest.NewStore()
	svc := analysis.NewService(store, &analysistest.Queue{}, logging.Discard())

	t.Run("unknown task", func(t *testing.T) {
		if _, err := svc.Get(t.Context(), uuid.New()); !errors.Is(err, analysis.ErrTaskNotFound) {
			t.Fatalf("Get error = %v, want ErrTaskNotFound", err)
		}
	})

	t.Run("processing task", func(t *testing.T) {
		task := saveTask(t, store, analysis.StatusProcessing, time.Now())
		got, err := svc.Get(t.Context(), task.ID)
		if err != nil || got.Status != analysis.StatusProcessing {
			t.Fatalf("Get = %+v, %v; want processing", got, err)
		}
	})

	t.Run("stale processing task is reported as failed", func(t *testing.T) {
		task := saveTask(t, store, analysis.StatusProcessing, time.Now().Add(-time.Hour))
		got, err := svc.Get(t.Context(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != analysis.StatusFailed || got.Error != "Processing timed out" {
			t.Fatalf("Get = %+v; want failed with timeout message", got)
		}
	})
}

func saveTask(t *testing.T, store *analysistest.Store, status analysis.Status, updated time.Time) *analysis.Task {
	t.Helper() // при падении тест укажет на строку вызывающего, а не на эту
	task := &analysis.Task{ID: uuid.New(), VideoID: "dQw4w9WgXcQ", Status: status, UpdatedAt: updated}
	if err := store.Save(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestAnalyzerRun(t *testing.T) {
	comments := []analysis.Comment{{ID: "c1", Text: "thanks!"}, {ID: "c2", Text: "thank you"}}
	transient := errors.New("connection reset")

	tests := []struct {
		name        string
		source      *analysistest.Source
		classifier  *analysistest.Classifier
		lastAttempt bool

		wantErr     error // nil — успех; проверяется через errors.Is
		wantHistory []analysis.Status
		wantError   string // поле Task.Error
		wantResult  analysis.Result
	}{
		{
			name:        "success",
			source:      &analysistest.Source{Comments: comments},
			classifier:  &analysistest.Classifier{},
			wantHistory: []analysis.Status{analysis.StatusPending, analysis.StatusProcessing, analysis.StatusCompleted},
			wantResult: analysis.Result{analysis.CategoryThanks: {
				{ID: "c1", Text: "thanks!"}, {ID: "c2", Text: "thank you"},
			}},
		},
		{
			name:        "no comments: completed with empty result",
			source:      &analysistest.Source{},
			classifier:  &analysistest.Classifier{Err: errors.New("must not be called")},
			wantHistory: []analysis.Status{analysis.StatusPending, analysis.StatusProcessing, analysis.StatusCompleted},
			wantResult:  analysis.Result{},
		},
		{
			name: "comment source error fails immediately without retry",
			source: &analysistest.Source{Err: fmt.Errorf("wrapped: %w",
				&analysis.CommentSourceError{Message: "Video not found", Err: errors.New("http 404")})},
			classifier:  &analysistest.Classifier{},
			wantErr:     analysis.ErrPermanent,
			wantHistory: []analysis.Status{analysis.StatusPending, analysis.StatusProcessing, analysis.StatusFailed},
			wantError:   "Video not found",
		},
		{
			name:        "transient error with attempts left stays processing",
			source:      &analysistest.Source{Comments: comments},
			classifier:  &analysistest.Classifier{Err: transient},
			wantErr:     transient,
			wantHistory: []analysis.Status{analysis.StatusPending, analysis.StatusProcessing},
		},
		{
			name:        "transient error on last attempt fails with generic message",
			source:      &analysistest.Source{Comments: comments},
			classifier:  &analysistest.Classifier{Err: transient},
			lastAttempt: true,
			wantErr:     transient,
			wantHistory: []analysis.Status{analysis.StatusPending, analysis.StatusProcessing, analysis.StatusFailed},
			wantError:   "Internal error", // детали ошибки пользователю не показываем
		},
		{
			name:        "timeout on last attempt",
			source:      &analysistest.Source{Err: context.DeadlineExceeded},
			classifier:  &analysistest.Classifier{},
			lastAttempt: true,
			wantErr:     context.DeadlineExceeded,
			wantHistory: []analysis.Status{analysis.StatusPending, analysis.StatusProcessing, analysis.StatusFailed},
			wantError:   "Processing timed out",
		},
		{
			name:        "shutdown never marks the task failed",
			source:      &analysistest.Source{Err: context.Canceled},
			classifier:  &analysistest.Classifier{},
			lastAttempt: true,
			wantErr:     context.Canceled,
			wantHistory: []analysis.Status{analysis.StatusPending, analysis.StatusProcessing},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := analysistest.NewStore()
			task := saveTask(t, store, analysis.StatusPending, time.Now())
			analyzer := analysis.NewAnalyzer(store, tt.source, tt.classifier, logging.Discard())

			err := analyzer.Run(t.Context(), task.ID, tt.lastAttempt)

			if tt.wantErr == nil && err != nil {
				t.Fatalf("Run error = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run error = %v, want %v", err, tt.wantErr)
			}
			if diff := cmp.Diff(tt.wantHistory, store.History(task.ID)); diff != "" {
				t.Errorf("status history mismatch (-want +got):\n%s", diff)
			}
			got, _ := store.Get(t.Context(), task.ID)
			if got.Error != tt.wantError {
				t.Errorf("task error = %q, want %q", got.Error, tt.wantError)
			}
			if diff := cmp.Diff(tt.wantResult, got.Result); diff != "" {
				t.Errorf("result mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAnalyzerSkipsMissingAndFinishedTasks(t *testing.T) {
	store := analysistest.NewStore()
	classifier := &analysistest.Classifier{}
	source := &analysistest.Source{Comments: []analysis.Comment{{ID: "1", Text: "hi"}}}
	analyzer := analysis.NewAnalyzer(store, source, classifier, logging.Discard())

	if err := analyzer.Run(t.Context(), uuid.New(), false); err != nil {
		t.Errorf("missing task: Run error = %v, want nil (nothing to retry)", err)
	}

	done := saveTask(t, store, analysis.StatusCompleted, time.Now())
	if err := analyzer.Run(t.Context(), done.ID, false); err != nil {
		t.Errorf("completed task: Run error = %v, want nil", err)
	}
	if classifier.Calls() != 0 {
		t.Errorf("classifier called %d times, want 0", classifier.Calls())
	}
	if diff := cmp.Diff([]analysis.Status{analysis.StatusCompleted}, store.History(done.ID)); diff != "" {
		t.Errorf("finished task was modified (-want +got):\n%s", diff)
	}
}
