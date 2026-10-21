package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"ytcomments/internal/core/postgres"
	"ytcomments/internal/core/postgres/pgtest"
)

// Схема этим тестам не нужна: pgtest только создаёт базу.
func TestMain(m *testing.M) { pgtest.Main(m, nil) }

func TestIsUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"no rows", fmt.Errorf("get: %w", pgx.ErrNoRows), false},
		{"canceled", context.Canceled, false},
		{"network", errors.New("dial tcp 127.0.0.1:1: connect: connection refused"), true},
		{"deadline", context.DeadlineExceeded, true},
		{"syntax error", &pgconn.PgError{Code: "42601"}, false},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"connection failure", &pgconn.PgError{Code: "08006"}, true},
		{"too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"admin shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"database does not exist", &pgconn.PgError{Code: "3D000"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := postgres.IsUnavailable(tt.err); got != tt.want {
				t.Errorf("IsUnavailable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestConnectToStoppedServerIsUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := postgres.Connect(ctx, "postgres://app:app@127.0.0.1:1/x?sslmode=disable")
	if err == nil || !postgres.IsUnavailable(err) {
		t.Fatalf("Connect to a closed port: err = %v, want an unavailable error", err)
	}
}

func TestCreateDatabaseIsIdempotent(t *testing.T) {
	name := "analysis_go_test_create_" + randomSuffix()
	dbURL := withDatabase(t, pgtest.URL(), name)
	t.Cleanup(func() {
		_, err := pgtest.Pool(t).Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize())
		if err != nil {
			t.Errorf("drop database: %v", err)
		}
	})

	for i, want := range []bool{true, false} { // второй вызов — база уже есть
		created, err := postgres.CreateDatabase(t.Context(), dbURL)
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if created != want {
			t.Errorf("call %d: created = %v, want %v", i+1, created, want)
		}
	}
}

func TestCreateDatabaseRejectsMaintenanceDB(t *testing.T) {
	if _, err := postgres.CreateDatabase(t.Context(), withDatabase(t, pgtest.URL(), "postgres")); err == nil {
		t.Error("want error for the maintenance database")
	}
}

func randomSuffix() string { return uuid.NewString()[:8] }

func withDatabase(t *testing.T, raw, name string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
