// Команда worker — фоновый обработчик задач анализа из очереди asynq.
//
// Отдельный процесс нужен не ради асинхронности (горутины дешёвые и в API), а ради
// надёжности: задачи переживают рестарт и деплой API, есть повторы, а API и воркеры
// масштабируются независимо. Как Sidekiq рядом с Puma.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"ytcomments/internal/analysis"
	"ytcomments/internal/config"
	"ytcomments/internal/llm"
	"ytcomments/internal/logging"
	"ytcomments/internal/youtube"
)

const (
	// workerConcurrency — сколько задач воркер обрабатывает одновременно (горутин).
	// Аналог concurrency у Sidekiq.
	workerConcurrency = 4
	// shutdownTimeout — сколько ждать завершения текущих задач при остановке.
	// Незавершённые asynq вернёт в очередь. Как -t 25 у Sidekiq; docker compose
	// даёт контейнеру 30 с (stop_grace_period).
	shutdownTimeout = 25 * time.Second

	youtubeHTTPTimeout = 15 * time.Second
	// LLM отвечает на батч из 50 комментариев десятки секунд.
	llmHTTPTimeout = 2 * time.Minute

	startupPingTimeout = 3 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(os.Stdout, cfg.LogLevel.Level())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Ошибки конфигурации LLM ловим до подключения к чему-либо.
	model, err := llm.ParseModel(cfg.LLMModel)
	if err != nil {
		return err
	}
	transport := newTransport()
	llmClient, err := llm.NewOpenAICompatible(model, cfg.LLMAPIKey.Reveal(),
		&http.Client{Transport: transport, Timeout: llmHTTPTimeout})
	if err != nil {
		return err
	}

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer closeWithLog(logger, "redis", rdb.Close)
	pingRedis(ctx, logger, rdb, cfg.RedisURL)

	analyzer := analysis.NewAnalyzer(
		analysis.NewRedisRepository(rdb, cfg.TaskTTL()),
		youtube.NewClient(
			&http.Client{Transport: transport, Timeout: youtubeHTTPTimeout},
			cfg.YouTubeAPIKey.Reveal(), cfg.YouTubeMaxComments,
			youtube.WithLogger(logger),
		),
		analysis.NewLLMClassifier(llmClient, cfg.LLMBatchSize, logger),
		logger,
	)

	server := asynq.NewServerFromRedisClient(rdb, asynq.Config{
		Concurrency:     workerConcurrency,
		Queues:          map[string]int{analysis.QueueName: 1},
		RetryDelayFunc:  analysis.RetryDelay,
		ShutdownTimeout: shutdownTimeout,
		Logger:          logging.NewAsynqLogger(logger),
		LogLevel:        asynq.InfoLevel,
		// ErrorHandler вызывается, когда обработчик вернул ошибку (до повтора или архивации).
		ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, task *asynq.Task, err error) {
			retried, _ := asynq.GetRetryCount(ctx)
			maxRetry, _ := asynq.GetMaxRetry(ctx)
			id, _ := asynq.GetTaskID(ctx)
			logger.WarnContext(ctx, "queue: task attempt returned error",
				"task_id", id, "type", task.Type(), "retried", retried, "max_retry", maxRetry, "error", err)
		}),
	})

	mux := asynq.NewServeMux()
	mux.Handle(analysis.TypeAnalyzeVideo, analysis.NewTaskHandler(analyzer, logger))

	// Start не блокирует: обработка идёт в горутинах asynq. (Run заблокировал бы и сам
	// ловил сигналы — нам удобнее управлять остановкой через общий ctx.)
	if err := server.Start(mux); err != nil {
		return fmt.Errorf("start worker: %w", err)
	}
	logger.Info("worker ready",
		"redis", config.RedactURL(cfg.RedisURL),
		"queue", analysis.QueueName,
		"tasks", []string{analysis.TypeAnalyzeVideo},
		"concurrency", workerConcurrency,
		"llm", model.String(),
		"llm_key_from", cfg.LLMKeySource,
		"max_comments", cfg.YouTubeMaxComments,
	)

	<-ctx.Done()
	logger.Info("worker shutting down, waiting for active tasks", "timeout", shutdownTimeout)
	server.Shutdown()
	logger.Info("worker stopped")
	return nil
}

// newTransport — общий транспорт (пул соединений) для внешних HTTP-клиентов
// с таймаутами на каждом этапе соединения. Общий таймаут запроса задаёт http.Client.
func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 5 * time.Second
	t.ResponseHeaderTimeout = llmHTTPTimeout
	t.MaxIdleConnsPerHost = workerConcurrency * 4
	return t
}

func pingRedis(ctx context.Context, logger *slog.Logger, rdb *redis.Client, url string) {
	ctx, cancel := context.WithTimeout(ctx, startupPingTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		// asynq сам будет переподключаться, но причина видна сразу.
		logger.WarnContext(ctx, "redis is unavailable", "redis", config.RedactURL(url), "error", err)
		return
	}
	logger.InfoContext(ctx, "redis is available", "redis", config.RedactURL(url))
}

func closeWithLog(logger *slog.Logger, name string, closeFn func() error) {
	if err := closeFn(); err != nil {
		logger.Warn("close failed", "resource", name, "error", err)
	}
}
