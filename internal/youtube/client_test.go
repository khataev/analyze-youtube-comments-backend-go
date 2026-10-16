package youtube_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"ytcomments/internal/youtube"
)

// fakeAPI — httptest-сервер вместо YouTube (аналог respx в Python-версии).
// handler получает номер запроса, начиная с 1.
func fakeAPI(t *testing.T, handler func(n int, w http.ResponseWriter, r *http.Request)) (*youtube.Client, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(int(calls.Add(1)), w, r)
	}))
	t.Cleanup(srv.Close)
	client := youtube.NewClient(srv.Client(), "secret-key", 250,
		youtube.WithBaseURL(srv.URL), youtube.WithoutBackoff())
	return client, &calls
}

// page отдаёт n комментариев с ID, начинающимися с offset.
func page(w http.ResponseWriter, offset, n int, next string) {
	type snippet struct {
		TextOriginal string `json:"textOriginal"`
	}
	type top struct {
		ID      string  `json:"id"`
		Snippet snippet `json:"snippet"`
	}
	items := make([]map[string]any, n)
	for i := range items {
		id := strconv.Itoa(offset + i)
		items[i] = map[string]any{"snippet": map[string]any{
			"topLevelComment": top{ID: id, Snippet: snippet{TextOriginal: "text " + id}},
		}}
	}
	body := map[string]any{"items": items}
	if next != "" {
		body["nextPageToken"] = next
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func TestFetchCommentsPaginatesUpToLimit(t *testing.T) {
	client, calls := fakeAPI(t, func(n int, w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("key") != "" {
			t.Error("API key must not be sent in the query string")
		}
		if got := r.Header.Get("X-Goog-Api-Key"); got != "secret-key" {
			t.Errorf("X-Goog-Api-Key = %q", got)
		}
		if q.Get("videoId") != "vid" || q.Get("maxResults") != "100" {
			t.Errorf("unexpected query %v", q)
		}
		if want := map[int]string{1: "", 2: "p2", 3: "p3"}[n]; q.Get("pageToken") != want {
			t.Errorf("request %d: pageToken = %q, want %q", n, q.Get("pageToken"), want)
		}
		page(w, (n-1)*100, 100, fmt.Sprintf("p%d", n+1)) // страниц «бесконечно» много
	})

	comments, err := client.FetchComments(t.Context(), "vid")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 250 {
		t.Errorf("got %d comments, want 250 (the limit)", len(comments))
	}
	if calls.Load() != 3 {
		t.Errorf("requests = %d, want 3", calls.Load())
	}
	if comments[0] != (youtube.Comment{ID: "0", Text: "text 0"}) {
		t.Errorf("first comment = %+v", comments[0])
	}
}

func TestFetchCommentsStopsOnLastPage(t *testing.T) {
	client, calls := fakeAPI(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			page(w, 0, 100, "p2")
			return
		}
		page(w, 100, 5, "")
	})

	comments, err := client.FetchComments(t.Context(), "vid")
	if err != nil || len(comments) != 105 || calls.Load() != 2 {
		t.Fatalf("got %d comments, %d requests, err %v; want 105, 2, nil", len(comments), calls.Load(), err)
	}
}

func TestFetchCommentsRetries(t *testing.T) {
	tests := []struct {
		name      string
		failures  []int // статусы первых ответов, дальше — успех
		wantErr   bool
		wantCalls int32
	}{
		{"5xx then success", []int{http.StatusInternalServerError}, false, 2},
		{"429 then success", []int{http.StatusTooManyRequests, http.StatusBadGateway}, false, 3},
		{"gives up after 3 attempts", []int{503, 503, 503, 503}, true, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, calls := fakeAPI(t, func(n int, w http.ResponseWriter, _ *http.Request) {
				if n <= len(tt.failures) {
					w.WriteHeader(tt.failures[n-1])
					return
				}
				page(w, 0, 1, "")
			})

			_, err := client.FetchComments(t.Context(), "vid")

			if gotErr := err != nil; gotErr != tt.wantErr {
				t.Fatalf("error = %v, want error: %v", err, tt.wantErr)
			}
			if calls.Load() != tt.wantCalls {
				t.Errorf("requests = %d, want %d", calls.Load(), tt.wantCalls)
			}
		})
	}
}

func TestFetchCommentsPermanentErrors(t *testing.T) {
	tests := []struct {
		status      int
		wantMessage string
		wantIs      error
	}{
		{http.StatusNotFound, "Video not found", youtube.ErrVideoNotFound},
		{http.StatusForbidden, "Comments are disabled for this video or API quota is exceeded", youtube.ErrCommentsUnavailable},
		{http.StatusBadRequest, "YouTube API rejected the request (400)", nil},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			client, calls := fakeAPI(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			})

			_, err := client.FetchComments(t.Context(), "vid")

			var ytErr *youtube.Error
			if !errors.As(err, &ytErr) || ytErr.Message != tt.wantMessage {
				t.Fatalf("error = %v, want *youtube.Error %q", err, tt.wantMessage)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("errors.Is(%v, %v) = false", err, tt.wantIs)
			}
			if calls.Load() != 1 {
				t.Errorf("requests = %d, want 1 (no retries for client errors)", calls.Load())
			}
		})
	}
}

func TestFetchCommentsRespectsCancellation(t *testing.T) {
	client, calls := fakeAPI(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := client.FetchComments(ctx, "vid"); err == nil {
		t.Fatal("want error for cancelled context")
	}
	if calls.Load() > 1 {
		t.Errorf("requests = %d; cancelled context must stop retries", calls.Load())
	}
}
