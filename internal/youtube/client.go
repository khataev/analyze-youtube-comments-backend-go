// Package youtube — тонкий клиент YouTube Data API v3 (только то, что нужно сервису).
//
// Пакет ничего не знает о домене анализа: он возвращает комментарии и ошибки YouTube.
package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	defaultBaseURL = "https://www.googleapis.com/youtube/v3"
	pageSize       = 100 // максимум, который разрешает API

	maxAttempts    = 3
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 5 * time.Second
)

// Comment — комментарий верхнего уровня.
type Comment struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Error — ошибка YouTube, сообщение которой можно показать пользователю.
// Это постоянные ошибки: повтор запроса не поможет.
type Error struct {
	StatusCode int
	Message    string
}

func (e *Error) Error() string { return "youtube: " + e.Message }

// Сигнальные значения для errors.Is. Сообщения совпадают с Python-версией.
var (
	ErrVideoNotFound       = &Error{StatusCode: http.StatusNotFound, Message: "Video not found"}
	ErrCommentsUnavailable = &Error{
		StatusCode: http.StatusForbidden,
		Message:    "Comments are disabled for this video or API quota is exceeded",
	}
)

// Client ходит в API через общий *http.Client. Создаётся один раз на процесс,
// безопасен для использования из нескольких горутин.
type Client struct {
	http        *http.Client
	apiKey      string
	maxComments int
	baseURL     string
	logger      *slog.Logger
	sleep       func(context.Context, time.Duration) error
}

// Option — функциональная опция: идиоматичный в Go способ задавать необязательные
// параметры конструктора (в Python это были бы keyword-аргументы со значениями по умолчанию).
type Option func(*Client)

// WithBaseURL подменяет адрес API (для тестов через httptest).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithLogger задаёт логгер.
func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.logger = l } }

// WithoutBackoff отключает паузы между повторами (для тестов).
func WithoutBackoff() Option {
	return func(c *Client) { c.sleep = func(context.Context, time.Duration) error { return nil } }
}

// NewClient создаёт клиент. httpClient должен иметь таймауты — его создаёт main.
func NewClient(httpClient *http.Client, apiKey string, maxComments int, opts ...Option) *Client {
	c := &Client{
		http:        httpClient,
		apiKey:      apiKey,
		maxComments: maxComments,
		baseURL:     defaultBaseURL,
		logger:      slog.New(slog.DiscardHandler),
		sleep:       sleepContext,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// FetchComments загружает комментарии постранично, но не больше maxComments.
func (c *Client) FetchComments(ctx context.Context, videoID string) ([]Comment, error) {
	comments := make([]Comment, 0, min(c.maxComments, pageSize))
	pageToken := ""
	for len(comments) < c.maxComments {
		page, err := c.fetchPageWithRetry(ctx, videoID, pageToken)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			top := item.Snippet.TopLevelComment
			comments = append(comments, Comment{ID: top.ID, Text: top.Snippet.TextOriginal})
		}
		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	if len(comments) > c.maxComments {
		comments = comments[:c.maxComments]
	}
	return comments, nil
}

// threadsPage — только нужные поля ответа commentThreads.list.
// Лишние поля JSON-декодер Go просто игнорирует.
type threadsPage struct {
	NextPageToken string `json:"nextPageToken"`
	Items         []struct {
		Snippet struct {
			TopLevelComment struct {
				ID      string `json:"id"`
				Snippet struct {
					TextOriginal string `json:"textOriginal"`
				} `json:"snippet"`
			} `json:"topLevelComment"`
		} `json:"snippet"`
	} `json:"items"`
}

// retryableError помечает временные ошибки: сеть, 5xx, 429.
type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func (c *Client) fetchPageWithRetry(ctx context.Context, videoID, pageToken string) (*threadsPage, error) {
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		page, err := c.fetchPage(ctx, videoID, pageToken)
		var retryable *retryableError
		if err == nil || !errors.As(err, &retryable) || attempt == maxAttempts {
			return page, err
		}
		c.logger.WarnContext(ctx, "youtube request failed, retrying",
			"attempt", attempt, "max_attempts", maxAttempts, "backoff", backoff, "error", err)
		if err := c.sleep(ctx, backoff); err != nil {
			return nil, err
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (c *Client) fetchPage(ctx context.Context, videoID, pageToken string) (*threadsPage, error) {
	query := url.Values{
		"part":       {"snippet"},
		"videoId":    {videoID},
		"maxResults": {strconv.Itoa(pageSize)},
		"textFormat": {"plainText"},
	}
	if pageToken != "" {
		query.Set("pageToken", pageToken)
	}
	// context передаётся в запрос: при отмене (shutdown, таймаут задачи) запрос прервётся.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/commentThreads?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("youtube: build request: %w", err)
	}
	// Ключ в заголовке, а не в query: иначе он попадёт в логи и тексты ошибок (*url.Error содержит URL).
	req.Header.Set("X-Goog-Api-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // отмена — не повод для повтора
		}
		return nil, &retryableError{fmt.Errorf("youtube: %w", err)}
	}
	defer resp.Body.Close()

	if err := checkStatus(resp.StatusCode); err != nil {
		return nil, err
	}
	var page threadsPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("youtube: decode response: %w", err)
	}
	return &page, nil
}

func checkStatus(code int) error {
	switch {
	case code < 300:
		return nil
	case code == http.StatusNotFound:
		return ErrVideoNotFound
	case code == http.StatusForbidden:
		return ErrCommentsUnavailable
	case code == http.StatusTooManyRequests || code >= 500:
		return &retryableError{fmt.Errorf("youtube: unexpected status %d", code)}
	case code >= 400:
		return &Error{StatusCode: code, Message: fmt.Sprintf("YouTube API rejected the request (%d)", code)}
	default:
		return fmt.Errorf("youtube: unexpected status %d", code)
	}
}

// sleepContext — пауза, которую можно прервать отменой context
// (обычный time.Sleep отмену не замечает).
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
