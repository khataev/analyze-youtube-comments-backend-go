// Команда api — HTTP-сервер: принимает ссылки, ставит задачи в очередь, отдаёт статусы.
//
// main — composition root: здесь и только здесь создаются конкретные реализации
// (Redis, asynq, сервис) и связываются друг с другом. Остальной код получает
// зависимости через конструкторы. Аналог lifespan + deps.py в FastAPI, но явно и без магии.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"ytcomments/internal/analysis"
	"ytcomments/internal/config"
	"ytcomments/internal/httpapi"
	"ytcomments/internal/logging"
)

const (
	startupPingTimeout = 3 * time.Second
	shutdownTimeout    = 10 * time.Second
)

func main() {
	// Вся логика в run(), который возвращает error: так работают defer (закрытие ресурсов),
	// а os.Exit вызывается в одном месте. os.Exit внутри run пропустил бы все defer.
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":8000", "HTTP listen address")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(os.Stdout, cfg.LogLevel.Level())

	// ctx отменится по SIGINT (Ctrl+C) или SIGTERM (docker stop, Kubernetes).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	redisOpts, err := redis.ParseURL(cfg.RedisURL) // URL уже проверен в config
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer closeWithLog(logger, "redis", rdb.Close) // defer выполняются в обратном порядке: Redis закроется последним

	pingRedis(ctx, logger, rdb, cfg.RedisURL)

	// Клиент asynq использует наш пул соединений; закрывать пул — наша забота (rdb.Close выше).
	queueClient := asynq.NewClientFromRedisClient(rdb)

	service := analysis.NewService(
		analysis.NewRedisRepository(rdb, cfg.TaskTTL()),
		analysis.NewAsynqQueue(queueClient),
		logger,
	)
	router := httpapi.NewRouter(httpapi.Deps{
		Analysis:    analysis.NewHandler(service, logger),
		Storage:     httpapi.PingerFunc(func(ctx context.Context) error { return rdb.Ping(ctx).Err() }),
		CORSOrigins: cfg.CORSOrigins,
		Logger:      logger,
	})

	server := &http.Server{
		Addr:    *addr,
		Handler: router,
		// Таймауты обязательны: у http.Server по умолчанию их нет вовсе.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// ListenAndServe блокирует, поэтому запускаем его в горутине, а основная ждёт
	// либо сигнала, либо ошибки сервера (например, порт занят). Канал — способ
	// передать результат из горутины (как Future/Queue в Python).
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("api started", "addr", *addr)
		serverErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	logger.Info("api shutting down")
	// Новый context: исходный уже отменён, а на завершение текущих запросов нужно время.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http shutdown: %w", err)
	}
	logger.Info("api stopped")
	return nil
}

// pingRedis не роняет процесс: Redis может подняться позже (health отдаст 503),
// но в логе сразу видно, к какому Redis подключаемся и в какую очередь пишем.
func pingRedis(ctx context.Context, logger *slog.Logger, rdb *redis.Client, url string) {
	ctx, cancel := context.WithTimeout(ctx, startupPingTimeout)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.WarnContext(ctx, "redis is unavailable", "redis", config.RedactURL(url), "error", err)
		return
	}
	logger.InfoContext(ctx, "redis is available", "redis", config.RedactURL(url), "queue", analysis.QueueName)
}

func closeWithLog(logger *slog.Logger, name string, closeFn func() error) {
	if err := closeFn(); err != nil {
		logger.Warn("close failed", "resource", name, "error", err)
	}
}
