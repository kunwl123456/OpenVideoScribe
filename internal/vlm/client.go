// Package vlm is a tiny OpenAI-Vision-Chat-Completions client. Any
// provider that speaks the multipart-content protocol works:
// 火山方舟 Doubao Vision / OpenAI gpt-4o / vLLM with vision models / etc.
//
// We deliberately don't use a third-party SDK — the protocol is small,
// the dependency footprint of net/http+encoding/json is zero, and the
// error mapping is identical to internal/llm so callers get a familiar
// shape.
package vlm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"scribe-web/internal/config"
)

// Sentinel errors. Identical shape to internal/llm.Err* so the HTTP
// layer can map both providers to the same status codes without a
// special case.
var (
	ErrNoAPIKey    = errors.New("vlm: api key not configured")
	ErrRateLimited = errors.New("vlm: provider rate limited")
	ErrUpstream    = errors.New("vlm: upstream error")
	ErrTimeout     = errors.New("vlm: request timed out")
)

// Provider is what callers depend on. Interface lives next to the only
// production implementation here because we expect at most two impls
// ever (real http + a test stub).
type Provider interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

type openaiClient struct {
	baseURL string
	apiKey  string
	model   string
	maxTok  int
	temp    float64
	http    *http.Client
}

// New returns a Provider bound to the given VLM config. nil / disabled
// cfg yields a client whose Chat always returns ErrNoAPIKey — the
// vision pipeline upstream is expected to check cfg.Enabled() first,
// this is just defence in depth.
func New(cfg *config.VLMConfig) Provider {
	if cfg == nil {
		return &openaiClient{}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &openaiClient{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		maxTok:  cfg.MaxTokens,
		temp:    cfg.Temperature,
		http:    &http.Client{Timeout: timeout},
	}
}

// Chat sends one synchronous vision completion. req.Model / Temperature
// / MaxTokens override the config defaults when set, mirroring the llm
// package so callers can tune per-frame without rebuilding the client.
func (c *openaiClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if c == nil || c.apiKey == "" {
		return nil, ErrNoAPIKey
	}
	if c.baseURL == "" {
		return nil, fmt.Errorf("vlm: base_url not configured")
	}
	if req.Model == "" {
		req.Model = c.model
	}
	if req.Model == "" {
		return nil, fmt.Errorf("vlm: model not configured")
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = c.maxTok
	}
	if req.Temperature == 0 {
		req.Temperature = c.temp
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("vlm: marshal request: %w", err)
	}
	url := c.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vlm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeoutErr(err) {
			return nil, fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		return nil, fmt.Errorf("vlm: http: %w", err)
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: %s", ErrRateLimited, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: status %d", ErrNoAPIKey, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("vlm: status %d: %s", resp.StatusCode, snippet(raw))
	}
	if readErr != nil {
		return nil, fmt.Errorf("vlm: read body: %w", readErr)
	}

	var out ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("vlm: decode response: %w (body: %s)", err, snippet(raw))
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return nil, fmt.Errorf("vlm: empty completion (finish=%q)", choiceFinish(out))
	}
	return &out, nil
}

// EncodeImage reads a local image file and returns a ContentPart with a
// data: URI suitable for embedding in a ChatMessage. We support the
// extensions actually emitted by media.ExtractKeyframes (jpg) plus a
// permissive fallback so callers can pass png/webp/jpeg if a future
// extractor changes the output format.
//
// The whole image is loaded into memory; keyframes are downscaled by
// ffmpeg to ≤960px wide so this stays under ~200 KiB per frame even
// for high-bitrate sources.
func EncodeImage(path string) (ContentPart, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ContentPart{}, fmt.Errorf("vlm: read image %s: %w", path, err)
	}
	mime := mimeFromExt(path)
	uri := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
	return ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: uri}}, nil
}

// TextPart is a tiny constructor so call sites read cleanly.
func TextPart(s string) ContentPart {
	return ContentPart{Type: "text", Text: s}
}

func mimeFromExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".jpeg":
		return "image/jpeg"
	case ".jpg":
		return "image/jpeg"
	default:
		return "image/jpeg"
	}
}

func choiceFinish(r ChatResponse) string {
	if len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].FinishReason
}

func snippet(b []byte) string {
	const max = 240
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

func isTimeoutErr(err error) bool {
	type timeoutI interface{ Timeout() bool }
	var t timeoutI
	if errors.As(err, &t) {
		return t.Timeout()
	}
	return false
}
