package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"ytcomments/internal/analysis"
	"ytcomments/internal/analysis/analysistest"
	"ytcomments/internal/httpapi"
	"ytcomments/internal/logging"
)

// testApp — приложение целиком: настоящие роутер, хендлер, сервис и Redis-хранилище
// (на miniredis), подменена только очередь.
type testApp struct {
	server *httptest.Server
	redis  *miniredis.Miniredis
	repo   *analysis.RedisRepository
	queue  *analysistest.Queue
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	logger := logging.Discard()
	repo := analysis.NewRedisRepository(rdb, time.Hour)
	queue := &analysistest.Queue{}
	router := httpapi.NewRouter(httpapi.Deps{
		Analysis:    analysis.NewHandler(analysis.NewService(repo, queue, logger), logger),
		Storage:     httpapi.PingerFunc(func(ctx context.Context) error { return rdb.Ping(ctx).Err() }),
		CORSOrigins: []string{"http://localhost:5173"},
		Logger:      logger,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &testApp{server: srv, redis: mr, repo: repo, queue: queue}
}

// do выполняет запрос и возвращает статус и тело, разобранное как JSON.
func (a *testApp) do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, a.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("%s %s: response is not JSON: %q", method, path, raw)
		}
	}
	return resp.StatusCode, parsed
}

func TestStartAnalysis(t *testing.T) {
	app := newTestApp(t)

	status, body := app.do(t, http.MethodPost, "/api/v1/analysis", `{"url": "https://youtu.be/dQw4w9WgXcQ"}`)
	if status != http.StatusAccepted {
		t.Fatalf("POST status = %d, body %v", status, body)
	}
	taskID, _ := body["task_id"].(string)
	if diff := cmp.Diff([]uuid.UUID{uuid.MustParse(taskID)}, app.queue.Enqueued()); diff != "" {
		t.Errorf("enqueued mismatch (-want +got):\n%s", diff)
	}

	status, body = app.do(t, http.MethodGet, "/api/v1/analysis/"+taskID, "")
	want := map[string]any{"task_id": taskID, "status": "pending", "result": nil, "error": nil}
	if status != http.StatusOK {
		t.Fatalf("GET status = %d", status)
	}
	if diff := cmp.Diff(want, body); diff != "" {
		t.Errorf("GET body mismatch (-want +got):\n%s", diff)
	}
}

func TestStartAnalysisValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not youtube", `{"url": "https://example.com/watch?v=dQw4w9WgXcQ"}`},
		{"not a url", `{"url": "hello"}`},
		{"missing url", `{}`},
		{"url is not a string", `{"url": 42}`},
		{"invalid json", `{"url": `},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(t)
			status, body := app.do(t, http.MethodPost, "/api/v1/analysis", tt.body)
			if status != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, want 422", status)
			}
			if detail, _ := body["detail"].(string); detail == "" {
				t.Errorf("body = %v, want {\"detail\": \"...\"}", body)
			}
			if len(app.queue.Enqueued()) != 0 {
				t.Error("invalid request must not enqueue anything")
			}
		})
	}
}

func TestGetAnalysis(t *testing.T) {
	app := newTestApp(t)
	completed := &analysis.Task{
		ID:      uuid.New(),
		VideoID: "dQw4w9WgXcQ",
		Status:  analysis.StatusCompleted,
		Result:  analysis.Result{analysis.CategoryThanks: {{ID: "1", Text: "thanks!"}}},
	}
	failed := &analysis.Task{ID: uuid.New(), VideoID: "dQw4w9WgXcQ", Status: analysis.StatusFailed, Error: "Video not found"}
	for _, task := range []*analysis.Task{completed, failed} {
		if err := app.repo.Save(t.Context(), task); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   map[string]any
	}{
		{
			name: "completed", path: "/api/v1/analysis/" + completed.ID.String(), wantStatus: http.StatusOK,
			wantBody: map[string]any{
				"task_id": completed.ID.String(), "status": "completed", "error": nil,
				"result": map[string]any{"Thanks": []any{map[string]any{"id": "1", "text": "thanks!"}}},
			},
		},
		{
			name: "failed", path: "/api/v1/analysis/" + failed.ID.String(), wantStatus: http.StatusOK,
			wantBody: map[string]any{
				"task_id": failed.ID.String(), "status": "failed", "result": nil, "error": "Video not found",
			},
		},
		{
			name: "unknown task", path: "/api/v1/analysis/" + uuid.NewString(), wantStatus: http.StatusNotFound,
			wantBody: map[string]any{"detail": "Task not found"},
		},
		{
			name: "malformed task id", path: "/api/v1/analysis/not-a-uuid", wantStatus: http.StatusUnprocessableEntity,
			wantBody: map[string]any{"detail": "Invalid task_id: must be a UUID"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := app.do(t, http.MethodGet, tt.path, "")
			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d", status, tt.wantStatus)
			}
			if diff := cmp.Diff(tt.wantBody, body); diff != "" {
				t.Errorf("body mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStorageUnavailable(t *testing.T) {
	app := newTestApp(t)
	if status, body := app.do(t, http.MethodGet, "/health", ""); status != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("health before outage = %d %v", status, body)
	}

	app.redis.Close() // Redis «упал»
	want := map[string]any{"detail": "Storage is unavailable"}

	for _, req := range []struct{ method, path, body string }{
		{http.MethodGet, "/health", ""},
		{http.MethodGet, "/api/v1/analysis/" + uuid.NewString(), ""},
		{http.MethodPost, "/api/v1/analysis", `{"url": "https://youtu.be/dQw4w9WgXcQ"}`},
	} {
		status, body := app.do(t, req.method, req.path, req.body)
		if status != http.StatusServiceUnavailable {
			t.Errorf("%s %s: status = %d, want 503", req.method, req.path, status)
		}
		if diff := cmp.Diff(want, body); diff != "" {
			t.Errorf("%s %s: body mismatch (-want +got):\n%s", req.method, req.path, diff)
		}
	}
}

func TestQueueUnavailable(t *testing.T) {
	app := newTestApp(t)
	app.queue.Err = errors.New("dial tcp: connection refused") // детали не должны уйти клиенту

	status, body := app.do(t, http.MethodPost, "/api/v1/analysis", `{"url": "https://youtu.be/dQw4w9WgXcQ"}`)
	if status != http.StatusInternalServerError || body["detail"] != "Internal error" {
		t.Errorf("got %d %v, want 500 Internal error", status, body)
	}
}

func TestCORS(t *testing.T) {
	app := newTestApp(t)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodOptions, app.server.URL+"/api/v1/analysis", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	resp, err := app.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "content-type" {
		t.Errorf("Allow-Headers = %q", got)
	}

	req.Header.Set("Origin", "http://evil.test")
	resp, err = app.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unknown origin got Allow-Origin = %q", got)
	}
}
