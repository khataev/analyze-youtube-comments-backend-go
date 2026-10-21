package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// providerBaseURLs — OpenAI-совместимые провайдеры. Новый провайдер добавляется строкой.
var providerBaseURLs = map[string]string{
	"openai":   "https://api.openai.com/v1",
	"deepseek": "https://api.deepseek.com",
}

// sdkMaxRetries — SDK сам повторяет запрос при сетевых ошибках, 408/409/429 и 5xx.
const sdkMaxRetries = 2

// ErrNoToolCall — модель ответила текстом вместо вызова инструмента.
var ErrNoToolCall = errors.New("llm: model did not call the tool")

// OpenAICompatible — клиент любого провайдера с OpenAI Chat Completions API.
type OpenAICompatible struct {
	client openai.Client
	model  Model
}

// Option — необязательная настройка клиента.
type Option func(*[]option.RequestOption)

// WithBaseURL переопределяет адрес провайдера (тесты через httptest, локальный прокси).
func WithBaseURL(u string) Option {
	return func(opts *[]option.RequestOption) { *opts = append(*opts, option.WithBaseURL(u)) }
}

// NewOpenAICompatible создаёт клиент для провайдера из model. Неизвестный провайдер
// или пустой ключ — ошибка при старте, а не при первой задаче.
func NewOpenAICompatible(model Model, apiKey string, httpClient *http.Client, extra ...Option) (*OpenAICompatible, error) {
	baseURL, ok := providerBaseURLs[model.Provider]
	if !ok {
		return nil, fmt.Errorf("llm: unknown provider %q (supported: %s)", model.Provider, supportedProviders())
	}
	if apiKey == "" {
		return nil, fmt.Errorf("llm: API key for provider %q is not set", model.Provider)
	}
	opts := []option.RequestOption{
		option.WithBaseURL(baseURL),
		option.WithAPIKey(apiKey), // уходит в заголовок Authorization: Bearer
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(sdkMaxRetries),
	}
	for _, o := range extra {
		o(&opts)
	}
	return &OpenAICompatible{client: openai.NewClient(opts...), model: model}, nil
}

// CallTool вызывает модель и заставляет её вызвать инструмент req.ToolName.
func (c *OpenAICompatible) CallTool(ctx context.Context, req ToolRequest) (ToolResponse, error) {
	params := openai.ChatCompletionNewParams{
		Model: c.model.Name,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(req.System),
			openai.UserMessage(req.User),
		},
		Tools: []openai.ChatCompletionToolUnionParam{
			openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{
				Name:        req.ToolName,
				Description: openai.String(req.ToolDesc),
				Parameters:  openai.FunctionParameters(req.Schema),
			}),
		},
		ToolChoice: openai.ToolChoiceOptionFunctionToolChoice(
			openai.ChatCompletionNamedToolChoiceFunctionParam{Name: req.ToolName},
		),
	}
	completion, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return ToolResponse{}, fmt.Errorf("llm: %s: %w", c.model, err)
	}
	usage := Usage{
		InputTokens:  completion.Usage.PromptTokens,
		OutputTokens: completion.Usage.CompletionTokens,
	}
	for _, choice := range completion.Choices {
		for _, call := range choice.Message.ToolCalls {
			if call.Function.Name == req.ToolName {
				return ToolResponse{Arguments: json.RawMessage(call.Function.Arguments), Usage: usage}, nil
			}
		}
	}
	return ToolResponse{Usage: usage}, ErrNoToolCall
}

func supportedProviders() string {
	names := make([]string, 0, len(providerBaseURLs))
	for name := range providerBaseURLs {
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}
