# YouTube Comments Analyzer — backend на Go

Реализация на Go того же сервиса, что и [`../backend-python`](../backend-python): по ссылке на
видео загружает комментарии через YouTube Data API, классифицирует их через LLM и отдаёт
результат, сгруппированный по категориям. HTTP-контракт, переменные окружения и схема
таблицы `analysis_tasks` совпадают с Python-версией, поэтому общий фронтенд работает без изменений.
Postgres и Redis общие для всех реализаций (`../docker-compose.yml`), у Go-версии база
`analysis_go` (тестовая — `analysis_go_test`) и Redis DB `1`.

Проект намеренно устроен как «средний, который будет расти»: домены, слои, интерфейсы,
явная сборка зависимостей. Ниже по ходу объясняются решения, неочевидные для тех, кто
пришёл из Ruby или Python.

## Структура

```
backend-go/
├── cmd/                       # точки входа: один каталог = один бинарник
│   ├── api/main.go            # HTTP-сервер (composition root API)
│   ├── worker/main.go         # воркер очереди (composition root воркера)
│   └── migrate/main.go        # создание базы и миграции: prepare, create, up, down, status
├── internal/                  # весь прикладной код; импортировать его снаружи модуля нельзя
│   ├── analysis/              # домен «анализ комментариев»: всё, что к нему относится
│   │   ├── model.go           #   Task, Comment, Status, Category, доменные ошибки, CommentSourceError
│   │   ├── videourl.go        #   ParseVideoID: watch?v=, youtu.be, /shorts, /embed, /live
│   │   ├── service.go         #   Service — сторона API (Start, Get) + интерфейсы TaskStore, TaskQueue
│   │   ├── analyzer.go        #   Analyzer — сторона воркера + интерфейсы CommentSource, Classifier
│   │   ├── classifier.go      #   LLMClassifier: промпт, JSON-схема, батчи, ограниченный параллелизм
│   │   ├── grouping.go        #   GroupByCategory: отбрасывает выдуманные и повторяющиеся id
│   │   ├── repository.go      #   PostgresRepository: Task <-> analysisdb.AnalysisTask
│   │   ├── analysisdb/        #   SQL-запросы (queries.sql) и сгенерированный sqlc код к ним
│   │   ├── queue.go           #   asynq: постановка задачи (API) и её обработчик (воркер)
│   │   ├── handler.go         #   HTTP-хендлер домена и схемы API (analyzeRequest, taskStatusResponse, ...)
│   │   └── analysistest/      #   фейки интерфейсов для тестов (как net/http/httptest)
│   ├── postgres/              # пул pgx, создание базы, миграции goose, классификация ошибок
│   │   ├── migrations/        #   SQL-миграции, вшиваются в бинарник (go:embed)
│   │   └── pgtest/            #   тестовая база: подготовка в TestMain, транзакция с откатом
│   ├── youtube/               # клиент YouTube Data API: пагинация, ретраи, ошибки 404/403/400
│   ├── llm/                   # провайдер-агностичный вызов LLM + OpenAI-совместимая реализация
│   ├── httpapi/               # сборка HTTP-приложения: маршруты, /health, middleware
│   ├── httpx/                 # JSON-ответы, единый маппинг ошибок в HTTP-коды, middleware
│   ├── apperr/                # прикладные ошибки с «видом» (NotFound, Invalid, Unavailable)
│   ├── config/                # чтение и валидация конфигурации, поиск .env, Secret
│   └── logging/               # slog + поля лога через context, адаптер логгера для asynq
├── test/e2e/                  # сквозной тест «API → очередь → воркер»
├── tools/go.mod               # инструменты разработки (sqlc) со своими зависимостями
├── sqlc.yaml                  # настройки генерации кода запросов
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
                                 youtube.Client   LLMClassifier ─► llm  PostgresRepository  AsynqQueue
                                                                          │
                                                                          ▼
                                                                analysisdb (sqlc) ─► pgx
```

- **Хендлер** знает только HTTP: декодирует JSON, разбирает `task_id`, вызывает сервис.
- **Service и Analyzer** не знают ни про HTTP, ни про Postgres, Redis и asynq. Они зависят от интерфейсов.
- **`youtube` зависит от домена, а не наоборот.** Клиент реализует `analysis.CommentSource`: возвращает доменный `analysis.Comment` и оборачивает свои ошибки в `analysis.CommentSourceError`. Анализатор проверяет `errors.As(err, &sourceErr)` и не импортирует `youtube`. Это dependency inversion: домен объявляет, что ему нужно, а инфраструктура подстраивается.
- **`llm`** ничего не знает о домене: это переиспользуемый клиент, домен адаптирует его через свой интерфейс `ToolCaller`.
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
- `PostgresRepository` нигде не пишет `implements TaskStore`: достаточно иметь методы `Save` и `Get`.
- Поэтому интерфейс объявляют там, где он нужен (`analysis.TaskStore`), и делают маленьким. Реализации о нём не знают.
- В тестах подставляются простые фейки из `analysistest` — без моков и без `dependency_overrides`.

**Composition root вместо DI.**
- В `main` создаются пул Postgres, Redis, HTTP-клиенты, сервисы, и всё связывается явно через конструкторы `NewXxx(...)`.
- Нет `Depends`, глобальных `get_settings()` и инициализации при импорте.
- Если хендлеру чего-то не хватает, программа не скомпилируется.

**`context.Context`.**
- Передаётся первым аргументом во все функции, которые делают ввод-вывод.
- Несёт сигнал отмены и дедлайн: при SIGTERM, таймауте задачи asynq или уходе клиента обрываются HTTP-запросы к YouTube и LLM, запросы к Postgres и команды Redis.
- Дополнительно через context в логи попадает `task_id` (`logging.With`), примерно как `Rails.logger.tagged`.
- Если сохранить статус `failed` нужно уже после отмены, используется `context.WithoutCancel` (`analyzer.go`).

**Ошибки — это значения.**
- Функция возвращает `error` последним результатом, и вызывающий проверяет его явно.
- Причины оборачиваются (`fmt.Errorf("fetch comments: %w", err)`) и проверяются по всей цепочке: `errors.Is(err, ErrTaskNotFound)` — аналог `rescue SomeError`, `errors.As(err, &sourceErr)` — достать типизированную ошибку (`*analysis.CommentSourceError` с сообщением для пользователя).
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

## Хранение задач

Задачи хранятся в Postgres, в таблице `analysis_tasks`, бессрочно. Redis — только очередь asynq.

| Колонка | Тип | |
|---|---|---|
| `id` | `uuid` | первичный ключ, он же `task_id` и ID задачи в asynq |
| `video_id` | `varchar(11)` | индекс `ix_analysis_tasks_video_id` |
| `status` | `varchar(16)` | `pending` → `processing` → `completed` / `failed` |
| `result` | `jsonb` | `{"Thanks": [{"id", "text"}]}`; `NULL`, пока результата нет |
| `error` | `text` | сообщение для пользователя у `failed` |
| `created_at`, `updated_at` | `timestamptz` | |

Схема совпадает с миграцией Alembic Python-версии, формат `result` тоже: строки, записанные одной версией, читает другая (это проверяет `TestPostgresRepositoryStorageFormat`).

**Три представления задачи — три типа:**

| Слой | Тип | Где |
|---|---|---|
| домен | `analysis.Task`, `analysis.Comment` | `analysis/model.go` |
| хранение | `analysisdb.AnalysisTask` (генерирует sqlc по схеме таблицы) | `analysis/analysisdb/` |
| API | `analyzeRequest`, `taskCreatedResponse`, `taskStatusResponse`, `commentResponse` | `analysis/handler.go` |

- В Python это `AnalysisTask`, `AnalysisTaskOrm` и `AnalyzeRequest` / `TaskStatusResponse` / `CommentSchema`.
- В Go имя пакета — часть имени: снаружи пишут `analysis.Task`, поэтому `analysis.AnalysisTask` было бы повтором. Модель хранения отличается пакетом (`analysisdb.`), а не суффиксом.
- Схемы API названы по роли единым шаблоном `<что><Request|Response>` и не экспортируются: за пределами хендлера они не нужны.
- У доменных типов нет JSON-тегов. Форму JSON задают слои, которым она нужна: HTTP (`commentResponse`), хранилище (`storedComment` в колонке `result`) и промпт LLM (`promptComment`).

**Репозиторий** (`analysis/repository.go`) — Data Mapper, как в Python-версии: переводит `Task` в параметры сгенерированного запроса и обратно.
- Каждая операция — своя короткая транзакция (`pgx.BeginFunc`: `COMMIT` при `nil`, иначе `ROLLBACK`). Соединение берётся из пула только на время запроса: обработка задачи в воркере длится минутами, держать соединение всё это время незачем.
- `Save` — upsert (`INSERT ... ON CONFLICT (id) DO UPDATE`), аналог `session.merge`. `created_at` при обновлении не меняется.
- Репозиторий зависит не от пула, а от маленького интерфейса `TxBeginner` с одним методом `Begin`. Ему удовлетворяют и `*pgxpool.Pool`, и `pgx.Tx`. У транзакции `Begin` создаёт `SAVEPOINT`, поэтому в тестах репозиторий без изменений работает внутри внешней транзакции, как `join_transaction_mode="create_savepoint"` в Python.
- Ошибки: `pgx.ErrNoRows` → `ErrTaskNotFound` (404). Недоступность базы → `ErrStorageUnavailable` (503): не удалось подключиться, разрыв, таймаут, а также ответы сервера классов `08` (соединение), `53` (`too_many_connections`), `57P` (сервер выключается) и `3D000` (базы нет), см. `postgres.IsUnavailable`. Ошибка самого запроса — внутренняя (500): это баг, а не повод сказать клиенту «попробуйте позже».

**SQL и sqlc.**
- Запросы пишутся руками в `analysis/analysisdb/queries.sql`, а [sqlc](https://sqlc.dev) генерирует по ним и по схеме из миграций типобезопасный Go-код: структуру `AnalysisTask`, `UpsertTaskParams` и методы `UpsertTask`, `GetTask`. Опечатка в имени колонки — ошибка генерации, а не падение в рантайме.
- Сгенерированный код лежит в репозитории, поэтому для сборки и тестов sqlc не нужен. После изменения SQL или миграций: `make generate`.
- sqlc — инструмент проекта, а не системная установка: версия зафиксирована в `tools/go.mod` (директива `tool`, Go 1.24+), а `go tool sqlc` сам скачивает и собирает её. Аналог гема в Gemfile с `bundle exec` или `devDependencies` с `npx`. Отдельный модуль нужен, чтобы тяжёлые зависимости sqlc (парсер Postgres) не попадали в `go.mod` приложения.

**Миграции и создание базы** — `cmd/migrate`, аналог `rails db:*`:

```bash
make db-prepare    # migrate prepare: создать базу, если её нет, и применить миграции (rails db:prepare)
make db-create     # migrate create  (rails db:create)
make db-migrate    # migrate up      (rails db:migrate)
make db-rollback   # migrate down    (rails db:rollback)
make db-status     # migrate status  (rails db:migrate:status)
```

- Миграции — SQL-файлы [goose](https://github.com/pressly/goose) в `internal/postgres/migrations` с секциями `-- +goose Up` / `-- +goose Down`. Через `go:embed` они вшиты в бинарник, так что образу не нужны ни исходники, ни отдельный CLI.
- Новая миграция — файл `00002_<что>.sql` рядом. Автогенерации по моделям, как `alembic revision --autogenerate`, нет: в Go нет ORM-моделей, схема описывается только SQL.
- Миграции идут под advisory lock Postgres: если их одновременно запустят два процесса, второй дождётся первого.
- В Postgres нет `CREATE DATABASE IF NOT EXISTS`, а саму команду нельзя выполнить ни в транзакции, ни подключившись к создаваемой базе. Поэтому `postgres.CreateDatabase` подключается к служебной базе `postgres`, проверяет `pg_database` и выполняет `CREATE DATABASE`. Если базу параллельно создал другой процесс (`42P04 duplicate_database`), это тоже успех.
- `migrate` читает только `DATABASE_URL` (`config.LoadDatabase`): ключи YouTube и LLM ему не нужны. Конфигурация разделена: `config.Database` встроена в `config.Config` (embedding), как `class Settings(DatabaseSettings)` в Python.

## Выбор библиотек

| Задача | Библиотека | Почему |
|---|---|---|
| HTTP-роутинг | `net/http` | С Go 1.22 `ServeMux` понимает `GET /analysis/{task_id}`, для трёх маршрутов `chi` не нужен |
| Логи | `log/slog` | Стандартная библиотека, структурные поля `task_id=...` |
| Postgres | `jackc/pgx/v5` + `pgxpool` | Стандарт де-факто для Postgres в Go: родные `jsonb`, `uuid`, `timestamptz`, коды ошибок в `*pgconn.PgError`. `database/sql` поверх него (`pgx/v5/stdlib`) нужен только goose |
| Запросы | `sqlc` (генератор, не зависимость) | SQL пишется руками, Go-код к нему генерируется. ORM (GORM) в Go не принят так широко, как ActiveRecord или SQLAlchemy |
| Миграции | `pressly/goose/v3` | Работает как библиотека (миграции вшиты в бинарник), понятен sqlc, advisory lock из коробки. `golang-migrate` удобнее как CLI, чем как библиотека |
| Redis | `redis/go-redis/v9` | Только очередь: стандарт де-факто, его же использует asynq (общий пул соединений) |
| Очередь | `hibiken/asynq` | Аналог Sidekiq: надёжная выдача, ретраи, таймауты, веб-интерфейс |
| Конфигурация | `caarlos0/env/v11` + `joho/godotenv` | Теги структуры, как pydantic-settings; диапазоны проверяет явный `validate()` |
| LLM | `openai/openai-go/v3` | Официальный SDK; один клиент для DeepSeek, OpenAI и любого OpenAI-совместимого API |
| Параллелизм | `golang.org/x/sync/errgroup` | Полустандартная библиотека: группа горутин с лимитом и отменой |
| UUID | `google/uuid` | Стандарт де-факто |
| Тесты | `testing`, `httptest`, `google/go-cmp`, `alicebob/miniredis` | Стандартный стиль Go; `cmp.Diff` для понятных диффов; БД — настоящий Postgres, miniredis — только для очереди |

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
| `REDIS_URL` | `redis://localhost:6379/1` | только очередь; в логах пароль маскируется |
| `DATABASE_URL` | `postgres://app:app@localhost:5432/analysis_go?sslmode=disable` | формат libpq URL (pgx, psql, goose); пароль в логах маскируется |
| `CORS_ORIGINS` | `["http://localhost:5173"]` | JSON-список или через запятую |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`/`WARNING`, `ERROR` |
| `YOUTUBE_API_KEY` | — | обязательна; уходит в заголовок `X-Goog-Api-Key`, а не в query |
| `YOUTUBE_MAX_COMMENTS` | `200` | |
| `LLM_MODEL` | `openai:o3-mini` | `provider:model`; провайдеры: `deepseek`, `openai` |
| `LLM_API_KEY` | — | если пусто, берётся `<PROVIDER>_API_KEY`, как в pydantic-ai |
| `LLM_BATCH_SIZE` | `50` | |

`PYDANTIC_AI_NO_BANNER` относится только к Python и здесь игнорируется.

- **`DATABASE_URL`** в Python записан в формате SQLAlchemy (`postgresql+asyncpg://...`), здесь — в обычном формате libpq. `sslmode=disable`: общий Postgres в Docker без TLS, иначе pgx (по умолчанию `prefer`) сначала пробует TLS, и в ошибках подключения появляются лишние строки.
- **`TEST_DATABASE_URL`** — только для тестов, по умолчанию `postgres://app:app@localhost:5432/analysis_go_test?sslmode=disable`. Задаётся отдельно, а не выводится из `DATABASE_URL`: опечатка в одной переменной не должна привести к тому, что тесты пишут в рабочую базу.
- Записи хранятся бессрочно, поэтому `TASK_TTL_SECONDS` больше нет.

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

Сначала общая инфраструктура (Redis и Postgres для всех реализаций), один раз:

```bash
docker compose -f ../docker-compose.yml up -d
cp .env.example .env   # заполнить YOUTUBE_API_KEY, LLM_MODEL и LLM_API_KEY
```

**Локально** (нужен Go 1.27+):

```bash
make db-prepare   # создать базу analysis_go и применить миграции
make run-api      # go run ./cmd/api
make run-worker   # go run ./cmd/worker — в отдельном терминале
# или собрать бинарники: make build && ./bin/migrate && ./bin/api & ./bin/worker
```

**В Docker:**

```bash
make docker-up    # общая инфраструктура, затем docker compose up --build: migrate → api (порт 8000) + worker
```

- **Свой Redis и Postgres не поднимаются.** Сервисы подключаются к сети `ayc-infra` как к внешней и ходят в `redis:6379/1` и `postgres:5432/analysis_go`.
- **`migrate` — одноразовый сервис**: создаёт базу, если её нет, и применяет миграции (`rails db:prepare`). `api` и `worker` стартуют только после его успешного завершения (`service_completed_successfully`).
- **Образ один** на все три сервиса: многоэтапная сборка, итог — `distroless/static:nonroot` с тремя статическими бинарниками без shell и пакетного менеджера. Отличаются только точкой входа: `/app/migrate`, `/app/api` или `/app/worker`.
- **Порт API — 8000**, как у Python-версии, поэтому одновременно их не запустить: сначала `docker compose down` в соседней папке.
- Заглянуть в базу: `docker compose -f ../docker-compose.yml exec postgres psql -U app analysis_go`.

## Как проверить

```bash
make test         # go test -race ./... — нужен общий Postgres; Redis, YouTube и LLM не нужны
make lint         # go vet + golangci-lint (включает gofmt и goimports)
make test-cover   # покрытие
```

**Тестовая база — настоящий Postgres**, как в Rails, а не SQLite или подмена в памяти: они скрывают различия СУБД (`jsonb`, `ON CONFLICT`, `timestamptz`).
- База `analysis_go_test` создаётся сама. Схема накатывается миграциями один раз на прогон пакета (`pgtest.Main` в `TestMain`, аналог `before(:suite)`): заодно проверяется, что миграции применяются.
- `go test ./...` запускает каждый пакет отдельным процессом, параллельно, поэтому «один раз на прогон» — это один раз на пакет. Создание базы идемпотентно, миграции идут под advisory lock, так что пакеты друг другу не мешают.
- Каждый тест работает в транзакции, которая откатывается в конце (`pgtest.Tx`, как `use_transactional_tests`). Код, который сам открывает транзакции, получает внутри неё savepoint.
- Исключение — e2e: API и воркер работают одновременно в разных горутинах, а транзакция — одно соединение, которое нельзя использовать конкурентно (та же оговорка есть в Python-версии). Там пул с настоящими коммитами, и тест удаляет свои задачи в `t.Cleanup`.
- Полный цикл миграций up → down → up проверяется в отдельной временной схеме тестовой базы: откат в общей схеме сломал бы параллельно идущие пакеты.
- Если Postgres не запущен, пакет останавливается одной строкой с подсказкой, а не десятком одинаковых ошибок соединения:

```
pgtest: test database is not ready: postgres: connect to "postgres": failed to connect to `user=app database=postgres`: ...
pgtest: is Postgres running? Start the shared infrastructure: docker compose -f ../docker-compose.yml up -d
```

Что покрыто тестами:

| Что | Где | Как |
|---|---|---|
| разбор ссылок | `analysis/videourl_test.go` | табличный тест, 25 случаев |
| группировка | `analysis/grouping_test.go` | выдуманные и повторяющиеся id, неизвестные категории |
| сервис и анализатор | `analysis/service_test.go` | фейки интерфейсов; таблица исходов: успех, ошибка источника, временная ошибка, последняя попытка, таймаут, shutdown |
| классификатор | `analysis/classifier_test.go` | батчи, ограничение параллелизма, порядок, валидация ответа, комментарии передаются как JSON |
| хранилище | `analysis/repository_test.go` | Postgres в транзакции: round trip, upsert, формат колонок (совместимость с Python), не найдено, база недоступна → 503, ошибка запроса → 500 |
| база и миграции | `postgres/postgres_test.go` | создание базы идемпотентно, миграции up → down → up, классификация ошибок |
| клиент YouTube | `youtube/client_test.go` | `httptest`: пагинация до лимита, ключ в заголовке, ретраи 5xx/429, ошибки 404/403/400 как `CommentSourceError`, отмена |
| клиент LLM | `llm/openai_test.go` | `httptest`: `tool_choice`, заголовок `Authorization`, токены |
| конфигурация | `config/config_test.go` | значения по умолчанию, ошибки, маскирование, поиск `.env`, `ParseDatabase` без ключей API |
| HTTP API целиком | `httpapi/router_test.go` | Postgres в транзакции: контракт, 422/404, 503 при недоступном Postgres или Redis, CORS |
| путь «API → очередь → воркер» | `test/e2e/pipeline_test.go` | настоящий asynq на miniredis, задачи в Postgres; итог проверяется и в таблице |

Вручную:

```bash
curl -s localhost:8000/health
curl -s -X POST localhost:8000/api/v1/analysis -H 'Content-Type: application/json' \
     -d '{"url": "https://youtu.be/dQw4w9WgXcQ"}'
curl -s localhost:8000/api/v1/analysis/<task_id>
```

В логах каждый этап помечен `task_id`, так что «воркер не запущен» сразу отличается от «задача упала»:

```
api    msg="postgres is available" postgres="postgres://app:xxxxx@postgres:5432/analysis_go?sslmode=disable"
api    msg="redis is available" redis=redis://redis:6379/1 queue=analysis
worker msg="worker ready" postgres=... redis=redis://redis:6379/1 queue=analysis concurrency=4 llm=deepseek:deepseek-chat
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
| Точки входа | `asgi.py` + `worker.py` (`taskiq worker app.worker:broker`) + `python -m app.db.create` и `alembic` | `cmd/api`, `cmd/worker`, `cmd/migrate` — три бинарника в одном образе |
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
| Доменная модель и схемы | `AnalysisTask` / `AnalysisTaskOrm` / `TaskStatusResponse`, `CommentSchema` | `analysis.Task` / `analysisdb.AnalysisTask` / `taskStatusResponse`, `commentResponse`: пакет вместо суффикса |
| Доступ к БД | SQLAlchemy 2 (async ORM, asyncpg) | pgx + sqlc: SQL руками, Go-код генерируется |
| Миграции | Alembic, `--autogenerate` по моделям | goose, SQL-файлы вшиты в бинарник; автогенерации нет |
| Создание базы | `python -m app.db.create` | `migrate create` / `migrate prepare` |
| Тесты | pytest, fakeredis, respx, `FunctionModel`, `InMemoryBroker`; Postgres с откатом через savepoint | `testing`, `httptest`, фейки, Postgres с откатом через savepoint (`pgtest`); miniredis — только очередь; e2e на настоящем asynq |
| Проверки | ruff, mypy `--strict` | `go vet`, golangci-lint; типы проверяет компилятор |
| Образ | `python:3.13-slim` + venv | distroless static, три бинарника, ~76 МБ (в основном SDK openai-go) |
| Хранение | Postgres `analysis_tasks`, бессрочно | та же схема и формат `result`; базы разные (`analysis_python` / `analysis_go`) |
