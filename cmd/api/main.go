// Команда api — HTTP-сервер: принимает ссылки, ставит задачи в очередь, отдаёт статусы.
//
// main — composition root: здесь и только здесь создаются конкретные реализации
// (Postgres, Redis, asynq, сервис) и связываются друг с другом. Остальной код получает
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
	"ytcomments/internal/postgres"
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

	// Пул Postgres: задачи и их результаты. Соединения открываются по требованию.
	db, err := postgres.NewPool(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close() // defer выполняются в обратном порядке: пул закроется последним

	redisOpts, err := redis.ParseURL(cfg.RedisURL) // URL уже проверен в config
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer closeWithLog(logger, "redis", rdb.Close)

	probe(ctx, logger, "postgres", cfg.DatabaseURL, db.Ping)
	probe(ctx, logger, "redis", cfg.RedisURL, func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
		"queue", analysis.QueueName)

	// Клиент asynq использует наш пул соединений; закрывать пул — наша забота (rdb.Close выше).
	queueClient := asynq.NewClientFromRedisClient(rdb)

	service := analysis.NewService(
		analysis.NewPostgresRepository(db),
		analysis.NewAsynqQueue(queueClient),
		logger,
	)
	router := httpapi.NewRouter(httpapi.Deps{
		Analysis: analysis.NewHandler(service, logger),
		Health: []httpapi.HealthCheck{
			{Name: "postgres", Pinger: db},
			{Name: "redis", Pinger: httpapi.PingerFunc(func(ctx context.Context) error { return rdb.Ping(ctx).Err() })},
		},
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

// probe проверяет зависимость при старте, но не роняет процесс: Postgres или Redis
// могут подняться позже (/health до тех пор отдаёт 503, asynq переподключается сам).
// Зато в логе сразу видно, куда процесс подключается (пароль замаскирован).
func probe(ctx context.Context, logger *slog.Logger, name, url string, ping func(context.Context) error, attrs ...any) {
	ctx, cancel := context.WithTimeout(ctx, startupPingTimeout)
	defer cancel()
	attrs = append([]any{name, config.RedactURL(url)}, attrs...)
	if err := ping(ctx); err != nil {
		logger.WarnContext(ctx, name+" is unavailable", append(attrs, "error", err)...)
		return
	}
	logger.InfoContext(ctx, name+" is available", attrs...)
}

func closeWithLog(logger *slog.Logger, name string, closeFn func() error) {
	if err := closeFn(); err != nil {
		logger.Warn("close failed", "resource", name, "error", err)
	}
}
