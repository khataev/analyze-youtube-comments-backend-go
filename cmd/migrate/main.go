// Команда migrate — управление базой: создание и миграции. Аналог `rails db:*`.
//
//	migrate [prepare]  создать базу, если её нет, и применить миграции (rails db:prepare)
//	migrate create     только создать базу (rails db:create)
//	migrate up         применить новые миграции (rails db:migrate)
//	migrate down       откатить последнюю миграцию (rails db:rollback)
//	migrate status     состояние миграций (rails db:migrate:status)
//
// Читает только DATABASE_URL (config.LoadDatabase): ключи YouTube и LLM не нужны.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"ytcomments/internal/config"
	"ytcomments/internal/logging"
	"ytcomments/internal/postgres"
)

const usage = `usage: migrate [command]

commands:
  prepare  create the database if missing and apply migrations (default)
  create   create the database if missing
  up       apply pending migrations
  down     roll back the last migration
  status   show migration status
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

var commands = []string{"prepare", "create", "up", "down", "status"}

func run(args []string) error {
	command := "prepare"
	if len(args) > 0 {
		command = args[0]
	}
	switch {
	case command == "-h" || command == "--help" || command == "help":
		fmt.Fprint(os.Stderr, usage)
		return nil
	case len(args) > 1 || !slices.Contains(commands, command):
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", strings.Join(args, " "))
	}

	cfg, err := config.LoadDatabase()
	if err != nil {
		return err
	}
	logger := logging.New(os.Stdout, slog.LevelInfo)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.InfoContext(ctx, "migrate", "command", command, "postgres", config.RedactURL(cfg.DatabaseURL))

	switch command {
	case "prepare":
		if err := createDatabase(ctx, logger, cfg.DatabaseURL); err != nil {
			return err
		}
		return withMigrator(cfg.DatabaseURL, logger, func(m *postgres.Migrator) error { return m.Up(ctx) })
	case "create":
		return createDatabase(ctx, logger, cfg.DatabaseURL)
	case "up":
		return withMigrator(cfg.DatabaseURL, logger, func(m *postgres.Migrator) error { return m.Up(ctx) })
	case "down":
		return withMigrator(cfg.DatabaseURL, logger, func(m *postgres.Migrator) error { return m.Down(ctx) })
	case "status":
		return withMigrator(cfg.DatabaseURL, logger, func(m *postgres.Migrator) error { return printStatus(ctx, m) })
	default:
		panic("unreachable: command is validated above")
	}
}

func createDatabase(ctx context.Context, logger *slog.Logger, databaseURL string) error {
	created, err := postgres.CreateDatabase(ctx, databaseURL)
	if err != nil {
		return err
	}
	if created {
		logger.InfoContext(ctx, "database created")
	} else {
		logger.InfoContext(ctx, "database already exists")
	}
	return nil
}

func withMigrator(databaseURL string, logger *slog.Logger, fn func(*postgres.Migrator) error) (err error) {
	m, err := postgres.NewMigrator(databaseURL, logger)
	if err != nil {
		return err
	}
	// Именованный результат err позволяет вернуть и ошибку Close, не потеряв основную.
	defer func() { err = errors.Join(err, m.Close()) }()
	return fn(m)
}

func printStatus(ctx context.Context, m *postgres.Migrator) error {
	statuses, err := m.Status(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tSTATE\tAPPLIED AT\tFILE")
	for _, s := range statuses {
		applied := "-"
		if !s.AppliedAt.IsZero() {
			applied = s.AppliedAt.Local().Format(time.DateTime)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", s.Source.Version, s.State, applied, s.Source.Path)
	}
	return w.Flush()
}
