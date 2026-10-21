package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]string{"YOUTUBE_API_KEY=yt", "OPENAI_API_KEY=sk"})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Database:           Database{DatabaseURL: "postgres://app:app@localhost:5432/analysis_go?sslmode=disable"},
		RedisURL:           "redis://localhost:6379/1",
		CORSOrigins:        StringList{"http://localhost:5173"},
		LogLevel:           LogLevel(slog.LevelInfo),
		YouTubeAPIKey:      "yt",
		YouTubeMaxComments: 200,
		LLMModel:           "openai:o3-mini",
		LLMAPIKey:          "sk", // ключ провайдера из OPENAI_API_KEY, как в pydantic-ai
		LLMKeySource:       "OPENAI_API_KEY",
		LLMBatchSize:       50,
	}
	if diff := cmp.Diff(want, cfg); diff != "" {
		t.Errorf("config mismatch (-want +got):\n%s", diff)
	}
}

func TestParseValues(t *testing.T) {
	cfg, err := Parse([]string{
		"YOUTUBE_API_KEY=yt",
		"REDIS_URL=redis://:pass@redis:6380/1",
		`CORS_ORIGINS=["http://a.test", "http://b.test"]`,
		"LOG_LEVEL=warning",
		"LLM_MODEL=deepseek:deepseek-chat",
		"LLM_API_KEY=explicit",
		"DEEPSEEK_API_KEY=ignored",
		"LLM_BATCH_SIZE=10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(StringList{"http://a.test", "http://b.test"}, cfg.CORSOrigins); diff != "" {
		t.Errorf("CORS_ORIGINS mismatch: %s", diff)
	}
	if cfg.LogLevel.Level() != slog.LevelWarn {
		t.Errorf("LOG_LEVEL = %v, want WARN", cfg.LogLevel.Level())
	}
	if cfg.LLMAPIKey.Reveal() != "explicit" || cfg.LLMKeySource != "LLM_API_KEY" || cfg.LLMBatchSize != 10 {
		t.Errorf("LLM settings = %q from %s, batch %d", cfg.LLMAPIKey.Reveal(), cfg.LLMKeySource, cfg.LLMBatchSize)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     []string
		wantErr string // подстрока сообщения
	}{
		{"missing youtube key", nil, "YOUTUBE_API_KEY"},
		{"empty youtube key", []string{"YOUTUBE_API_KEY="}, "YOUTUBE_API_KEY"},
		{"zero max comments", []string{"YOUTUBE_API_KEY=k", "YOUTUBE_MAX_COMMENTS=0"}, "YOUTUBE_MAX_COMMENTS must be greater than 0"},
		{"not a number", []string{"YOUTUBE_API_KEY=k", "LLM_BATCH_SIZE=many"}, "LLM_BATCH_SIZE"},
		{"bad model", []string{"YOUTUBE_API_KEY=k", "LLM_MODEL=gpt-4o"}, "provider:model"},
		{"bad redis url", []string{"YOUTUBE_API_KEY=k", "REDIS_URL=http://x"}, "REDIS_URL"},
		{"bad log level", []string{"YOUTUBE_API_KEY=k", "LOG_LEVEL=LOUD"}, "LOG_LEVEL"},
		{"bad database url", []string{"YOUTUBE_API_KEY=k", "DATABASE_URL=postgres://h:port/db"}, "DATABASE_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.env)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Parse error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// Командам БД (cmd/migrate) ключи YouTube и LLM не нужны.
func TestParseDatabaseNeedsNoAPIKeys(t *testing.T) {
	db, err := ParseDatabase([]string{"DATABASE_URL=postgres://u:p@db:5432/x"})
	if err != nil {
		t.Fatalf("ParseDatabase without API keys: %v", err)
	}
	if db.DatabaseURL != "postgres://u:p@db:5432/x" {
		t.Errorf("DatabaseURL = %q", db.DatabaseURL)
	}
}

func TestInvalidDatabaseURLDoesNotLeakPassword(t *testing.T) {
	_, err := ParseDatabase([]string{"DATABASE_URL=postgres://app:hunter2@db:port/x"})
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error = %v; want it to name DATABASE_URL and hide the password", err)
	}
}

func TestSecretIsNotPrinted(t *testing.T) {
	s := Secret("sk-very-secret")
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%v %s", s, s), slog.AnyValue(s).Resolve().String()} {
		if strings.Contains(out, "very-secret") {
			t.Errorf("secret leaked: %q", out)
		}
	}
}

func TestRedactURL(t *testing.T) {
	tests := map[string]string{
		"redis://localhost:6379/0":          "redis://localhost:6379/0",
		"redis://:hunter2@redis:6379/0":     "redis://:xxxxx@redis:6379/0",
		"redis://user:hunter2@redis:6379/0": "redis://user:xxxxx@redis:6379/0",
		"postgres://app:app@pg:5432/db":     "postgres://app:xxxxx@pg:5432/db",
	}
	for in, want := range tests {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindInModule(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "cmd", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "go.mod"), "module x\n")

	if _, ok := findInModule(nested); ok {
		t.Error("found .env that does not exist")
	}

	writeFile(t, filepath.Join(root, ".env"), "A=1\n")
	got, ok := findInModule(nested)
	if !ok || got != filepath.Join(root, ".env") {
		t.Errorf("findInModule = %q, %v; want module root .env", got, ok)
	}

	// Вне модуля поиск ничего не находит, даже если выше лежит .env.
	if _, ok := findInModule(t.TempDir()); ok {
		t.Error("found .env outside of a module")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
