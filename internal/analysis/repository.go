package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"ytcomments/internal/apperr"
)

const keyPrefix = "analysis:task:"

// RedisRepository хранит задачи в Redis: JSON по ключу analysis:task:<uuid> с TTL.
// Реализует TaskStore. Ошибки Redis наружу не выходят «как есть»: они
// превращаются в ErrStorageUnavailable (клиент получит 503), причина остаётся в цепочке.
type RedisRepository struct {
	rdb redis.UniversalClient
	ttl time.Duration
	now func() time.Time
}

// NewRedisRepository создаёт хранилище поверх общего клиента Redis.
func NewRedisRepository(rdb redis.UniversalClient, ttl time.Duration) *RedisRepository {
	return &RedisRepository{rdb: rdb, ttl: ttl, now: time.Now}
}

// Save сохраняет задачу и обновляет UpdatedAt.
func (r *RedisRepository) Save(ctx context.Context, task *Task) error {
	task.UpdatedAt = r.now().UTC()
	data, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("marshal task: %w", err)
	}
	if err := r.rdb.Set(ctx, key(task.ID), data, r.ttl).Err(); err != nil {
		return unavailable(err)
	}
	return nil
}

// Get читает задачу. Нет ключа — ErrTaskNotFound.
func (r *RedisRepository) Get(ctx context.Context, id uuid.UUID) (*Task, error) {
	data, err := r.rdb.Get(ctx, key(id)).Bytes()
	if errors.Is(err, redis.Nil) { // redis.Nil — «ключа нет», это не сбой
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, unavailable(err)
	}
	var task Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("decode task %s: %w", id, err)
	}
	return &task, nil
}

func key(id uuid.UUID) string { return keyPrefix + id.String() }

func unavailable(err error) error {
	return apperr.Wrap(ErrStorageUnavailable.Kind, ErrStorageUnavailable.Message, err)
}
