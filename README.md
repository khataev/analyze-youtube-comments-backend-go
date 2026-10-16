# YouTube Comments Analyzer — backend на Go

Реализация на Go того же сервиса, что и [`../backend-python`](../backend-python): по ссылке на
видео загружает комментарии через YouTube Data API, классифицирует их через LLM и отдаёт
результат, сгруппированный по категориям. HTTP-контракт, переменные окружения и формат
записей в Redis совпадают с Python-версией, поэтому общий фронтенд работает без изменений.

Проект намеренно устроен как «средний, который будет расти»: домены, слои, интерфейсы,
явная сборка зависимостей. Ниже по ходу объясняются решения, неочевидные для тех, кто
пришёл из Ruby или Python.

## Структура

```
backend-go/
├── cmd/                       # точки входа: один каталог = один бинарник
│   ├── api/main.go            # HTTP-сервер (composition root API)
│   └── worker/main.go         # воркер очереди (composition root воркера)
├── internal/                  # весь прикладной код; импортировать его снаружи модуля нельзя
│   ├── analysis/              # домен «анализ комментариев»: всё, что к нему относится
│   │   ├── model.go           #   Task, Status, Category, Classification, доменные ошибки
│   │   ├── videourl.go        #   ParseVideoID: watch?v=, youtu.be, /shorts, /embed, /live
│   │   ├── service.go         #   Service — сторона API (Start, Get) + интерфейсы TaskStore, TaskQueue
│   │   ├── analyzer.go        #   Analyzer — сторона воркера + интерфейсы CommentSource, Classifier
│   │   ├── classifier.go      #   LLMClassifier: промпт, JSON-схема, батчи, ограниченный параллелизм
│   │   ├── grouping.go        #   GroupByCategory: отбрасывает выдуманные и повторяющиеся id
│   │   ├── repository.go      #   RedisRepository: analysis:task:<uuid>, JSON, TTL
│   │   ├── queue.go           #   asynq: постановка задачи (API) и её обработчик (воркер)
│   │   ├── handler.go         #   HTTP-хендлер домена: только разбор запроса и ответ
│   │   └── analysistest/      #   фейки интерфейсов для тестов (как net/http/httptest)
│   ├── youtube/               # клиент YouTube Data API: пагинация, ретраи, ошибки 404/403/400
│   ├── llm/                   # провайдер-агностичный вызов LLM + OpenAI-совместимая реализация
│   ├── httpapi/               # сборка HTTP-приложения: маршруты, /health, middleware
│   ├── httpx/                 # JSON-ответы, единый маппинг ошибок в HTTP-коды, middleware
│   ├── apperr/                # прикладные ошибки с «видом» (NotFound, Invalid, Unavailable)
│   ├── config/                # чтение и валидация конфигурации, поиск .env, Secret
│   └── logging/               # slog + поля лога через context, адаптер логгера для asynq
├── test/e2e/                  # сквозной тест «API → очередь → воркер»
├── Makefile  Dockerfile  docker-compose.yml  .golangci.yml  .env.example
```

### Направление зависимостей

```
cmd/api ────► httpapi ──► analysis.Handler ──► analysis.Service ──► TaskStore, TaskQueue (интерфейсы)
                                                                          ▲            ▲
cmd/worker ─► asynq ──► analysis.TaskHandler ──► analysis.Analyzer        │            │
                                                   │                      │            │
                                                   ▼                      │            │
                                 CommentSource, Classifier (интерфейсы)   │            │
                                        ▲              ▲                  │            │
                                 youtube.Client   LLMClassifier ─► llm    RedisRepository  AsynqQueue
```

- **Хендлер** знает только HTTP: декодирует JSON, разбирает `task_id`, вызывает сервис.
- **Service и Analyzer** не знают ни про HTTP, ни про Redis, ни про asynq. Они зависят от интерфейсов.
- **`youtube` и `llm`** ничего не знают о домене: это переиспользуемые клиенты.
- **Конкретные реализации** подставляются в `cmd/*/main.go`.

### Почему так устроено (для тех, кто пришёл из Ruby/Python)

**Пакеты и `internal/`.**
- Пакет в Go — это каталог; все его файлы видят друг друга целиком.
- Видимость задаётся регистром: `Service` экспортирован, `service` — нет. Аналог `_private`, но проверяемый компилятором.
- Каталог `internal/` запрещает импортировать код извне модуля.
- Код организован по доменам, а не по слоям: вместо `models/`, `services/`, `handlers/` (как `app/models` в Rails) есть пакет `analysis`, в котором лежит всё о домене. Новый домен — новый пакет рядом.
- Круговые импорты в Go запрещены компилятором, поэтому направление зависимостей не просто договорённость.

**Интерфейсы объявляются у потребителя.**
- В Go интерфейс реализуется неявно, как `Protocol` в Python или duck typing в Ruby, только с проверкой при компиляции.
- `RedisRepository` нигде не пишет `implements TaskStore`: достаточно иметь методы `Save` и `Get`.
- Поэтому интерфейс объявляют там, где он нужен (`analysis.TaskStore`), и делают маленьким. Реализации о нём не знают.
- В тестах подставляются простые фейки из `analysistest` — без моков и без `dependency_overrides`.

**Composition root вместо DI.**
- В `main` создаются Redis, HTTP-клиенты, сервисы, и всё связывается явно через конструкторы `NewXxx(...)`.
- Нет `Depends`, глобальных `get_settings()` и инициализации при импорте.
- Если хендлеру чего-то не хватает, программа не скомпилируется.

**`context.Context`.**
- Передаётся первым аргументом во все функции, которые делают ввод-вывод.
- Несёт сигнал отмены и дедлайн: при SIGTERM, таймауте задачи asynq или уходе клиента обрываются HTTP-запросы к YouTube и LLM и команды Redis.
- Дополнительно через context в логи попадает `task_id` (`logging.With`), примерно как `Rails.logger.tagged`.
- Если сохранить статус `failed` нужно уже после отмены, используется `context.WithoutCancel` (`analyzer.go`).

**Ошибки — это значения.**
- Функция возвращает `error` последним результатом, и вызывающий проверяет его явно.
- Причины оборачиваются (`fmt.Errorf("fetch comments: %w", err)`) и проверяются по всей цепочке: `errors.Is(err, ErrTaskNotFound)` — аналог `rescue SomeError`, `errors.As(err, &ytErr)` — достать типизированную ошибку.
- Домен сообщает вид ошибки (`apperr.KindNotFound`), а HTTP-код выбирается в одном месте — `httpx.WriteError`.
- Для неизвестных ошибок клиент получает только `"Internal error"`, причина пишется в лог.

**Горутины.**
- `go f()` запускает функцию конкурентно, и стоит это дёшево.
- Батчи LLM обрабатываются через `errgroup` с `SetLimit(4)` — аналог `asyncio.gather` с семафором. При первой ошибке остальные запросы отменяются через context.
- `ListenAndServe` в `cmd/api` работает в горутине, а результат возвращается через канал.
- Сама обработка задач всё равно идёт в отдельном процессе-воркере (см. ниже).

**`run() error` в `main`.**
- `os.Exit` пропускает `defer`, поэтому вся логика вынесена в `run()`, а выход происходит в одном месте.
- Ресурсы закрываются через `defer` в обратном порядке создания.

## Очередь и надёжность

Обработка идёт в отдельном процессе `worker`, а не в горутине API. Горутины дешёвые, но отдельный процесс нужен ради надёжности:
- задачи переживают рестарт и деплой API;
- есть повторные попытки;
- API и воркеры масштабируются независимо, как Puma и Sidekiq.

Очередь — **[asynq](https://github.com/hibiken/asynq)**, ближайший аналог Sidekiq в Go.

**Задача не теряется при падении воркера.**
- В Python-версии `BRPOP` удаляет сообщение в момент получения, как в Sidekiq OSS.
- asynq Lua-скриптом атомарно переносит задачу в `active` и выдаёт аренду на 30 с, которую воркер продлевает, пока работает.
- Если процесс убит, recoverer возвращает задачу в очередь. Это ближе к `super_fetch` из Sidekiq Pro.
- Lua нужен потому, что переход затрагивает несколько ключей сразу: `MULTI` не умеет ветвиться, а `WATCH` требует повторов.

**Повторы.**
- 3 повтора с паузами 10 с, 20 с и 40 с (`analysis.RetryDelay`), таймаут одной попытки — 10 минут.
- Доменные ошибки (видео не найдено, комментарии отключены) не повторяются: задача сразу получает `failed`, а в asynq уходит `SkipRetry`.

**Задача не зависает в `processing`:**
- ошибка YouTube — сразу `failed` с понятным сообщением;
- временная ошибка на последней попытке — `failed` с `"Internal error"`, на таймауте — с `"Processing timed out"`;
- SIGTERM: воркер до 25 с ждёт текущие задачи, а незавершённые asynq возвращает в очередь. Статус `failed` при этом не ставится;
- `kill -9` на последней попытке: asynq архивирует задачу, не вызывая наш код. На этот случай `Service.Get` считает задачу в `processing` без обновлений дольше 15 минут упавшей и отдаёт `failed`;
- повторная доставка уже завершённой задачи (at-least-once) игнорируется.

**Блокирующее ожидание и таймаут чтения.** В Python-версии redis-py 8 рвал `BRPOP` через 5 с. Здесь этой проблемы нет по двум причинам:
- asynq вообще не использует блокирующие команды: он опрашивает очередь Lua-скриптом раз в `TaskCheckInterval` (1 с). Это проверяет тест `TestWorkerSurvivesIdleLongerThanReadTimeout`: воркер простаивает дольше `ReadTimeout`, а потом обрабатывает задачу;
- go-redis для блокирующих команд сам продлевает дедлайн чтения на время блокировки. Это зафиксировано тестом `TestGoRedisBlockingCommandIgnoresReadTimeout` на случай перехода на `BLMOVE` или `XREADGROUP`.

Задачи в очереди можно смотреть через [asynqmon](https://github.com/hibiken/asynqmon) (аналог Sidekiq Web) или CLI `asynq`. ID задачи в asynq совпадает с `task_id`.

## Выбор библиотек

| Задача | Библиотека | Почему |
|---|---|---|
| HTTP-роутинг | `net/http` | С Go 1.22 `ServeMux` понимает `GET /analysis/{task_id}`, для трёх маршрутов `chi` не нужен |
| Логи | `log/slog` | Стандартная библиотека, структурные поля `task_id=...` |
| Redis | `redis/go-redis/v9` | Стандарт де-факто, его же использует asynq (общий пул соединений) |
| Очередь | `hibiken/asynq` | Аналог Sidekiq: надёжная выдача, ретраи, таймауты, веб-интерфейс |
| Конфигурация | `caarlos0/env/v11` + `joho/godotenv` | Теги структуры, как pydantic-settings; диапазоны проверяет явный `validate()` |
| LLM | `openai/openai-go/v3` | Официальный SDK; один клиент для DeepSeek, OpenAI и любого OpenAI-совместимого API |
| Параллелизм | `golang.org/x/sync/errgroup` | Полустандартная библиотека: группа горутин с лимитом и отменой |
| UUID | `google/uuid` | Стандарт де-факто |
| Тесты | `testing`, `httptest`, `google/go-cmp`, `alicebob/miniredis` | Стандартный стиль Go; `cmp.Diff` для понятных диффов; Redis в памяти процесса |

Ретраи YouTube — небольшой собственный цикл (`youtube.fetchPageWithRetry`). Отдельная зависимость ради 20 строк не нужна.

**Структурированный ответ LLM.**
- Модели передаётся инструмент `submit_classifications` с JSON-схемой, где категории заданы как `enum`, и `tool_choice` требует его вызвать. Так же поступает pydantic-ai.
- `response_format: json_schema` не используется: DeepSeek его не поддерживает.
- Ответ дополнительно проверяется в Go. При невалидном ответе модель опрашивается ещё раз (одна повторная попытка, как в pydantic-ai).
- Сетевые ошибки, 429 и 5xx SDK повторяет сам (2 раза).

## Конфигурация

Переменные те же, что в Python-версии, см. [`.env.example`](.env.example):

| Переменная | По умолчанию | |
|---|---|---|
| `REDIS_URL` | `redis://localhost:6379/0` | в логах пароль маскируется |
| `CORS_ORIGINS` | `["http://localhost:5173"]` | JSON-список или через запятую |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`/`WARNING`, `ERROR` |
| `YOUTUBE_API_KEY` | — | обязательна; уходит в заголовок `X-Goog-Api-Key`, а не в query |
| `YOUTUBE_MAX_COMMENTS` | `200` | |
| `LLM_MODEL` | `openai:o3-mini` | `provider:model`; провайдеры: `deepseek`, `openai` |
| `LLM_API_KEY` | — | если пусто, берётся `<PROVIDER>_API_KEY`, как в pydantic-ai |
| `LLM_BATCH_SIZE` | `50` | |
| `TASK_TTL_SECONDS` | `86400` | |

`PYDANTIC_AI_NO_BANNER` относится только к Python и здесь игнорируется.

**Проверка при старте.** Конфигурация проверяется сразу, и при ошибке процесс падает с понятным сообщением, где перечислены все проблемы:

```
api: config: required environment variable "YOUTUBE_API_KEY" is not set
worker: llm: unknown provider "acme" (supported: deepseek, openai)
```

**Поиск `.env` не зависит от текущего каталога.** Файл ищется вверх от текущего каталога и от каталога бинарника, но не выше корня модуля (каталога с `go.mod`). В Docker файла нет, а переменные приходят из окружения. Переменные окружения процесса важнее значений из `.env`.

**Константы вместо новых переменных:**

| Константа | Значение | Где |
|---|---|---|
| лимит параллельных запросов к LLM | 4 | `analysis/classifier.go` |
| concurrency воркера | 4 | `cmd/worker` |
| таймаут одной попытки | 10 мин | `analysis/queue.go` |
| число повторов | 3 | `analysis/queue.go` |
| HTTP-таймаут YouTube | 15 с | `cmd/worker` |
| HTTP-таймаут LLM | 2 мин | `cmd/worker` |

Адрес API задаётся флагом `-addr` (по умолчанию `:8000`).

## Запуск

```bash
cp .env.example .env   # заполнить YOUTUBE_API_KEY, LLM_MODEL и LLM_API_KEY
```

**Локально** (нужны Go 1.27+ и Redis):

```bash
make run-api      # go run ./cmd/api
make run-worker   # go run ./cmd/worker — в отдельном терминале
# или собрать бинарники: make build && ./bin/api & ./bin/worker
```

**В Docker:**

```bash
make docker-up    # docker compose up --build: redis + api (порт 8000) + worker
```

- **Образ один** на оба сервиса: многоэтапная сборка, итог — `distroless/static:nonroot` с двумя статическими бинарниками без shell и пакетного менеджера.
- **Отличаются только точкой входа:** `/app/api` или `/app/worker`.
- **Порт Redis наружу не публикуется.** Заглянуть в Redis: `docker compose exec redis redis-cli`.

## Как проверить

```bash
make test         # go test -race ./... — Redis, YouTube и LLM не нужны
make lint         # go vet + golangci-lint (включает gofmt и goimports)
make test-cover   # покрытие
```

Что покрыто тестами:

| Что | Где | Как |
|---|---|---|
| разбор ссылок | `analysis/videourl_test.go` | табличный тест, 25 случаев |
| группировка | `analysis/grouping_test.go` | выдуманные и повторяющиеся id, неизвестные категории |
| сервис и анализатор | `analysis/service_test.go` | фейки интерфейсов; таблица исходов: успех, ошибка YouTube, временная ошибка, последняя попытка, таймаут, shutdown |
| классификатор | `analysis/classifier_test.go` | батчи, ограничение параллелизма, порядок, валидация ответа, комментарии передаются как JSON |
| хранилище | `analysis/repository_test.go` | miniredis; TTL, совместимость с записями Python-версии, Redis недоступен |
| клиент YouTube | `youtube/client_test.go` | `httptest`: пагинация до лимита, ключ в заголовке, ретраи 5xx/429, ошибки 404/403/400, отмена |
| клиент LLM | `llm/openai_test.go` | `httptest`: `tool_choice`, заголовок `Authorization`, токены |
| конфигурация | `config/config_test.go` | значения по умолчанию, ошибки, маскирование, поиск `.env` |
| HTTP API целиком | `httpapi/router_test.go` | контракт, 422/404/503, CORS |
| путь «API → очередь → воркер» | `test/e2e/pipeline_test.go` | настоящий asynq на miniredis |

Вручную:

```bash
curl -s localhost:8000/health
curl -s -X POST localhost:8000/api/v1/analysis -H 'Content-Type: application/json' \
     -d '{"url": "https://youtu.be/dQw4w9WgXcQ"}'
curl -s localhost:8000/api/v1/analysis/<task_id>
```

В логах каждый этап помечен `task_id`, так что «воркер не запущен» сразу отличается от «задача упала»:

```
api    msg="task created"  video_id=dQw4w9WgXcQ task_id=...
api    msg="task enqueued" queue=analysis task_id=...
worker msg="task received" attempt=1 max_attempts=4 task_id=...
worker msg="llm: batch done" batch=2/4 comments=50 items=50 duration=10.382s tokens_in=2361 tokens_out=2206 task_id=...
worker msg="task completed" duration=11.536s categories="map[Feedback:170 Questions:15 ...]" task_id=...
```

## Сравнение с Python-версией

| | Python (FastAPI) | Go |
|---|---|---|
| Структура | `src/app/analysis/` + `core/`, `clients/` | `internal/analysis/` + `youtube/`, `llm/`, `httpx/`, `config/` |
| Точки входа | `asgi.py` + `worker.py` (`taskiq worker app.worker:broker`) | `cmd/api`, `cmd/worker` — два бинарника в одном образе |
| Сборка зависимостей | `lifespan`, `app.state`, `Depends`, `dependency_overrides` в тестах | явно в `main`, конструкторы; в тестах передаются фейки |
| Абстракции | `Protocol` (`TaskQueue`, `CommentSource`, `CommentClassifier`) | интерфейсы у потребителя (`TaskStore`, `TaskQueue`, `CommentSource`, `Classifier`, `ToolCaller`) |
| Ошибки → HTTP | `AppError.status_code` + exception handlers | `apperr.Kind` в домене, код только в `httpx.WriteError` |
| Валидация | Pydantic (`HttpUrl`, `UUID`, `StrEnum`) | явные функции: `ParseVideoID`, `uuid.Parse`, `Category.Valid` |
| Ошибка 422 | `{"detail": [{"msg": ...}]}` (формат FastAPI) | `{"detail": "..."}`: фронтенд понимает оба формата, отличается только тело 422 |
| Очередь | taskiq + `ListQueueBroker` (`BRPOP`: задача теряется при падении воркера) | asynq: аренда и recoverer, задача переживает `kill -9` |
| Ретраи задачи | нет: любая ошибка сразу даёт `failed` | 3 повтора для временных ошибок; доменные — сразу `failed` |
| Таймаут чтения Redis | пришлось отключать `socket_timeout` | не нужен: asynq не блокируется, go-redis продлевает дедлайн сам |
| Параллельность LLM | `asyncio.gather` без ограничения | `errgroup.SetLimit(4)` с отменой при первой ошибке |
| Структурированный ответ | pydantic-ai, `output_type` | tool calling через openai-go + ручная проверка схемы |
| LLM-провайдеры | всё, что умеет pydantic-ai (включая нативный Anthropic) | только OpenAI-совместимые API; новый — одна строка в `llm/openai.go` |
| Логи | `logging`, `[task_id]` в тексте сообщения | `slog`, `task_id=` как поле, через context |
| Конфигурация | pydantic-settings, `.env` в текущем каталоге | `env` + `godotenv`, `.env` ищется до корня модуля |
| Секреты | `SecretStr` | `config.Secret` (`String`/`LogValue` печатают `***`) |
| Тесты | pytest, fakeredis, respx, `FunctionModel`, `InMemoryBroker` | `testing`, miniredis, `httptest`, фейки; e2e на настоящем asynq |
| Проверки | ruff, mypy `--strict` | `go vet`, golangci-lint; типы проверяет компилятор |
| Образ | `python:3.13-slim` + venv | distroless static, два бинарника, ~66 МБ из-за SDK openai-go |
| Хранение | `analysis:task:<uuid>`, JSON | тот же ключ и формат: одну базу читают обе версии |
