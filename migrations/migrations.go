// Package migrations — схема базы приложения: SQL-миграции goose, аналог db/migrate в Rails.
//
// Лежит в корне проекта, а не в internal/core/postgres: таблицы принадлежат модулям
// (analysis_tasks — модулю analysis), а core/postgres умеет применять любые миграции
// и о конкретных таблицах не знает. Файлы вшиваются в бинарник (go:embed), поэтому
// образу не нужны ни исходники, ни отдельный CLI.
package migrations

import "embed"

// FS — все миграции. Передаётся в postgres.NewMigrator и pgtest.Main.
//
//go:embed *.sql
var FS embed.FS
