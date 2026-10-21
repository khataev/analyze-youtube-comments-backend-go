package analysis_test

import (
	"testing"

	"ytcomments/internal/postgres/pgtest"
)

// TestMain — точка входа тестов пакета (аналог before(:suite) в RSpec):
// один раз готовит тестовую базу, затем запускает все тесты.
func TestMain(m *testing.M) { pgtest.Main(m) }
