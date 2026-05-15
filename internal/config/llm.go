// Package config — llm.go centralises the LLM provider configuration
// in one place so the rest of the codebase never reads env vars or
// JSON files directly. Two source layers, in order of precedence:
//
//  1. Environment variables (SCRIBE_LLM_*). Highest precedence so
//     deployments can override anything without touching files.
//  2. A JSON config file. Default location is <data_dir>/scribe-llm.json,
//     overridable via SCRIBE_LLM_CONFIG. The repo ships a sample at
//     scribe-llm.example.json — copy + fill in api_key and model.
//
// We use a JSON file (not YAML/TOML) on purpose: it's stdlib, it matches
// what most Chinese LLM dashboards copy-paste out of, and there's zero
// dependency footprint.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LLMConfig is the resolved LLM provider configuration. Wire format
// for the JSON file matches this struct one-to-one (snake_case via
// json tags). Any provider that speaks the OpenAI Chat Completions
// API works — point BaseURL at it and fill APIKey + Model. Tested
// against:
//
//   - 火山方舟 / Doubao: BaseURL "https://ark.cn-beijing.volces.com/api/v3"
//   - DeepSeek:          BaseURL "https://api.deepseek.com/v1"
//   - OpenAI:            BaseURL "https://api.openai.com/v1"
//   - 任意 OpenAI 兼容自部署: 填实际地址即可
type LLMConfig struct {
	BaseURL     string        `json:"base_url"`
	APIKey      string        `json:"api_key"`
	Model       string        `json:"model"`
	Timeout     time.Duration `json:"-"` // populated from TimeoutSeconds
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`

	// Internal: how the JSON file represents Timeout. We keep it
	// separate so callers always see a typed time.Duration.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// Optional pricing knobs for client-side cost estimation. Units:
	// yuan (¥) per 1 million tokens. Source: each provider's official
	// pricing page — we don't auto-discover. Leave at 0 to disable
	// the cost column in the UI.
	PriceInputPerMTok  float64 `json:"price_input_per_mtok,omitempty"`
	PriceOutputPerMTok float64 `json:"price_output_per_mtok,omitempty"`
}

// Enabled reports whether the LLM features should be exposed. The HTTP
// layer should return 503 for /api/jobs/{id}/summarize when this is
// false, with a friendly "configure scribe-llm.json or set SCRIBE_LLM_*"
// message.
func (c *LLMConfig) Enabled() bool {
	return c != nil && c.APIKey != "" && c.Model != "" && c.BaseURL != ""
}

// Redacted returns a copy safe to log or expose via /api/health: API
// key is masked to its first 4 + last 4 chars.
func (c *LLMConfig) Redacted() LLMConfig {
	if c == nil {
		return LLMConfig{}
	}
	out := *c
	out.APIKey = maskKey(c.APIKey)
	return out
}

func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "***"
	}
	return k[:4] + "…" + k[len(k)-4:]
}

// loadLLMConfig resolves the LLM section. It never returns an error
// for "unconfigured" — that's a valid state. It only errors when a
// config file exists but is malformed.
func loadLLMConfig(dataDir string) (*LLMConfig, error) {
	cfg := defaultLLMConfig()

	path := chooseLLMConfigPath(dataDir)
	if path != "" {
		if err := mergeLLMFromFile(cfg, path); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	mergeLLMFromEnv(cfg)
	finaliseLLMConfig(cfg)
	return cfg, nil
}

// defaultLLMConfig provides production-friendly defaults: the official
// 火山方舟 endpoint and a 60s timeout. APIKey + Model are intentionally
// blank — the user must opt in.
func defaultLLMConfig() *LLMConfig {
	return &LLMConfig{
		BaseURL:        "https://ark.cn-beijing.volces.com/api/v3",
		Timeout:        60 * time.Second,
		TimeoutSeconds: 60,
		MaxTokens:      4096,
		Temperature:    0.3,
	}
}

func chooseLLMConfigPath(dataDir string) string {
	if p := os.Getenv("SCRIBE_LLM_CONFIG"); p != "" {
		return p
	}
	candidates := []string{
		filepath.Join(dataDir, "scribe-llm.json"),
		"scribe-llm.json", // project-relative fallback for dev
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func mergeLLMFromFile(cfg *LLMConfig, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fromFile LLMConfig
	if err := json.Unmarshal(raw, &fromFile); err != nil {
		return err
	}
	if v := strings.TrimSpace(fromFile.BaseURL); v != "" {
		cfg.BaseURL = v
	}
	if v := strings.TrimSpace(fromFile.APIKey); v != "" {
		cfg.APIKey = v
	}
	if v := strings.TrimSpace(fromFile.Model); v != "" {
		cfg.Model = v
	}
	if fromFile.TimeoutSeconds > 0 {
		cfg.TimeoutSeconds = fromFile.TimeoutSeconds
	}
	if fromFile.MaxTokens > 0 {
		cfg.MaxTokens = fromFile.MaxTokens
	}
	if fromFile.Temperature > 0 {
		cfg.Temperature = fromFile.Temperature
	}
	if fromFile.PriceInputPerMTok > 0 {
		cfg.PriceInputPerMTok = fromFile.PriceInputPerMTok
	}
	if fromFile.PriceOutputPerMTok > 0 {
		cfg.PriceOutputPerMTok = fromFile.PriceOutputPerMTok
	}
	return nil
}

func mergeLLMFromEnv(cfg *LLMConfig) {
	if v := strings.TrimSpace(os.Getenv("SCRIBE_LLM_BASE_URL")); v != "" {
		cfg.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_LLM_API_KEY")); v != "" {
		cfg.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_LLM_MODEL")); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("SCRIBE_LLM_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("SCRIBE_LLM_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxTokens = n
		}
	}
	if v := os.Getenv("SCRIBE_LLM_TEMPERATURE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.Temperature = f
		}
	}
	if v := os.Getenv("SCRIBE_LLM_PRICE_INPUT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.PriceInputPerMTok = f
		}
	}
	if v := os.Getenv("SCRIBE_LLM_PRICE_OUTPUT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.PriceOutputPerMTok = f
		}
	}
}

func finaliseLLMConfig(cfg *LLMConfig) {
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 60
	}
	cfg.Timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
}
