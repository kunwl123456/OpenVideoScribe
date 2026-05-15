// Package ytdlp wraps the yt-dlp CLI. Today we only need "download
// best audio + give me the file path and metadata"; that keeps the
// surface area small.
package ytdlp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"scribe-web/internal/config"
)

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

// Download fetches the bestaudio variant of `url` into outDir and
// returns the resulting file path + metadata. We use --no-playlist so
// pasting a playlist URL doesn't quietly explode into N downloads.
func Download(ctx context.Context, cfg *config.Config, url, outDir string, onProgress func(string)) (*Result, error) {
	if err := config.RequireBin(cfg.YtDlpBin, "yt-dlp"); err != nil {
		return nil, err
	}

	tmpl := filepath.Join(outDir, "%(id)s.%(ext)s")
	args := []string{
		"--no-playlist",
		"--no-warnings",
		"--newline",
		"--print-json",
		"-f", "bestaudio/best",
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
