// Package config — notion.go centralises Notion export configuration.
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

// NotionConfig is the resolved Notion integration configuration.
// It supports two parent modes:
//  1. page_id
//  2. database_id (with optional property name mapping)
type NotionConfig struct {
	BaseURL        string        `json:"base_url,omitempty"`
	Token          string        `json:"token"`
	PageID         string        `json:"page_id,omitempty"`
	DatabaseID     string        `json:"database_id,omitempty"`
	NotionVersion  string        `json:"notion_version,omitempty"`
	Timeout        time.Duration `json:"-"`
	TimeoutSeconds int           `json:"timeout_seconds,omitempty"`

	// When parent is database_id, TitleProperty should point to a
	// "title" type database property (often "Name").
	TitleProperty string `json:"title_property,omitempty"`
	// Optional URL property name for source URL.
	URLProperty string `json:"url_property,omitempty"`
}

func (c *NotionConfig) Enabled() bool {
	if c == nil || strings.TrimSpace(c.Token) == "" {
		return false
	}
	hasPage := strings.TrimSpace(c.PageID) != ""
	hasDB := strings.TrimSpace(c.DatabaseID) != ""
	return hasPage || hasDB
}

func (c *NotionConfig) Redacted() NotionConfig {
	if c == nil {
		return NotionConfig{}
	}
	out := *c
	out.Token = maskKey(c.Token)
	return out
}

func defaultNotionConfig() *NotionConfig {
	return &NotionConfig{
		BaseURL:        "https://api.notion.com/v1",
		NotionVersion:  "2022-06-28",
		TimeoutSeconds: 25,
		TitleProperty:  "Name",
		URLProperty:    "",
	}
}

func loadNotionConfig(dataDir string) (*NotionConfig, error) {
	cfg := defaultNotionConfig()
	path := chooseNotionConfigPath(dataDir)
	if path != "" {
		if err := mergeNotionFromFile(cfg, path); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}
	mergeNotionFromEnv(cfg)
	finaliseNotionConfig(cfg)
	return cfg, nil
}

func chooseNotionConfigPath(dataDir string) string {
	if p := os.Getenv("SCRIBE_NOTION_CONFIG"); p != "" {
		return p
	}
	candidates := []string{
		filepath.Join(dataDir, "scribe-notion.json"),
		"scribe-notion.json",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func mergeNotionFromFile(cfg *NotionConfig, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fromFile NotionConfig
	if err := json.Unmarshal(raw, &fromFile); err != nil {
		return err
	}
	if v := strings.TrimSpace(fromFile.BaseURL); v != "" {
		cfg.BaseURL = v
	}
	if v := strings.TrimSpace(fromFile.Token); v != "" {
		cfg.Token = v
	}
	if v := strings.TrimSpace(fromFile.PageID); v != "" {
		cfg.PageID = v
	}
	if v := strings.TrimSpace(fromFile.DatabaseID); v != "" {
		cfg.DatabaseID = v
	}
	if v := strings.TrimSpace(fromFile.NotionVersion); v != "" {
		cfg.NotionVersion = v
	}
	if fromFile.TimeoutSeconds > 0 {
		cfg.TimeoutSeconds = fromFile.TimeoutSeconds
	}
	if v := strings.TrimSpace(fromFile.TitleProperty); v != "" {
		cfg.TitleProperty = v
	}
	if v := strings.TrimSpace(fromFile.URLProperty); v != "" {
		cfg.URLProperty = v
	}
	return nil
}

func mergeNotionFromEnv(cfg *NotionConfig) {
	if v := strings.TrimSpace(os.Getenv("SCRIBE_NOTION_BASE_URL")); v != "" {
		cfg.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_NOTION_TOKEN")); v != "" {
		cfg.Token = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_NOTION_PAGE_ID")); v != "" {
		cfg.PageID = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_NOTION_DATABASE_ID")); v != "" {
		cfg.DatabaseID = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_NOTION_VERSION")); v != "" {
		cfg.NotionVersion = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_NOTION_TITLE_PROPERTY")); v != "" {
		cfg.TitleProperty = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_NOTION_URL_PROPERTY")); v != "" {
		cfg.URLProperty = v
	}
	if v := strings.TrimSpace(os.Getenv("SCRIBE_NOTION_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TimeoutSeconds = n
		}
	}
}

func finaliseNotionConfig(cfg *NotionConfig) {
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 25
	}
	cfg.Timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.notion.com/v1"
	}
	if strings.TrimSpace(cfg.NotionVersion) == "" {
		cfg.NotionVersion = "2022-06-28"
	}
	// Prefer database when both are provided.
	if strings.TrimSpace(cfg.DatabaseID) != "" {
		cfg.PageID = ""
	}
	if strings.TrimSpace(cfg.TitleProperty) == "" {
		cfg.TitleProperty = "Name"
	}
}
