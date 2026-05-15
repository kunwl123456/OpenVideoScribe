// Package media wraps ffmpeg for audio extraction. Whisper.cpp wants a
// 16 kHz mono PCM WAV; everything upstream of asr is normalised to
// that.
package media

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"scribe-web/internal/config"
)

// ExtractAudio writes a 16 kHz mono WAV next to (or beside, in workDir)
// the input file and returns the output path.
func ExtractAudio(ctx context.Context, cfg *config.Config, inputPath, workDir string) (string, error) {
	if err := config.RequireBin(cfg.FFmpegBin, "ffmpeg"); err != nil {
		return "", err
	}

	base := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))
	if workDir == "" {
		workDir = cfg.WorkDir
	}
	outPath := filepath.Join(workDir, base+".wav")

	cmd := exec.CommandContext(ctx, cfg.FFmpegBin,
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-i", inputPath,
		"-vn",
		"-ac", "1",
		"-ar", "16000",
		"-c:a", "pcm_s16le",
		outPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ffmpeg failed: %w\n%s", err, string(out))
	}
	return outPath, nil
}
