package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"

	_ "github.com/jackc/pgx/v5/stdlib" // драйвер "pgx" для database/sql: goose работает через него
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// SQL-файлы миграций вшиваются в бинарник (go:embed): образу не нужны ни исходники,
// ни отдельный CLI. Аналог db/migrate в Rails, но файлы — обычный SQL.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrator применяет миграции goose. Аналог `rails db:migrate` / `alembic upgrade`.
type Migrator struct {
	db       *sql.DB
	provider *goose.Provider
	logger   *slog.Logger
}

// NewMigrator открывает отдельное соединение для миграций.
//
// Миграции выполняются под advisory lock Postgres: если их одновременно запустят
// два процесса (два контейнера migrate, параллельные пакеты тестов), второй
// дождётся первого, а не попытается создать ту же таблицу.
func NewMigrator(databaseURL string, logger *slog.Logger) (*Migrator, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	// Ждать блокировку до минуты, проверяя раз в секунду (по умолчанию — раз в 5 с).
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 60))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations, goose.WithSessionLocker(locker))
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: migrations: %w", err)
	}
	return &Migrator{db: db, provider: provider, logger: logger}, nil
}

// Up применяет все новые миграции.
func (m *Migrator) Up(ctx context.Context) error {
	results, err := m.provider.Up(ctx)
	m.logResults(ctx, results)
	if err != nil {
		return fmt.Errorf("postgres: migrate up: %w", err)
	}
	version, err := m.provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("postgres: schema version: %w", err)
	}
	m.logger.InfoContext(ctx, "migrations applied", "applied", len(results), "version", version)
	return nil
}

// Down откатывает последнюю миграцию (`rails db:rollback`).
func (m *Migrator) Down(ctx context.Context) error {
	result, err := m.provider.Down(ctx)
	if result != nil {
		m.logResults(ctx, []*goose.MigrationResult{result})
	}
	if err != nil {
		return fmt.Errorf("postgres: migrate down: %w", err)
	}
	return nil
}

// Reset откатывает все миграции.
func (m *Migrator) Reset(ctx context.Context) error {
	results, err := m.provider.DownTo(ctx, 0)
	m.logResults(ctx, results)
	if err != nil {
		return fmt.Errorf("postgres: migrate reset: %w", err)
	}
	return nil
}

// Status возвращает состояние каждой миграции (`rails db:migrate:status`).
func (m *Migrator) Status(ctx context.Context) ([]*goose.MigrationStatus, error) {
	statuses, err := m.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: migrations status: %w", err)
	}
	return statuses, nil
}

// Close закрывает соединение.
func (m *Migrator) Close() error { return m.db.Close() }

func (m *Migrator) logResults(ctx context.Context, results []*goose.MigrationResult) {
	for _, r := range results {
		attrs := []any{"version", r.Source.Version, "file", r.Source.Path, "direction", r.Direction,
			"duration", r.Duration}
		if r.Error != nil {
			m.logger.ErrorContext(ctx, "migration failed", append(attrs, "error", r.Error)...)
			continue
		}
		m.logger.InfoContext(ctx, "migration done", attrs...)
	}
}
