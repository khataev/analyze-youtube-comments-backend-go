// Package e2e проверяет путь «API → очередь → воркер» целиком: настоящие HTTP-роутер,
// сервис, Redis-хранилище, очередь и сервер asynq. Redis — miniredis в памяти,
// YouTube и LLM — фейки. Внешние сервисы не нужны.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"ytcomments/internal/analysis"
	"ytcomments/internal/analysis/analysistest"
	"ytcomments/internal/httpapi"
	"ytcomments/internal/logging"
)

// readTimeout намеренно маленький: воркер простаивает дольше него (см. ниже).
const readTimeout = 200 * time.Millisecond

type pipeline struct {
	api *httptest.Server
}

// startPipeline поднимает API и воркер поверх одного Redis.
func startPipeline(t *testing.T, source analysis.CommentSource) *pipeline {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), ReadTimeout: readTimeout})
	t.Cleanup(func() { _ = rdb.Close() })

	logger := logging.Discard()
	repo := analysis.NewRedisRepository(rdb, time.Hour)

	// Сторона API.
	service := analysis.NewService(repo, analysis.NewAsynqQueue(asynq.NewClientFromRedisClient(rdb)), logger)
	api := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Analysis: analysis.NewHandler(service, logger),
		Storage:  httpapi.PingerFunc(func(ctx context.Context) error { return rdb.Ping(ctx).Err() }),
		Logger:   logger,
	}))
	t.Cleanup(api.Close)

	// Сторона воркера — так же, как в cmd/worker, только с фейками YouTube и LLM.
	analyzer := analysis.NewAnalyzer(repo, source, &analysistest.Classifier{}, logger)
	worker := asynq.NewServerFromRedisClient(rdb, asynq.Config{
		Concurrency:       2,
		Queues:            map[string]int{analysis.QueueName: 1},
		TaskCheckInterval: 50 * time.Millisecond,
		Logger:            logging.NewAsynqLogger(logger),
		LogLevel:          asynq.ErrorLevel,
	})
	mux := asynq.NewServeMux()
	mux.Handle(analysis.TypeAnalyzeVideo, analysis.NewTaskHandler(analyzer, logger))
	if err := worker.Start(mux); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(worker.Shutdown) // Cleanup выполняются в обратном порядке: воркер остановится до закрытия Redis
	return &pipeline{api: api}
}

type statusBody struct {
	TaskID string          `json:"task_id"`
	Status string          `json:"status"`
	Result analysis.Result `json:"result"`
	Error  *string         `json:"error"`
}

func (p *pipeline) start(t *testing.T, url string) string {
	t.Helper()
	body := strings.NewReader(fmt.Sprintf(`{"url": %q}`, url))
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, p.api.URL+"/api/v1/analysis", body)
	resp, err := p.api.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d", resp.StatusCode)
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.TaskID
}

// waitFinal опрашивает статус, как это делает фронтенд, пока задача не завершится.
func (p *pipeline) waitFinal(t *testing.T, taskID string) statusBody {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, p.api.URL+"/api/v1/analysis/"+taskID, nil)
		resp, err := p.api.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body statusBody
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if body.Status == string(analysis.StatusCompleted) || body.Status == string(analysis.StatusFailed) {
			return body
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach a final status in time", taskID)
	return statusBody{}
}

func TestPipelineCompletes(t *testing.T) {
	p := startPipeline(t, &analysistest.Source{Comments: []analysis.Comment{
		{ID: "a", Text: "Thank you!"}, {ID: "b", Text: "Thanks"},
	}})

	taskID := p.start(t, "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	got := p.waitFinal(t, taskID)

	want := statusBody{
		TaskID: taskID,
		Status: "completed",
		Result: analysis.Result{analysis.CategoryThanks: {{ID: "a", Text: "Thank you!"}, {ID: "b", Text: "Thanks"}}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("final status mismatch (-want +got):\n%s", diff)
	}
}

func TestPipelineSourceErrorFailsTask(t *testing.T) {
	const message = "Comments are disabled for this video or API quota is exceeded"
	p := startPipeline(t, &analysistest.Source{Err: &analysis.CommentSourceError{Message: message}})

	got := p.waitFinal(t, p.start(t, "https://youtu.be/dQw4w9WgXcQ"))

	if got.Status != "failed" || got.Error == nil || *got.Error != message {
		t.Errorf("got %+v, want failed with %q", got, message)
	}
}

// TestWorkerSurvivesIdleLongerThanReadTimeout — регрессия из Python-версии: там воркер
// ждал задачу блокирующим BRPOP, и redis-py рвал чтение по socket_timeout.
// asynq не использует блокирующих команд (опрашивает очередь), поэтому воркер,
// простоявший без задач во много раз дольше ReadTimeout, всё ещё работает.
func TestWorkerSurvivesIdleLongerThanReadTimeout(t *testing.T) {
	p := startPipeline(t, &analysistest.Source{Comments: []analysis.Comment{{ID: "a", Text: "hi"}}})

	time.Sleep(5 * readTimeout) // воркер простаивает на пустой очереди

	if got := p.waitFinal(t, p.start(t, "https://youtu.be/dQw4w9WgXcQ")); got.Status != "completed" {
		t.Errorf("status after idle = %q, want completed", got.Status)
	}
}

// TestGoRedisBlockingCommandIgnoresReadTimeout фиксирует поведение go-redis на случай,
// если очередь когда-нибудь перейдёт на блокирующие команды (BLMOVE, XREADGROUP BLOCK):
// для них go-redis сам продлевает дедлайн чтения на время блокировки (+10 с),
// а не рвёт соединение по ReadTimeout, как redis-py 8.
func TestGoRedisBlockingCommandIgnoresReadTimeout(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), ReadTimeout: readTimeout, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	go func() {
		time.Sleep(3 * readTimeout)
		_, _ = mr.Lpush("queue", "job")
	}()

	got, err := rdb.BLPop(t.Context(), 0, "queue").Result() // 0 — ждать бесконечно
	if err != nil {
		t.Fatalf("BLPOP failed after waiting longer than ReadTimeout: %v", err)
	}
	if diff := cmp.Diff([]string{"queue", "job"}, got); diff != "" {
		t.Errorf("BLPOP mismatch (-want +got):\n%s", diff)
	}
}
