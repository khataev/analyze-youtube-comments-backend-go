package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"ytcomments/internal/analysis/analysisdb"
	"ytcomments/internal/postgres"
)

// TxBeginner — всё, что репозиторию нужно от базы: открыть транзакцию.
//
// Ему удовлетворяют и *pgxpool.Pool (в api и воркере), и pgx.Tx (в тестах). У pgx.Tx
// метод Begin создаёт SAVEPOINT, поэтому в тестах репозиторий без изменений работает
// внутри внешней транзакции, которая в конце откатывается.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// PostgresRepository хранит задачи в таблице analysis_tasks. Реализует TaskStore.
//
// Data Mapper: сервис и анализатор работают с доменным Task, а модель хранения —
// сгенерированная sqlc analysisdb.AnalysisTask; репозиторий переводит одно в другое.
//
// Каждая операция — своя короткая транзакция: соединение берётся из пула на время
// запроса и сразу возвращается. Обработка задачи в воркере длится минутами,
// держать соединение всё это время незачем.
type PostgresRepository struct {
	db  TxBeginner
	now func() time.Time
}

// NewPostgresRepository создаёт репозиторий поверх пула (или транзакции в тестах).
func NewPostgresRepository(db TxBeginner) *PostgresRepository {
	return &PostgresRepository{db: db, now: time.Now}
}

// Save создаёт задачу или обновляет существующую (upsert) и обновляет UpdatedAt.
//
// Время округляется до микросекунд — точности timestamptz: тогда задача в памяти
// совпадает с тем, что потом прочитает Get.
func (r *PostgresRepository) Save(ctx context.Context, task *Task) error {
	task.CreatedAt = task.CreatedAt.Truncate(time.Microsecond)
	task.UpdatedAt = r.now().UTC().Truncate(time.Microsecond)
	params, err := toStorage(task)
	if err != nil {
		return err
	}
	// BeginFunc: COMMIT, если функция вернула nil, иначе ROLLBACK (как `with session.begin()`).
	err = pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		return analysisdb.New(tx).UpsertTask(ctx, params)
	})
	if err != nil {
		return storageError("save task", err)
	}
	return nil
}

// Get читает задачу. Нет строки — ErrTaskNotFound.
func (r *PostgresRepository) Get(ctx context.Context, id uuid.UUID) (*Task, error) {
	var stored analysisdb.AnalysisTask
	err := pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		var err error
		stored, err = analysisdb.New(tx).GetTask(ctx, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) { // «строки нет» — это не сбой
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, storageError("get task", err)
	}
	return toDomain(stored)
}

// storedComment — форма комментария в колонке result (jsonb). Совпадает с Python-версией:
// {"Thanks": [{"id": "...", "text": "..."}]}.
type storedComment struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func toStorage(task *Task) (analysisdb.UpsertTaskParams, error) {
	params := analysisdb.UpsertTaskParams{
		ID:        task.ID,
		VideoID:   task.VideoID,
		Status:    string(task.Status),
		CreatedAt: task.CreatedAt,
		UpdatedAt: task.UpdatedAt,
	}
	if task.Error != "" {
		params.Error = &task.Error
	}
	if task.Result != nil { // nil — результата нет: в колонке NULL, а не {}
		stored := make(map[Category][]storedComment, len(task.Result))
		for category, comments := range task.Result {
			items := make([]storedComment, len(comments))
			for i, c := range comments {
				items[i] = storedComment(c)
			}
			stored[category] = items
		}
		data, err := json.Marshal(stored)
		if err != nil {
			return params, fmt.Errorf("encode result of task %s: %w", task.ID, err)
		}
		params.Result = data
	}
	return params, nil
}

func toDomain(stored analysisdb.AnalysisTask) (*Task, error) {
	task := &Task{
		ID:        stored.ID,
		VideoID:   stored.VideoID,
		Status:    Status(stored.Status),
		CreatedAt: stored.CreatedAt.UTC(), // pgx отдаёт время в локальной зоне процесса
		UpdatedAt: stored.UpdatedAt.UTC(),
	}
	if stored.Error != nil {
		task.Error = *stored.Error
	}
	if stored.Result != nil {
		var result map[Category][]storedComment
		if err := json.Unmarshal(stored.Result, &result); err != nil {
			return nil, fmt.Errorf("decode result of task %s: %w", stored.ID, err)
		}
		task.Result = make(Result, len(result))
		for category, items := range result {
			comments := make([]Comment, len(items))
			for i, item := range items {
				comments[i] = Comment(item)
			}
			task.Result[category] = comments
		}
	}
	return task, nil
}

// storageError: недоступность базы — ErrStorageUnavailable (клиент получит 503),
// прочие ошибки (неверный запрос, нарушение ограничений) — внутренние (500).
// Причина в обоих случаях остаётся в цепочке для логов.
func storageError(op string, err error) error {
	if postgres.IsUnavailable(err) {
		return unavailable(fmt.Errorf("%s: %w", op, err))
	}
	return fmt.Errorf("%s: %w", op, err)
}
