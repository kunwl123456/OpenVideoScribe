// Package config — vlm.go centralises the VLM (vision language model)
// provider configuration. Same two-layer source-of-truth as llm.go:
//
//  1. Environment variables (SCRIBE_VLM_*). Highest precedence.
//  2. A JSON config file. Default <data_dir>/scribe-vlm.json, override
//     via SCRIBE_VLM_CONFIG. Sample shipped at scribe-vlm.example.json.
//
// We keep VLM as its own config (not a sub-section of LLM) because the
// model, pricing, context window and timeout always differ from the
// text LLM; mixing them would make the JSON ambiguous and force every
// override to specify "which one". An absent or invalid VLM config is
// a valid state — the pipeline simply skips the visual analysis step.
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

// VLMConfig is the resolved vision provider configuration. Wire format
// for scribe-vlm.json maps to this struct one-to-one (snake_case via
// json tags). Any provider that speaks OpenAI vision chat completions
// (multipart content with image_url parts) works. Tested against:
//
//   - 火山方舟 / Doubao Seed Vision: BaseURL "https://ark.cn-beijing.volces.com/api/v3"
//   - OpenAI gpt-4o / gpt-4o-mini: BaseURL "https://api.openai.com/v1"
type VLMConfig struct {
	BaseURL     string        `json:"base_url"`
	APIKey      string        `json:"api_key"`
	Model       string        `json:"model"`
	Timeout     time.Duration `json:"-"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`

	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// Pricing knobs (¥ per 1M tokens) — leave at 0 to skip cost text.
	PriceInputPerMTok  float64 `json:"price_input_per_mtok,omitempty"`
	PriceOutputPerMTok float64 `json:"price_output_per_mtok,omitempty"`

	// Vision-specific knobs.
	//
	// FrameIntervalSeconds is the minimum gap (seconds) between two
	// sampled keyframes. Acts as a "minimum density" floor on top of
	// the scene detector — guarantees a frame every N seconds even
	// for long talking-head shots with no cuts.
	FrameIntervalSeconds int `json:"frame_interval_seconds,omitempty"`

	// SceneThreshold is the ffmpeg select='gt(scene,X)' value. 0.3–0.5
	// is the typical sweet spot: 0.3 catches subtle slide changes, 0.5
	// only large cuts. 0 disables scene-based selection (interval only).
	SceneThreshold float64 `json:"scene_threshold,omitempty"`

	// MaxFrames is the hard upper bound per job — also the cost ceiling.
	// When the extractor yields more, we down-sample evenly across the
	// timeline so coverage stays uniform.
	MaxFrames int `json:"max_frames,omitempty"`

	// Concurrency limits how many VLM calls run in parallel for one job.
	// VLM calls are expensive and providers rate-limit aggressively; 4–8
	// is a friendly default. <=0 means "fall back to defaultVLMConcurrency".
	Concurrency int `json:"concurrency,omitempty"`
}

// Enabled reports whether the visual-analysis stage should run. The
// jobs layer checks this before extracting keyframes; the HTTP layer
// can also expose it via /api/health if needed.
func (c *VLMConfig) Enabled() bool {
	return c != nil && c.APIKey != "" && c.Model != "" && c.BaseURL != ""
}

// Redacted returns a copy safe to log or expose: APIKey masked.
func (c *VLMConfig) Redacted() VLMConfig {
	if c == nil {
		return VLMConfig{}
	}
	out := *c
	out.APIKey = maskKey(c.APIKey)
	return out
}

// loadVLMConfig mirrors loadLLMConfig. Never errors for "unconfigured"
// — only for malformed JSON files.
func loadVLMConfig(dataDir string) (*VLMConfig, error) {
	cfg := defaultVLMConfig()

	path := chooseVLMConfigPath(dataDir)
	if path != "" {
		if err := mergeVLMFromFile(cfg, path); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	mergeVLMFromEnv(cfg)
	finaliseVLMConfig(cfg)
	return cfg, nil
}

// defaultVLMConfig: same Doubao base URL as LLM by default (most users
// will reuse one ark API key) but no model — they must opt in.
func defaultVLMConfig() *VLMConfig {
	return &VLMConfig{
		BaseURL:              "https://ark.cn-beijing.volces.com/api/v3",
		Timeout:              90 * time.Second,
		TimeoutSeconds:       90,
		MaxTokens:            600,
		Temperature:          0.2,
		FrameIntervalSeconds: 15,
		SceneThreshold:       0.4,
		MaxFrames:            60,
		Concurrency:          4,
	}
}

func chooseVLMConfigPath(dataDir string) string {
	if p := os.Getenv("SCRIBE_VLM_CONFIG"); p != "" {
		return p
	}
	candidates := []string{
		filepath.Join(dataDir, "scribe-vlm.json"),
		"scribe-vlm.json",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func mergeVLMFromFile(cfg *VLMConfig, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fromFile VLMConfig
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
	if fromFile.FrameIntervalSeconds > 0 {
		cfg.FrameIntervalSeconds = fromFile.FrameIntervalSeconds
	}
	if fromFile.SceneThreshold > 0 {
		cfg.SceneThreshold = fromFile.SceneThreshold
	}
	if fromFile.MaxFrames > 0 {
		cfg.MaxFrames = fromFile.MaxFrames
	}
	if fromFile.Concurrency > 0 {
		cfg.Concurrency = fromFile.Concurrency
	}
	return nil
}

func mergeVLMFromEnv(cfg *VLMConfig) {
	if v := strings.TrimSpace(os.Getenv("SCRIBE_VLM_BASE_URL")); v != "" {
		cfg.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_VLM_API_KEY")); v != "" {
		cfg.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_VLM_MODEL")); v != "" {
		cfg.Model = v
	}
	if v := os.Getenv("SCRIBE_VLM_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("SCRIBE_VLM_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxTokens = n
		}
	}
	if v := os.Getenv("SCRIBE_VLM_TEMPERATURE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.Temperature = f
		}
	}
	if v := os.Getenv("SCRIBE_VLM_PRICE_INPUT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.PriceInputPerMTok = f
		}
	}
	if v := os.Getenv("SCRIBE_VLM_PRICE_OUTPUT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.PriceOutputPerMTok = f
		}
	}
	if v := os.Getenv("SCRIBE_VLM_FRAME_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.FrameIntervalSeconds = n
		}
	}
	if v := os.Getenv("SCRIBE_VLM_SCENE_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.SceneThreshold = f
		}
	}
	if v := os.Getenv("SCRIBE_VLM_MAX_FRAMES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxFrames = n
		}
	}
	if v := os.Getenv("SCRIBE_VLM_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Concurrency = n
		}
	}
}

func finaliseVLMConfig(cfg *VLMConfig) {
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 90
	}
	cfg.Timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.FrameIntervalSeconds <= 0 {
		cfg.FrameIntervalSeconds = 15
	}
	if cfg.MaxFrames <= 0 {
		cfg.MaxFrames = 60
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 4
	}
	if cfg.SceneThreshold < 0 {
		cfg.SceneThreshold = 0
	}
}
