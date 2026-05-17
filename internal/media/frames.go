// Package media — frames.go extracts keyframes from a finished video
// for downstream visual analysis. One ffmpeg invocation produces both
// the JPEGs on disk and a sidecar text file enumerating each kept
// frame's timestamp.
//
// Selection strategy is hybrid (BIBIGPT-style):
//
//   1. Scene-change detector: select frames where the inter-frame
//      content difference exceeds SceneThreshold (0.3–0.5 typical).
//   2. Interval floor: also select a frame whenever the gap since the
//      last selected frame exceeds IntervalSec — guarantees a minimum
//      sampling density for long static shots (lectures, vlogs).
//
// After ffmpeg finishes, the result is optionally down-sampled evenly
// across the timeline so the count never exceeds MaxFrames (the cost
// ceiling for the VLM stage).
package media

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"scribe-web/internal/config"
)

// FrameOptions controls keyframe selection. Zero values fall back to
// sensible defaults inside ExtractKeyframes.
type FrameOptions struct {
	SceneThreshold float64 // 0 disables scene-based selection
	IntervalSec    int     // 0 -> 15s default
	MaxFrames      int     // 0 -> no cap
	MaxWidth       int     // downscaled output width in pixels, 0 -> 960
}

// Frame is one extracted keyframe; the only contract callers care about.
type Frame struct {
	Index        int     `json:"index"`
	TimestampSec float64 `json:"timestamp_sec"`
	ImagePath    string  `json:"image_path"` // absolute path on disk
}

// ExtractKeyframes runs ffmpeg once over inputPath and returns the
// kept frames sorted by timestamp. outDir is created if missing and
// will contain frame_NNNN.jpg + frames.txt (the ffmpeg metadata sidecar).
func ExtractKeyframes(ctx context.Context, cfg *config.Config, inputPath, outDir string, opts FrameOptions) ([]Frame, error) {
	if err := config.RequireBin(cfg.FFmpegBin, "ffmpeg"); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	absInput, err := filepath.Abs(inputPath)
	if err != nil {
		return nil, fmt.Errorf("abs input: %w", err)
	}

	interval := opts.IntervalSec
	if interval <= 0 {
		interval = 15
	}
	width := opts.MaxWidth
	if width <= 0 {
		width = 960
	}

	// We run ffmpeg with cwd=outDir so all relative paths in the filter
	// chain (metadata=file=, output pattern) stay free of the colons
	// that confuse ffmpeg's filter syntax on absolute Windows / WSL
	// mounts.
	const metaFile = "frames.txt"
	_ = os.Remove(filepath.Join(outDir, metaFile)) // never reuse stale data

	// select expression. First frame is forced via isnan(prev_selected_t).
	// Subsequent frames: scene > threshold OR enough time elapsed.
	// Commas inside the expression must be escaped with \, so ffmpeg's
	// filter-chain parser does not split the expression.
	var selectExpr string
	if opts.SceneThreshold > 0 {
		selectExpr = fmt.Sprintf(
			"if(isnan(prev_selected_t)\\,1\\,gt(scene\\,%g)+gte(t-prev_selected_t\\,%d))",
			opts.SceneThreshold, interval,
		)
	} else {
		selectExpr = fmt.Sprintf(
			"if(isnan(prev_selected_t)\\,1\\,gte(t-prev_selected_t\\,%d))",
			interval,
		)
	}

	vf := fmt.Sprintf(
		"select=%s,metadata=mode=print:file=%s,scale=min(%d\\,iw):-2",
		selectExpr, metaFile, width,
	)

	cmd := exec.CommandContext(ctx, cfg.FFmpegBin,
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-i", absInput,
		"-vf", vf,
		"-an",
		"-vsync", "vfr",
		"-q:v", "4",
		"frame_%04d.jpg",
	)
	cmd.Dir = outDir
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return nil, fmt.Errorf("ffmpeg extract frames: %w\n%s", runErr, string(out))
	}

	timestamps, err := parseFrameMetadata(filepath.Join(outDir, metaFile))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", metaFile, err)
	}
	matches, err := filepath.Glob(filepath.Join(outDir, "frame_*.jpg"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)

	// ffmpeg sometimes prints metadata for a frame that was decoded but
	// then filtered out at the encode stage, or vice versa. Pair by the
	// shorter list so we never reference a non-existent file.
	n := len(matches)
	if len(timestamps) < n {
		n = len(timestamps)
	}
	frames := make([]Frame, 0, n)
	for i := 0; i < n; i++ {
		frames = append(frames, Frame{
			Index:        i,
			TimestampSec: timestamps[i],
			ImagePath:    matches[i],
		})
	}

	if opts.MaxFrames > 0 && len(frames) > opts.MaxFrames {
		frames = downsampleEvenly(frames, opts.MaxFrames)
	}
	return frames, nil
}

// ptsTimeRe extracts the seconds-with-fraction value from lines like
//
//	frame:12   pts:230400   pts_time:9.6
//
// which is what the metadata filter emits at print mode.
var ptsTimeRe = regexp.MustCompile(`pts_time:([0-9]+\.?[0-9]*)`)

func parseFrameMetadata(path string) ([]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []float64
	s := bufio.NewScanner(f)
	// Some ffmpeg versions emit a single long line per frame; raise the
	// buffer ceiling to be safe.
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		m := ptsTimeRe.FindStringSubmatch(s.Text())
		if m == nil {
			continue
		}
		t, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, s.Err()
}

// downsampleEvenly trims a frame list down to at most maxFrames while
// keeping the first and last frame (so opener / closer cues survive)
// and distributing the rest uniformly across the timeline. Returns a
// new slice; the input is left untouched.
func downsampleEvenly(in []Frame, maxFrames int) []Frame {
	if maxFrames <= 0 || len(in) <= maxFrames {
		return in
	}
	if maxFrames == 1 {
		out := []Frame{in[0]}
		out[0].Index = 0
		return out
	}
	keepIdx := make(map[int]struct{}, maxFrames)
	keepIdx[0] = struct{}{}
	keepIdx[len(in)-1] = struct{}{}
	// Pick maxFrames-2 evenly spaced indices strictly inside (0, len-1).
	if maxFrames >= 3 {
		for k := 1; k <= maxFrames-2; k++ {
			idx := k * (len(in) - 1) / (maxFrames - 1)
			keepIdx[idx] = struct{}{}
		}
	}
	out := make([]Frame, 0, len(keepIdx))
	for i, f := range in {
		if _, ok := keepIdx[i]; ok {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimestampSec < out[j].TimestampSec })
	for i := range out {
		out[i].Index = i
	}
	return out
}
