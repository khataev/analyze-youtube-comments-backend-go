package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// maintenanceDB — служебная база, которая есть в любом кластере Postgres.
const maintenanceDB = "postgres"

// CreateDatabase создаёт базу из databaseURL, если её нет, — аналог `rails db:create`.
// Возвращает true, если база создана сейчас.
//
// В Postgres нет CREATE DATABASE IF NOT EXISTS, а саму команду нельзя выполнить
// ни внутри транзакции, ни подключившись к создаваемой базе. Поэтому подключаемся
// к служебной базе postgres одиночным соединением (pgx.Conn вне транзакции —
// это и есть autocommit) и сначала проверяем pg_database.
func CreateDatabase(ctx context.Context, databaseURL string) (bool, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return false, fmt.Errorf("postgres: %w", err)
	}
	name := cfg.Database
	if name == "" || name == maintenanceDB {
		return false, fmt.Errorf("postgres: DATABASE_URL must name an application database, got %q", name)
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = connectTimeout
	}
	cfg.Database = maintenanceDB

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return false, fmt.Errorf("postgres: connect to %q: %w", maintenanceDB, err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	var exists bool
	err = conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("postgres: check database %q: %w", name, err)
	}
	if exists {
		return false, nil
	}

	// Имя базы нельзя передать параметром ($1) — только вставить в текст команды,
	// поэтому оно экранируется как идентификатор: "analysis_go".
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P04" {
		// duplicate_database: другой процесс (параллельный пакет тестов, второй контейнер)
		// успел создать базу между проверкой и CREATE. Результат тот же — база есть.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres: create database %q: %w", name, err)
	}
	return true, nil
}
