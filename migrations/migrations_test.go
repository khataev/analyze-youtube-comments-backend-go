package migrations_test

import (
	"context"
	"log/slog"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ytcomments/internal/core/postgres"
	"ytcomments/internal/core/postgres/pgtest"
	"ytcomments/migrations"
)

func TestMain(m *testing.M) { pgtest.Main(m, migrations.FS) }

// TestMigrationsUpDownUp проверяет, что миграции применяются и откатываются начисто.
// Чтобы не ломать параллельно идущие пакеты, всё происходит в отдельной временной схеме
// тестовой базы: search_path в URL направляет туда и таблицы, и служебную таблицу goose.
func TestMigrationsUpDownUp(t *testing.T) {
	ctx := t.Context()
	schema := "migrations_check_" + uuid.NewString()[:8]
	pool := pgtest.Pool(t)
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})

	migrator, err := postgres.NewMigrator(withParam(t, pgtest.URL(), "search_path", schema),
		migrations.FS, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()

	tableExists := func() bool {
		var exists bool
		err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", schema+".analysis_tasks").Scan(&exists)
		if err != nil {
			t.Fatal(err)
		}
		return exists
	}

	steps := []struct {
		name      string
		run       func(context.Context) error
		wantTable bool
	}{
		{"up", migrator.Up, true},
		{"reset", migrator.Reset, false},
		{"up again", migrator.Up, true},
	}
	for _, step := range steps {
		if err := step.run(ctx); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got := tableExists(); got != step.wantTable {
			t.Fatalf("after %s: table exists = %v, want %v", step.name, got, step.wantTable)
		}
	}
}

func withParam(t *testing.T, raw, key, value string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}
