// Package config читает и валидирует конфигурацию процесса.
//
// Аналог pydantic-settings: поля описаны тегами структуры, значения берутся из
// переменных окружения и файла .env. Конфигурация читается один раз в main и
// передаётся дальше явно — глобального get_settings() здесь нет.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

// Database — только подключение к БД. Этого хватает командам создания базы и миграций
// (cmd/migrate): ключи YouTube и LLM им не нужны и не должны требоваться.
type Database struct {
	// Формат libpq URL, который понимают pgx, psql и goose:
	// postgres://user:password@host:5432/dbname?sslmode=disable.
	// sslmode=disable — общий Postgres в Docker без TLS; без него pgx (по умолчанию prefer)
	// сначала пробует TLS, и в ошибках подключения появляются лишние строки «tls error».
	DatabaseURL string `env:"DATABASE_URL" envDefault:"postgres://app:app@localhost:5432/analysis_go?sslmode=disable"`
}

// Config — вся конфигурация api и воркера. Имена переменных совпадают с Python-версией.
//
// Database встроена (embedding): её поля доступны как cfg.DatabaseURL, а env-библиотека
// разбирает их вместе с остальными. Это композиция, а не наследование, как
// `class Settings(DatabaseSettings)` в Python, но эффект для чтения тот же.
type Config struct {
	Database

	RedisURL    string     `env:"REDIS_URL"    envDefault:"redis://localhost:6379/0"`
	CORSOrigins StringList `env:"CORS_ORIGINS" envDefault:"[\"http://localhost:5173\"]"`
	LogLevel    LogLevel   `env:"LOG_LEVEL"    envDefault:"INFO"`

	YouTubeAPIKey      Secret `env:"YOUTUBE_API_KEY,required,notEmpty"`
	YouTubeMaxComments int    `env:"YOUTUBE_MAX_COMMENTS" envDefault:"200"`

	// Формат "провайдер:модель", например "deepseek:deepseek-chat".
	LLMModel string `env:"LLM_MODEL" envDefault:"openai:o3-mini"`
	// Если не задан, берётся <PROVIDER>_API_KEY (DEEPSEEK_API_KEY и т.п.), как в pydantic-ai.
	LLMAPIKey    Secret `env:"LLM_API_KEY"`
	LLMBatchSize int    `env:"LLM_BATCH_SIZE" envDefault:"50"`
	// Откуда взят ключ LLM — только для лога при старте.
	LLMKeySource string `env:"-"`

	TaskTTLSeconds int `env:"TASK_TTL_SECONDS" envDefault:"86400"`
}

// TaskTTL — TTL записи задачи в виде time.Duration.
func (c Config) TaskTTL() time.Duration {
	return time.Duration(c.TaskTTLSeconds) * time.Second
}

// Load ищет и подгружает .env (если он есть), затем читает окружение и валидирует.
// Переменные окружения процесса имеют приоритет над .env, как в pydantic-settings.
func Load() (Config, error) {
	if err := loadDotEnv(); err != nil {
		return Config{}, err
	}
	return Parse(os.Environ())
}

// LoadDatabase — как Load, но читает только Database.
func LoadDatabase() (Database, error) {
	if err := loadDotEnv(); err != nil {
		return Database{}, err
	}
	return ParseDatabase(os.Environ())
}

// ParseDatabase читает Database из списка "KEY=VALUE".
func ParseDatabase(environ []string) (Database, error) {
	db, err := parseEnv[Database](environ)
	if err != nil {
		return Database{}, err
	}
	if err := db.validate(); err != nil {
		return Database{}, fmt.Errorf("config: %w", err)
	}
	return db, nil
}

// Parse читает конфигурацию из списка "KEY=VALUE". Отдельно от Load, чтобы тесты
// не трогали окружение процесса.
func Parse(environ []string) (Config, error) {
	cfg, err := parseEnv[Config](environ)
	if err != nil {
		return Config{}, err
	}
	vars := env.ToMap(environ)
	cfg.LLMKeySource = "LLM_API_KEY"
	if cfg.LLMAPIKey == "" {
		cfg.LLMKeySource = providerKeyVar(cfg.LLMModel)
		cfg.LLMAPIKey = Secret(vars[cfg.LLMKeySource])
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func loadDotEnv() error {
	if path, ok := findDotEnv(); ok {
		if err := godotenv.Load(path); err != nil {
			return fmt.Errorf("load %s: %w", path, err)
		}
	}
	return nil
}

// parseEnv — общая часть Parse и ParseDatabase. Generic-функция (Go 1.18+):
// T — любая структура с env-тегами.
func parseEnv[T any](environ []string) (T, error) {
	var cfg T
	if err := env.ParseWithOptions(&cfg, env.Options{Environment: env.ToMap(environ)}); err != nil {
		return cfg, fmt.Errorf("config: %w", humanizeEnvError[T](err))
	}
	return cfg, nil
}

func (d Database) validate() error {
	if _, err := pgx.ParseConfig(d.DatabaseURL); err != nil {
		return fmt.Errorf("DATABASE_URL: %w", err) // pgx сам маскирует пароль в тексте ошибки
	}
	return nil
}

func (c Config) validate() error {
	// errors.Join собирает все ошибки сразу: пользователь видит весь список, а не первую.
	errs := []error{c.Database.validate()} // nil в списке errors.Join пропускает
	if _, err := redis.ParseURL(c.RedisURL); err != nil {
		errs = append(errs, fmt.Errorf("REDIS_URL: %w", err))
	}
	for name, value := range map[string]int{
		"YOUTUBE_MAX_COMMENTS": c.YouTubeMaxComments,
		"LLM_BATCH_SIZE":       c.LLMBatchSize,
		"TASK_TTL_SECONDS":     c.TaskTTLSeconds,
	} {
		if value <= 0 {
			errs = append(errs, fmt.Errorf("%s must be greater than 0, got %d", name, value))
		}
	}
	if provider, model, ok := strings.Cut(c.LLMModel, ":"); !ok || provider == "" || model == "" {
		errs = append(errs, fmt.Errorf("LLM_MODEL must look like \"provider:model\", got %q", c.LLMModel))
	}
	return errors.Join(errs...)
}

// humanizeEnvError заменяет имена полей структуры (LLMBatchSize) на имена
// переменных окружения (LLM_BATCH_SIZE): пользователь правит .env, а не Go-код.
func humanizeEnvError[T any](err error) error {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return err
	}
	errs := make([]error, 0, len(agg.Errors))
	for _, e := range agg.Errors {
		var parseErr env.ParseError
		if errors.As(e, &parseErr) {
			if field, ok := reflect.TypeFor[T]().FieldByName(parseErr.Name); ok {
				name, _, _ := strings.Cut(field.Tag.Get("env"), ",")
				e = fmt.Errorf("%s: %w", name, parseErr.Err)
			}
		}
		errs = append(errs, e)
	}
	return errors.Join(errs...)
}

func providerKeyVar(model string) string {
	provider, _, _ := strings.Cut(model, ":")
	return strings.ToUpper(provider) + "_API_KEY"
}

// RedactURL маскирует пароль в URL для логов: redis://:xxxxx@host:6379/0.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	return u.Redacted() // стандартная библиотека умеет это сама
}

// findDotEnv ищет .env, чтобы запуск не зависел от текущего каталога.
//
// Поиск идёт вверх от текущего каталога и от каталога бинарника, но не выше
// корня модуля (каталога с go.mod): так `go run` из подкаталога и собранный
// bin/api находят backend-go/.env, а случайный ~/.env не подхватывается.
// В Docker go.mod нет — переменные приходят из окружения контейнера.
func findDotEnv() (string, bool) {
	var starts []string
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, start := range starts {
		if path, ok := findInModule(start); ok {
			return path, true
		}
	}
	return "", false
}

func findInModule(dir string) (string, bool) {
	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			path := filepath.Join(dir, ".env")
			return path, fileExists(path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Secret — строка, которая не печатается в логах и fmt.
// Аналог pydantic SecretStr: значение достаётся явно через Reveal.
type Secret string

// Reveal возвращает настоящее значение.
func (s Secret) Reveal() string { return string(s) }

// String вызывается fmt.Print/%v/%s.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "***"
}

// LogValue вызывается slog при логировании значения.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// StringList принимает JSON-список (как в Python-версии: ["a","b"]) или строку через запятую.
type StringList []string

// UnmarshalText реализует encoding.TextUnmarshaler — env-библиотека вызывает его сама.
func (l *StringList) UnmarshalText(text []byte) error {
	raw := strings.TrimSpace(string(text))
	if strings.HasPrefix(raw, "[") {
		return json.Unmarshal([]byte(raw), (*[]string)(l))
	}
	*l = nil
	for part := range strings.SplitSeq(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

// LogLevel — уровень slog. Понимает и питоновское WARNING.
type LogLevel slog.Level

// UnmarshalText реализует encoding.TextUnmarshaler.
func (l *LogLevel) UnmarshalText(text []byte) error {
	raw := strings.ToUpper(strings.TrimSpace(string(text)))
	if raw == "WARNING" {
		raw = "WARN"
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return err
	}
	*l = LogLevel(level)
	return nil
}

// Level возвращает значение как slog.Level.
func (l LogLevel) Level() slog.Level { return slog.Level(l) }
