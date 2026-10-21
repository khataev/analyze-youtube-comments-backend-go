package analysis_test

import (
	"testing"

	"ytcomments/internal/core/postgres/pgtest"
	"ytcomments/migrations"
)

// TestMain — точка входа тестов пакета (аналог before(:suite) в RSpec):
// один раз готовит тестовую базу, затем запускает все тесты.
func TestMain(m *testing.M) { pgtest.Main(m, migrations.FS) }
