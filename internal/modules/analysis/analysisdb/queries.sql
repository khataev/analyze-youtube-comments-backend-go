-- name: UpsertTask :exec
-- INSERT, если задачи ещё нет, иначе UPDATE. created_at при обновлении не меняется.
INSERT INTO analysis_tasks (id, video_id, status, result, error, created_at, updated_at)
VALUES (@id, @video_id, @status, @result, @error, @created_at, @updated_at)
ON CONFLICT (id) DO UPDATE SET
    video_id   = EXCLUDED.video_id,
    status     = EXCLUDED.status,
    result     = EXCLUDED.result,
    error      = EXCLUDED.error,
    updated_at = EXCLUDED.updated_at;

-- name: GetTask :one
SELECT id, video_id, status, result, error, created_at, updated_at
FROM analysis_tasks
WHERE id = @id;
