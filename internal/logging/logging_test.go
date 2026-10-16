package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestContextAttrsAreAddedToRecords(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo)

	ctx := With(context.Background(), "task_id", "t-1")
	child := With(ctx, "batch", 2)
	logger.InfoContext(child, "batch done")
	logger.InfoContext(ctx, "task done")
	logger.Info("no context")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	checks := []struct {
		want, notWant string
	}{
		{want: "task_id=t-1 batch=2"},
		{want: "task_id=t-1", notWant: "batch="}, // дочерний context не меняет родительский
		{notWant: "task_id"},
	}
	for i, c := range checks {
		if c.want != "" && !strings.Contains(lines[i], c.want) {
			t.Errorf("line %d = %q, want it to contain %q", i, lines[i], c.want)
		}
		if c.notWant != "" && strings.Contains(lines[i], c.notWant) {
			t.Errorf("line %d = %q, must not contain %q", i, lines[i], c.notWant)
		}
	}
}
