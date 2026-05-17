package vlm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scribe-web/internal/config"
)

func makeClient(t *testing.T, srv *httptest.Server, key string) Provider {
	t.Helper()
	return New(&config.VLMConfig{
		BaseURL: srv.URL,
		APIKey:  key,
		Model:   "test-vlm",
		Timeout: 2 * time.Second,
	})
}

func sampleRequest() ChatRequest {
	return ChatRequest{
		Messages: []ChatMessage{{
			Role: "user",
			Content: []ContentPart{
				{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/jpeg;base64,AAA"}},
				TextPart("describe this"),
			},
		}},
	}
}

func TestChat_OKAndPayloadShape(t *testing.T) {
	var got ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("auth header = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"x","model":"test-vlm","created":1,
			"choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"画面：测试\n文字：无"}}],
			"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}
		}`))
	}))
	defer srv.Close()

	c := makeClient(t, srv, "secret")
	resp, err := c.Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Choices[0].Message.Content != "画面：测试\n文字：无" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content)
	}
	if got.Model != "test-vlm" {
		t.Errorf("model on wire = %q", got.Model)
	}
	// Critical: messages[0].content must marshal as a JSON array,
	// not a bare string. Re-marshalling shows what the server saw.
	if len(got.Messages) != 1 {
		t.Fatalf("messages len = %d", len(got.Messages))
	}
	parts := got.Messages[0].Content
	if len(parts) != 2 {
		t.Fatalf("parts len = %d", len(parts))
	}
	if parts[0].Type != "image_url" || parts[0].ImageURL == nil {
		t.Errorf("first part not image_url: %#v", parts[0])
	}
	if !strings.HasPrefix(parts[0].ImageURL.URL, "data:image/jpeg;base64,") {
		t.Errorf("image url not data uri: %q", parts[0].ImageURL.URL)
	}
	if parts[1].Type != "text" || parts[1].Text != "describe this" {
		t.Errorf("second part not text: %#v", parts[1])
	}
}

func TestChat_ErrorMappings(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		status  int
		body    string
		wantErr error
		wantSub string
	}{
		{name: "rate limited", key: "k", status: 429, body: `{"error":"slow"}`, wantErr: ErrRateLimited, wantSub: "slow"},
		{name: "upstream 502", key: "k", status: 502, body: `oops`, wantErr: ErrUpstream, wantSub: "status 502"},
		{name: "unauthorized", key: "wrong", status: 401, body: ``, wantErr: ErrNoAPIKey},
		{name: "forbidden", key: "k", status: 403, body: ``, wantErr: ErrNoAPIKey},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := makeClient(t, srv, tc.key)
			_, err := c.Chat(context.Background(), sampleRequest())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want errors.Is %v", err, tc.wantErr)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err %q missing %q", err, tc.wantSub)
			}
		})
	}
}

func TestChat_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := makeClient(t, srv, "k")
	_, err := c.Chat(context.Background(), sampleRequest())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestChat_EmptyCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"  "}}]}`))
	}))
	defer srv.Close()
	c := makeClient(t, srv, "k")
	_, err := c.Chat(context.Background(), sampleRequest())
	if err == nil || !strings.Contains(err.Error(), "empty completion") {
		t.Fatalf("err = %v, want empty completion", err)
	}
}

func TestNoAPIKey(t *testing.T) {
	c := New(&config.VLMConfig{BaseURL: "http://example.invalid", Model: "x"})
	_, err := c.Chat(context.Background(), sampleRequest())
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("err = %v, want ErrNoAPIKey", err)
	}
}

func TestEncodeImage_JPGBase64(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frame.jpg")
	// minimal but valid JPEG SOI marker is enough; we never decode here.
	payload := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'O', 'K', 0xFF, 0xD9}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	part, err := EncodeImage(path)
	if err != nil {
		t.Fatalf("EncodeImage: %v", err)
	}
	if part.Type != "image_url" || part.ImageURL == nil {
		t.Fatalf("part shape: %#v", part)
	}
	const prefix = "data:image/jpeg;base64,"
	if !strings.HasPrefix(part.ImageURL.URL, prefix) {
		t.Fatalf("bad prefix: %q", part.ImageURL.URL)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(part.ImageURL.URL, prefix))
	if err != nil {
		t.Fatalf("decode b64: %v", err)
	}
	if string(decoded) != string(payload) {
		t.Errorf("roundtrip mismatch: %v vs %v", decoded, payload)
	}
}

func TestTextPart(t *testing.T) {
	p := TextPart("hello")
	if p.Type != "text" || p.Text != "hello" || p.ImageURL != nil {
		t.Errorf("TextPart shape: %#v", p)
	}
}
