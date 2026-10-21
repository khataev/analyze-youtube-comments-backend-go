-- Схема совпадает с Python-версией (миграция Alembic cab607fd419c).

-- +goose Up
CREATE TABLE analysis_tasks (
    id         uuid PRIMARY KEY,
    video_id   varchar(11) NOT NULL,
    status     varchar(16) NOT NULL,
    -- jsonb: бинарный формат, по нему работают операторы ->>, @> и GIN-индексы
    result     jsonb,
    error      text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE INDEX ix_analysis_tasks_video_id ON analysis_tasks (video_id);

-- +goose Down
DROP TABLE analysis_tasks;
