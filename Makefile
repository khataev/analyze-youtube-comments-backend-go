# Основные команды. `make help` — список.
.DEFAULT_GOAL := help

.PHONY: help run-api run-worker build test test-cover lint fmt tidy docker-up docker-down

help: ## Показать список команд
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'

run-api: ## Запустить API локально (нужен Redis)
	go run ./cmd/api

run-worker: ## Запустить воркер локально (нужен Redis)
	go run ./cmd/worker

build: ## Собрать бинарники в bin/
	go build -o bin/ ./cmd/...

test: ## Тесты с детектором гонок (внешние сервисы не нужны)
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

docker-up: ## Поднять redis + api + worker в Docker
	docker compose up --build

docker-down: ## Остановить Docker-окружение
	docker compose down
