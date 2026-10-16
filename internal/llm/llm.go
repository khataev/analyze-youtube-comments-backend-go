// Package llm — провайдер-агностичный вызов LLM со структурированным ответом.
//
// Структурированный ответ получаем через принудительный вызов «инструмента»
// (function calling): модели передаётся JSON-схема аргументов, и она обязана
// вернуть JSON по этой схеме. Так делает и pydantic-ai. Почему не response_format
// json_schema: DeepSeek его не поддерживает, а tool calling есть у всех
// OpenAI-совместимых провайдеров.
//
// Пакет не знает про комментарии и категории — схему и промпт передаёт вызывающий.
package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolRequest — один вызов модели с обязательным вызовом инструмента.
type ToolRequest struct {
	System   string // инструкции (system prompt)
	User     string // данные пользователя
	ToolName string
	ToolDesc string
	Schema   map[string]any // JSON Schema аргументов инструмента (корень — object)
}

// ToolResponse — аргументы вызова инструмента (сырой JSON) и расход токенов.
type ToolResponse struct {
	Arguments json.RawMessage
	Usage     Usage
}

// Usage — расход токенов на запрос.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// Model — разобранная строка "провайдер:модель".
type Model struct {
	Provider string
	Name     string
}

func (m Model) String() string { return m.Provider + ":" + m.Name }

// ParseModel разбирает "deepseek:deepseek-chat".
func ParseModel(spec string) (Model, error) {
	provider, name, ok := strings.Cut(spec, ":")
	if !ok || provider == "" || name == "" {
		return Model{}, fmt.Errorf("llm: model must look like \"provider:model\", got %q", spec)
	}
	return Model{Provider: strings.ToLower(provider), Name: name}, nil
}
