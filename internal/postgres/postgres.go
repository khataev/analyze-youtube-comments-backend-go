// Package postgres — общая инфраструктура Postgres: пул соединений, создание базы,
// миграции и классификация ошибок. Доменных типов здесь нет: таблицы и запросы
// конкретного домена живут рядом с доменом (internal/analysis).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// connectTimeout — сколько ждать установки соединения, если в URL не задан connect_timeout.
// Без него при недоступном сервере запрос висел бы до таймаута TCP (минуты),
// а /health и API должны быстро ответить 503.
const connectTimeout = 5 * time.Second

// Connect создаёт пул соединений и проверяет, что база отвечает.
//
// Пул — аналог connection pool в ActiveRecord/SQLAlchemy: создаётся один раз на процесс,
// безопасен для использования из многих горутин, соединения берутся на время запроса.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := NewPool(databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

// NewPool создаёт пул, не подключаясь: соединения открываются при первом запросе.
// Так API может стартовать при временно недоступной базе и отвечать 503, а не падать.
func NewPool(databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = connectTimeout
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return pool, nil
}

// IsUnavailable — ошибка означает, что база недоступна (а не что запрос неверен):
// не удалось подключиться, соединение оборвалось, сервер перегружен или выключается.
// Такие ошибки сервис отдаёт клиенту как 503, остальные — как внутренние (500).
func IsUnavailable(err error) bool {
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, context.Canceled) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Сервер ответил ошибкой. Недоступность — только классы, которые говорят о самом
		// сервере, а не о запросе: 08 — соединение, 53 — нехватка ресурсов
		// (too_many_connections), 57P — сервер выключается, 3D000 — базы нет.
		return strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "53") ||
			strings.HasPrefix(pgErr.Code, "57P") || pgErr.Code == "3D000"
	}
	// Ответа от сервера нет: ошибка сети, таймаут, закрытый пул.
	return true
}
