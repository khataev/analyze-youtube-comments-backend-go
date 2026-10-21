// Package pgtest — тестовая база Postgres, как test-окружение в Rails:
// отдельная база analysis_go_test, схема из миграций, каждый тест — в транзакции с откатом.
//
// Использование в пакете с тестами, которые ходят в БД:
//
//	func TestMain(m *testing.M) { pgtest.Main(m) }
//
//	func TestX(t *testing.T) {
//		tx := pgtest.Tx(t) // всё, что тест запишет через tx, откатится в конце
//		...
//	}
package pgtest

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ytcomments/internal/postgres"
)

// DefaultURL — тестовая база в общей инфраструктуре (../docker-compose.yml).
//
// Адрес задаётся отдельной переменной TEST_DATABASE_URL, а не выводится из DATABASE_URL:
// опечатка в одной переменной не должна привести к тому, что тесты пишут в рабочую базу.
const DefaultURL = "postgres://app:app@localhost:5432/analysis_go_test?sslmode=disable"

const setupTimeout = 2 * time.Minute

var pool *pgxpool.Pool // общий пул пакета; создаётся в Main

// URL — адрес тестовой базы.
func URL() string {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u
	}
	return DefaultURL
}

// Main готовит тестовую базу и запускает тесты пакета. Вызывается из TestMain.
//
// go test запускает каждый пакет отдельным процессом (и по умолчанию параллельно),
// поэтому «один раз на прогон» здесь — один раз на пакет. Создание базы и миграции
// идемпотентны и выполняются под advisory lock, так что параллельные пакеты не мешают
// друг другу. Если Postgres не запущен, прогон пакета останавливается одной строкой
// с подсказкой, а не десятком одинаковых ошибок соединения.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	var err error
	if pool, err = prepare(ctx, URL()); err != nil {
		fmt.Fprintf(os.Stderr, "\npgtest: test database is not ready: %v\n"+
			"pgtest: is Postgres running? Start the shared infrastructure: docker compose -f ../docker-compose.yml up -d\n\n", err)
		return 1
	}
	defer pool.Close()
	return m.Run()
}

func prepare(ctx context.Context, url string) (*pgxpool.Pool, error) {
	if _, err := postgres.CreateDatabase(ctx, url); err != nil {
		return nil, err
	}
	migrator, err := postgres.NewMigrator(url, slog.New(slog.DiscardHandler))
	if err != nil {
		return nil, err
	}
	defer migrator.Close()
	if err := migrator.Up(ctx); err != nil {
		return nil, err
	}
	return postgres.Connect(ctx, url)
}

// Pool — общий пул тестовой базы. Записи через него коммитятся по-настоящему:
// используйте его только там, где транзакция с откатом невозможна, и убирайте за собой.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	if pool == nil {
		t.Fatal("pgtest: call pgtest.Main(m) from TestMain of this package")
	}
	return pool
}

// Tx открывает транзакцию, которая откатывается в конце теста (как use_transactional_tests
// в Rails). Код, который сам открывает транзакции через Begin, внутри неё получает
// SAVEPOINT: его COMMIT — это RELEASE SAVEPOINT, а данные всё равно исчезнут при откате.
//
// pgx.Tx — одно соединение: использовать его одновременно из нескольких горутин нельзя.
func Tx(t testing.TB) pgx.Tx {
	t.Helper()
	tx, err := Pool(t).Begin(t.Context())
	if err != nil {
		t.Fatalf("pgtest: begin: %v", err)
	}
	t.Cleanup(func() {
		// t.Context() к этому моменту уже отменён — откат идёт с собственным context.
		if err := tx.Rollback(context.Background()); err != nil {
			t.Errorf("pgtest: rollback: %v", err)
		}
	})
	return tx
}
