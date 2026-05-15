package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"scribe-web/internal/config"
)

func makeClient(t *testing.T, srv *httptest.Server, key string) Provider {
	t.Helper()
	return New(&config.LLMConfig{
		BaseURL: srv.URL,
		APIKey:  key,
		Model:   "test-model",
		Timeout: 2 * time.Second,
	})
}

func TestChat(t *testing.T) {
	cases := []struct {
		name      string
		key       string
		handler   http.HandlerFunc
		wantErr   error
		wantSub   string
		wantReply string
	}{
		{
			name: "ok",
			key:  "secret",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer secret" {
					t.Errorf("auth header = %q", got)
				}
				if got := r.Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("content-type = %q", got)
				}
				body, _ := io.ReadAll(r.Body)
				var req ChatRequest
				if err := json.Unmarshal(body, &req); err != nil {
					t.Fatalf("decode req: %v", err)
				}
				if req.Model != "test-model" {
					t.Errorf("model = %q", req.Model)
				}
				if len(req.Messages) == 0 || req.Messages[0].Role != "user" {
					t.Errorf("messages = %#v", req.Messages)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"id":"x","model":"test-model","created":1,
					"choices":[{"index":0,"finish_reason":"stop",
					"message":{"role":"assistant","content":"hello world"}}],
					"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
				}`))
			},
			wantReply: "hello world",
		},
		{
			name: "rate limited",
			key:  "secret",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			},
			wantErr: ErrRateLimited,
			wantSub: "slow down",
		},
		{
			name: "upstream 500",
			key:  "secret",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":"oh no"}`))
			},
			wantErr: ErrUpstream,
			wantSub: "status 502",
		},
		{
			name: "unauthorized",
			key:  "wrong",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
			wantErr: ErrNoAPIKey,
		},
		{
			name: "timeout",
			key:  "secret",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// Sleep longer than the 2s client timeout
				time.Sleep(3 * time.Second)
				w.WriteHeader(200)
			},
			wantErr: ErrTimeout,
		},
		{
			name: "empty content",
			key:  "secret",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":""}}]}`))
			},
			wantSub: "empty completion",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			c := makeClient(t, srv, tc.key)
			ctx := context.Background()
			resp, err := c.Chat(ctx, ChatRequest{
				Messages: []ChatMessage{{Role: "user", Content: "hi"}},
			})

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
				}
				if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("err %q missing %q", err, tc.wantSub)
				}
				return
			}
			if err != nil && tc.wantSub != "" {
				if !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("err %q missing %q", err, tc.wantSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if resp.Choices[0].Message.Content != tc.wantReply {
				t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, tc.wantReply)
			}
		})
	}
}

func TestNoAPIKey(t *testing.T) {
	c := New(&config.LLMConfig{BaseURL: "http://example.invalid", Model: "x"})
	_, err := c.Chat(context.Background(), ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("err = %v, want ErrNoAPIKey", err)
	}
}
