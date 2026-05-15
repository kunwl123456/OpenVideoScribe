// Package llm is a tiny OpenAI-Chat-Completions client. Any provider
// that speaks the same wire protocol works: 火山方舟 / Doubao, DeepSeek,
// OpenAI, vLLM, Ollama (OpenAI-compatible mode), self-hosted forks.
//
// We deliberately don't use a third-party SDK. The protocol is small,
// the dependency footprint of net/http+encoding/json is zero, and we
// don't need streaming yet.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"scribe-web/internal/config"
)

// Sentinel errors. Callers map these to HTTP status codes — the API
// layer turns them into 401/429/502/504 so the React UI can render a
// useful hint without parsing prose.
var (
	ErrNoAPIKey    = errors.New("llm: api key not configured")
	ErrRateLimited = errors.New("llm: provider rate limited")
	ErrUpstream    = errors.New("llm: upstream error")
	ErrTimeout     = errors.New("llm: request timed out")
)

// Provider is what callers depend on. Interface lives next to its only
// production implementation here because we expect maybe two impls ever
// (real http + a test stub) — splitting packages just for that would be
// premature.
type Provider interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// openaiClient is the default HTTP implementation.
type openaiClient struct {
	baseURL string
	apiKey  string
	model   string
	maxTok  int
	temp    float64
	http    *http.Client
}

// New returns a Provider bound to the given LLM config. If cfg is nil
// or disabled, Chat will return ErrNoAPIKey on every call — the HTTP
// layer is expected to check cfg.Enabled() before calling here, this
// is just defence in depth.
func New(cfg *config.LLMConfig) Provider {
	if cfg == nil {
		return &openaiClient{}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
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

// Chat sends a synchronous completion request and parses the response.
// req.Model / Temperature / MaxTokens override the config defaults
// when set, so the summary layer can tune per-prompt without rebuilding
// the client.
func (c *openaiClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if c == nil || c.apiKey == "" {
		return nil, ErrNoAPIKey
	}
	if c.baseURL == "" {
		return nil, fmt.Errorf("llm: base_url not configured")
	}
	// Fill in defaults from the bound config so callers can stay terse.
	if req.Model == "" {
		req.Model = c.model
	}
	if req.Model == "" {
		return nil, fmt.Errorf("llm: model not configured")
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = c.maxTok
	}
	if req.Temperature == 0 {
		req.Temperature = c.temp
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}
	url := c.baseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// net/http surfaces both context cancellation and the client
		// Timeout this way. Map them to ErrTimeout so the API layer
		// returns 504 — easier on operators than a raw "context
		// deadline exceeded" leak.
		if errors.Is(err, context.DeadlineExceeded) || isTimeoutErr(err) {
			return nil, fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		return nil, fmt.Errorf("llm: http: %w", err)
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
		return nil, fmt.Errorf("llm: status %d: %s", resp.StatusCode, snippet(raw))
	}
	if readErr != nil {
		return nil, fmt.Errorf("llm: read body: %w", readErr)
	}

	var out ChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w (body: %s)", err, snippet(raw))
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return nil, fmt.Errorf("llm: empty completion (finish=%q)", choiceFinish(out))
	}
	return &out, nil
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

// isTimeoutErr unwraps the various wrappers net/http puts around timeouts
// (net.OpError, url.Error, ...) and asks the inner error if it considers
// itself a timeout.
func isTimeoutErr(err error) bool {
	type timeoutI interface{ Timeout() bool }
	var t timeoutI
	if errors.As(err, &t) {
		return t.Timeout()
	}
	return false
}
