// Package config centralises every path / external binary / env knob
// the server needs at runtime. Keep it dependency-free so the rest of
// the codebase imports `config` without dragging anything in.
package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Config is a flat snapshot taken from env vars at boot.
type Config struct {
	Addr          string
	DataDir       string
	ModelsDir     string
	DownloadsDir  string
	ThumbnailsDir string
	FramesDir     string
	WorkDir       string
	WhisperBin    string
	FFmpegBin     string
	YtDlpBin      string
	ModelBaseURLs []string   // tried in order; first that succeeds wins
	LLM           *LLMConfig // OpenAI-compatible LLM provider, may be unconfigured
	VLM           *VLMConfig // OpenAI-compatible vision provider, may be unconfigured
}

// Load builds a Config from env vars, creating the data directories on
// disk as a side effect. Missing optional binaries are recorded but not
// fatal — the API will surface a clear error when a job actually needs
// the missing tool.
func Load() (*Config, error) {
	addr := envOr("SCRIBE_ADDR", ":8787")
	dataDir, err := resolveDataDir()
	if err != nil {
		return nil, err
	}

	c := &Config{
		Addr:          addr,
		DataDir:       dataDir,
		ModelsDir:     filepath.Join(dataDir, "models"),
		DownloadsDir:  filepath.Join(dataDir, "downloads"),
		ThumbnailsDir: filepath.Join(dataDir, "thumbnails"),
		FramesDir:     filepath.Join(dataDir, "frames"),
		WorkDir:       filepath.Join(dataDir, "work"),
		WhisperBin:    envOr("SCRIBE_WHISPER_BIN", ""),
		FFmpegBin:     envOr("SCRIBE_FFMPEG_BIN", ""),
		YtDlpBin:      envOr("SCRIBE_YTDLP_BIN", ""),
		ModelBaseURLs: parseModelBaseURLs(os.Getenv("WHISPER_MODEL_BASE_URL")),
	}

	for _, dir := range []string{c.ModelsDir, c.DownloadsDir, c.ThumbnailsDir, c.FramesDir, c.WorkDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	c.WhisperBin = resolveBinary(c.WhisperBin, "whisper-cli")
	c.FFmpegBin = resolveBinary(c.FFmpegBin, "ffmpeg")
	c.YtDlpBin = resolveBinary(c.YtDlpBin, "yt-dlp")

	llm, err := loadLLMConfig(c.DataDir)
	if err != nil {
		return nil, err
	}
	c.LLM = llm

	vlm, err := loadVLMConfig(c.DataDir)
	if err != nil {
		return nil, err
	}
	c.VLM = vlm

	return c, nil
}

// ModelFilePath returns the absolute path for a ggml model file given
// its key (tiny / base / small / medium).
func (c *Config) ModelFilePath(key string) string {
	return filepath.Join(c.ModelsDir, "ggml-"+key+".bin")
}

// RequireBin returns an error if the named binary wasn't located. Use
// at the start of a job step that needs the binary.
func RequireBin(path, name string) error {
	if path == "" {
		return fmt.Errorf("%s not found in PATH and no SCRIBE_%s override set", name, envSuffix(name))
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s at %s is not accessible: %w", name, path, err)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// defaultModelBaseURLs is the fallback list when WHISPER_MODEL_BASE_URL
// is unset. Order matters: official first, then a known China-friendly
// mirror so users behind the GFW still succeed without configuration.
var defaultModelBaseURLs = []string{
	"https://hf-mirror.com/ggerganov/whisper.cpp/resolve/main",
	"https://huggingface.co/ggerganov/whisper.cpp/resolve/main",
}

// parseModelBaseURLs accepts a comma-separated env value and returns
// the list to try in order. Empty input yields the built-in default.
func parseModelBaseURLs(raw string) []string {
	if raw == "" {
		return append([]string(nil), defaultModelBaseURLs...)
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(strings.TrimRight(p, "/")); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), defaultModelBaseURLs...)
	}
	return out
}

func envSuffix(name string) string {
	switch name {
	case "whisper-cli":
		return "WHISPER_BIN"
	case "ffmpeg":
		return "FFMPEG_BIN"
	case "yt-dlp":
		return "YTDLP_BIN"
	}
	return name
}

// resolveBinary prefers an explicit override path, otherwise looks the
// binary up on PATH. Returns "" if both miss; we don't fail boot — a
// later RequireBin call will surface the issue with context.
func resolveBinary(override, name string) string {
	if override != "" {
		return override
	}
	exeName := name
	if runtime.GOOS == "windows" && filepath.Ext(exeName) == "" {
		exeName += ".exe"
	}
	if p, err := exec.LookPath(exeName); err == nil {
		return p
	}
	return ""
}

// resolveDataDir picks a sensible default per OS, overridable via
// SCRIBE_DATA_DIR. We never let it land at "" — that would make the
// server happily write into the current working directory.
func resolveDataDir() (string, error) {
	if d := os.Getenv("SCRIBE_DATA_DIR"); d != "" {
		return d, nil
	}
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("APPDATA")
		if base == "" {
			return "", errors.New("APPDATA not set; pass SCRIBE_DATA_DIR explicitly")
		}
		return filepath.Join(base, "ScribeWeb"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "ScribeWeb"), nil
	default:
		// Linux / containers: prefer /var/lib/scribe-web in production,
		// otherwise XDG_DATA_HOME or ~/.local/share/scribe-web.
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return filepath.Join(d, "scribe-web"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", "scribe-web"), nil
	}
}
