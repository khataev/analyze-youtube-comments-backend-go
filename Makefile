# Основные команды. `make help` — список.
.DEFAULT_GOAL := help

.PHONY: help run-api run-worker build test test-cover lint fmt tidy docker-up docker-down \
	db-prepare db-create db-migrate db-rollback db-status generate

help: ## Показать список команд
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-13s %s\n", $$1, $$2}'

run-api: ## Запустить API локально (нужны Postgres и Redis из ../docker-compose.yml)
	go run ./cmd/api

run-worker: ## Запустить воркер локально (нужны Postgres и Redis)
	go run ./cmd/worker

# Аналоги rails db:*. Читают только DATABASE_URL из окружения или .env.
db-prepare: ## Создать базу, если её нет, и применить миграции (rails db:prepare)
	go run ./cmd/migrate prepare

db-create: ## Создать базу, если её нет (rails db:create)
	go run ./cmd/migrate create

db-migrate: ## Применить новые миграции (rails db:migrate)
	go run ./cmd/migrate up

db-rollback: ## Откатить последнюю миграцию (rails db:rollback)
	go run ./cmd/migrate down

db-status: ## Состояние миграций (rails db:migrate:status)
	go run ./cmd/migrate status

build: ## Собрать бинарники в bin/
	go build -o bin/ ./cmd/...

test: ## Тесты с детектором гонок (нужен Postgres: база analysis_go_test создаётся сама)
	go test -race -count=1 ./...

test-cover: ## Тесты с отчётом о покрытии
	go test -race -count=1 -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

lint: ## go vet + golangci-lint (включает проверку gofmt/goimports)
	go vet ./...
	golangci-lint run ./...

fmt: ## Отформатировать код
	golangci-lint fmt ./...

tidy: ## Привести go.mod/go.sum в порядок
	go mod tidy
	go -C tools mod tidy

# sqlc — инструмент проекта (tools/go.mod), а не системная установка: версия зафиксирована,
# `go tool` сам скачает и соберёт её при первом запуске (как bundle exec / npx).
generate: ## Перегенерировать код запросов к БД (sqlc)
	go -C tools tool sqlc generate -f ../sqlc.yaml

docker-up: ## Поднять общую инфраструктуру, затем migrate + api + worker в Docker
	docker compose -f ../docker-compose.yml up -d --wait
	docker compose up --build

docker-down: ## Остановить сервисы этого backend (общая инфраструктура остаётся)
	docker compose down
