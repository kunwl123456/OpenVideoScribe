// Package ytdlp wraps the yt-dlp CLI. Today we only need "download
// best audio + give me the file path and metadata"; that keeps the
// surface area small.
package ytdlp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"scribe-web/internal/config"
)

// FormatAudioOnly is yt-dlp's format selector for "best audio stream
// only" — the historical default when this server only did ASR. It
// keeps downloads small (a few MB for a 15-minute talk) at the cost of
// no video track, which is fine when the visual stage is disabled.
const FormatAudioOnly = "bestaudio/best"

// FormatVideoPlusAudio asks yt-dlp to mux the best video and best
// audio streams into one container (typically .mp4). Required when
// the visual stage is enabled, because ffmpeg keyframe extraction
// needs an actual video stream — a pure m4a triggers
// "Output file #0 does not contain any stream" and the analyzer
// returns zero frames.
const FormatVideoPlusAudio = "bestvideo*+bestaudio/best"

// Info is the subset of yt-dlp's --print-json output we care about.
// Engagement counters (view/like/...) are filled by yt-dlp for both
// YouTube and bilibili, but each platform omits different fields —
// expect nil/zero on the ones the source didn't expose. The UI hides
// any zero-valued counter.
type Info struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Uploader      string  `json:"uploader"`
	Duration      float64 `json:"duration"`
	Webpage       string  `json:"webpage_url"`
	Thumbnail     string  `json:"thumbnail,omitempty"`
	ViewCount     int64   `json:"view_count,omitempty"`
	LikeCount     int64   `json:"like_count,omitempty"`
	CommentCount  int64   `json:"comment_count,omitempty"`
	FavoriteCount int64   `json:"favorite_count,omitempty"` // mainly bilibili 收藏
	RepostCount   int64   `json:"repost_count,omitempty"`   // bilibili 分享
}

// Result is what Download returns.
type Result struct {
	Info     Info
	FilePath string
}

// SubtitleResult is a single platform-provided subtitle file downloaded
// by yt-dlp. Kind is "manual" for creator subtitles and "auto" for
// generated captions.
type SubtitleResult struct {
	Path     string
	Language string
	Kind     string
}

// Download fetches the bestaudio variant of `url` into outDir and
// returns the resulting file path + metadata. We use --no-playlist so
// pasting a playlist URL doesn't quietly explode into N downloads.
// Download runs yt-dlp against url and writes a single media file
// into outDir. The format parameter is forwarded verbatim to yt-dlp's
// "-f" flag; pass "" to fall back to FormatAudioOnly so the
// pre-visual-stage call sites keep working unchanged.
func Download(ctx context.Context, cfg *config.Config, url, outDir, format string, onProgress func(string)) (*Result, error) {
	if err := config.RequireBin(cfg.YtDlpBin, "yt-dlp"); err != nil {
		return nil, err
	}
	if format == "" {
		format = FormatAudioOnly
	}

	tmpl := filepath.Join(outDir, "%(id)s.%(ext)s")
	args := []string{
		"--no-playlist",
		"--no-warnings",
		"--newline",
		"--print-json",
		"-f", format,
		"-o", tmpl,
		url,
	}
	cmd := exec.CommandContext(ctx, cfg.YtDlpBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	go pumpProgress(stderr, onProgress)

	var infoLine string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "{") {
			infoLine = line
		} else if onProgress != nil {
			onProgress(line)
		}
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("yt-dlp failed: %w", err)
	}

	if infoLine == "" {
		return nil, fmt.Errorf("yt-dlp produced no metadata for %s", url)
	}
	var info Info
	if err := json.Unmarshal([]byte(infoLine), &info); err != nil {
		return nil, fmt.Errorf("parse yt-dlp json: %w", err)
	}
	if info.ID == "" {
		return nil, fmt.Errorf("yt-dlp metadata missing id")
	}
	path, err := findDownloaded(outDir, info.ID)
	if err != nil {
		return nil, err
	}
	return &Result{Info: info, FilePath: path}, nil
}

// DownloadSubtitle tries to fetch platform-provided subtitles without
// downloading media. It prefers creator-provided subtitles over automatic
// captions and returns ErrNoSubtitles when neither exists in the desired
// language set.
func DownloadSubtitle(ctx context.Context, cfg *config.Config, url, outDir, sourceID, language string, onProgress func(string)) (*SubtitleResult, error) {
	if err := config.RequireBin(cfg.YtDlpBin, "yt-dlp"); err != nil {
		return nil, err
	}
	if sourceID == "" {
		return nil, fmt.Errorf("source id required")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	langs := subtitleLanguageSpec(language)
	kinds := []struct {
		name string
		arg  string
	}{
		{name: "manual", arg: "--write-subs"},
		{name: "auto", arg: "--write-auto-subs"},
	}
	var lastErr error
	for _, kind := range kinds {
		cleanSubtitleFiles(outDir, sourceID)
		args := []string{
			"--no-playlist",
			"--no-warnings",
			"--skip-download",
			"--sub-format", "srt/vtt/best",
			"--sub-langs", langs,
			kind.arg,
			"-o", filepath.Join(outDir, "%(id)s.%(ext)s"),
			url,
		}
		if err := runSubtitleCommand(ctx, cfg.YtDlpBin, args, onProgress); err != nil {
			lastErr = err
		}
		sub, err := findSubtitle(outDir, sourceID, language)
		if err == nil {
			sub.Kind = kind.name
			return sub, nil
		}
		if lastErr == nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("no platform subtitles found: %w", lastErr)
	}
	return nil, fmt.Errorf("no platform subtitles found")
}

func runSubtitleCommand(ctx context.Context, bin string, args []string, onProgress func(string)) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if onProgress != nil {
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				onProgress(line)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("yt-dlp subtitle download failed: %w\n%s", err, string(out))
	}
	return nil
}

func subtitleLanguageSpec(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "", "auto":
		return "zh-Hans,zh-CN,zh,zh-TW,zh-Hant,zh.*,en,en.*,ja,ja.*"
	case "zh", "zh-cn", "zh-hans":
		return "zh-Hans,zh-CN,zh,zh.*,zh-TW,zh-Hant"
	case "en":
		return "en,en.*"
	case "ja", "jp":
		return "ja,ja.*"
	default:
		return language + "," + language + ".*"
	}
}

func cleanSubtitleFiles(outDir, sourceID string) {
	for _, pattern := range []string{
		filepath.Join(outDir, sourceID+".*.srt"),
		filepath.Join(outDir, sourceID+".*.vtt"),
		filepath.Join(outDir, sourceID+".srt"),
		filepath.Join(outDir, sourceID+".vtt"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, p := range matches {
			_ = os.Remove(p)
		}
	}
}

func findSubtitle(outDir, sourceID, language string) (*SubtitleResult, error) {
	var matches []string
	for _, pattern := range []string{
		filepath.Join(outDir, sourceID+".*.srt"),
		filepath.Join(outDir, sourceID+".*.vtt"),
		filepath.Join(outDir, sourceID+".srt"),
		filepath.Join(outDir, sourceID+".vtt"),
	} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		matches = append(matches, found...)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("subtitle file for %s not found in %s", sourceID, outDir)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return subtitleScore(matches[i], sourceID, language) < subtitleScore(matches[j], sourceID, language)
	})
	path := matches[0]
	return &SubtitleResult{
		Path:     path,
		Language: subtitleLanguageFromPath(path, sourceID),
	}, nil
}

func subtitleScore(path, sourceID, language string) int {
	lang := strings.ToLower(subtitleLanguageFromPath(path, sourceID))
	score := 100
	for i, pref := range subtitlePreferences(language) {
		p := strings.ToLower(strings.TrimSuffix(pref, ".*"))
		if lang == p {
			score = i
			break
		}
		if strings.HasSuffix(pref, ".*") && strings.HasPrefix(lang, p) && i+20 < score {
			score = i + 20
		}
	}
	if filepath.Ext(path) == ".srt" {
		score--
	}
	return score
}

func subtitlePreferences(language string) []string {
	return strings.Split(subtitleLanguageSpec(language), ",")
}

func subtitleLanguageFromPath(path, sourceID string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	prefix := sourceID + "."
	if strings.HasPrefix(name, prefix) {
		return strings.TrimPrefix(name, prefix)
	}
	return ""
}

func pumpProgress(r io.Reader, cb func(string)) {
	if cb == nil {
		_, _ = io.Copy(io.Discard, r)
		return
	}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		cb(line)
	}
}

// findDownloaded scans outDir for a file named `<id>.*`. yt-dlp picks
// the extension based on the chosen format, so we can't predict it
// up-front.
func findDownloaded(outDir, id string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(outDir, id+".*"))
	if err != nil {
		return "", err
	}
	for _, m := range matches {
		// Skip yt-dlp's residual .part and .info.json files.
		if strings.HasSuffix(m, ".part") || strings.HasSuffix(m, ".info.json") {
			continue
		}
		return m, nil
	}
	return "", fmt.Errorf("downloaded file for %s not found in %s", id, outDir)
}
