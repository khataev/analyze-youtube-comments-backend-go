package llm_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"ytcomments/internal/modules/llm"
)

func TestParseModel(t *testing.T) {
	tests := []struct {
		spec    string
		want    llm.Model
		wantErr bool
	}{
		{spec: "deepseek:deepseek-chat", want: llm.Model{Provider: "deepseek", Name: "deepseek-chat"}},
		{spec: "OpenAI:gpt-4o-mini", want: llm.Model{Provider: "openai", Name: "gpt-4o-mini"}},
		{spec: "openai:ft:gpt-4o:org:x", want: llm.Model{Provider: "openai", Name: "ft:gpt-4o:org:x"}},
		{spec: "gpt-4o", wantErr: true},
		{spec: ":model", wantErr: true},
		{spec: "openai:", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := llm.ParseModel(tt.spec)
			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("ParseModel(%q) error = %v, want error: %v", tt.spec, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseModel(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestNewOpenAICompatibleValidates(t *testing.T) {
	if _, err := llm.NewOpenAICompatible(llm.Model{Provider: "acme", Name: "m"}, "key", http.DefaultClient); err == nil {
		t.Error("unknown provider: want error")
	}
	if _, err := llm.NewOpenAICompatible(llm.Model{Provider: "deepseek", Name: "m"}, "", http.DefaultClient); err == nil {
		t.Error("empty API key: want error")
	}
}

func TestCallToolForcesToolCall(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "1", "object": "chat.completion", "created": 0, "model": "deepseek-chat",
			"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
				"role": "assistant", "content": null,
				"tool_calls": [{"id": "call_1", "type": "function",
					"function": {"name": "submit", "arguments": "{\"items\":[]}"}}]
			}}],
			"usage": {"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150}
		}`)
	}))
	defer srv.Close()

	client, err := llm.NewOpenAICompatible(llm.Model{Provider: "deepseek", Name: "deepseek-chat"}, "secret",
		srv.Client(), llm.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.CallTool(t.Context(), llm.ToolRequest{
		System: "sys", User: "[]", ToolName: "submit", ToolDesc: "d",
		Schema: map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if string(resp.Arguments) != `{"items":[]}` {
		t.Errorf("arguments = %s", resp.Arguments)
	}
	if diff := cmp.Diff(llm.Usage{InputTokens: 120, OutputTokens: 30}, resp.Usage); diff != "" {
		t.Errorf("usage mismatch (-want +got):\n%s", diff)
	}
	wantChoice := map[string]any{"type": "function", "function": map[string]any{"name": "submit"}}
	if diff := cmp.Diff(wantChoice, body["tool_choice"]); diff != "" {
		t.Errorf("tool_choice mismatch (-want +got):\n%s", diff)
	}
	if body["model"] != "deepseek-chat" {
		t.Errorf("model = %v", body["model"])
	}
}

func TestCallToolWithoutToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"1","object":"chat.completion","created":0,"model":"m",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer srv.Close()
	client, err := llm.NewOpenAICompatible(llm.Model{Provider: "openai", Name: "m"}, "k", srv.Client(), llm.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.CallTool(t.Context(), llm.ToolRequest{ToolName: "submit", Schema: map[string]any{"type": "object"}})
	if !errors.Is(err, llm.ErrNoToolCall) {
		t.Fatalf("error = %v, want ErrNoToolCall", err)
	}
}
