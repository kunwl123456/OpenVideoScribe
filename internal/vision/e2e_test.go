//go:build e2e

// e2e_test.go is the component-level smoke test for the VLM pipeline.
// It is gated behind `go test -tags=e2e` so default CI / dev runs stay
// fast and dependency-free; the test requires a real ffmpeg binary on
// PATH and uses an httptest server to stand in for the upstream VLM
// provider.
//
// What it proves:
//   1. ffmpeg on this platform accepts our select expression and emits
//      the metadata sidecar in the expected format.
//   2. media.ExtractKeyframes parses that sidecar and produces frames
//      with monotonically increasing timestamps.
//   3. vision.Service feeds those frames through the multi-modal HTTP
//      contract correctly (image_url + text content parts, response
//      content as plain string).
//   4. parseReply turns the captioned response back into Caption + OCR.
//
// To run inside Debian bookworm (matches the production runtime):
//   docker run --rm -v $PWD:/src -w /src golang:1.23-bookworm sh -c \
//     'apt-get update && apt-get install -y --no-install-recommends ffmpeg \
//      && go test -tags=e2e ./internal/vision/...'
package vision

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"scribe-web/internal/config"
	"scribe-web/internal/media"
	"scribe-web/internal/vlm"
)

func TestE2E_FramesAndVision(t *testing.T) {
	ffmpegBin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH; skipping e2e")
	}
	tmp := t.TempDir()

	// 1. Synth a deterministic 8s mp4 with the testsrc2 source.
	videoPath := filepath.Join(tmp, "in.mp4")
	cmd := exec.Command(ffmpegBin,
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=10:duration=8",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "ultrafast",
		videoPath,
	)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg synth (libx264 missing?) failed: %v\n%s", err, data)
	}

	// 2. Extract keyframes via our real ffmpeg pipeline.
	cfg := &config.Config{FFmpegBin: ffmpegBin}
	framesDir := filepath.Join(tmp, "frames")
	frames, err := media.ExtractKeyframes(context.Background(), cfg, videoPath, framesDir, media.FrameOptions{
		SceneThreshold: 0.3,
		IntervalSec:    2,
		MaxFrames:      10,
	})
	if err != nil {
		t.Fatalf("ExtractKeyframes: %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("frames=%d, want >=2 (synth video too short or filter broken)", len(frames))
	}

	// 3. Mock VLM upstream — assert payload shape, return a canned reply.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		var req vlm.ChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(req.Messages) < 2 {
			t.Errorf("messages len = %d", len(req.Messages))
		} else {
			parts := req.Messages[1].Content
			if len(parts) != 2 {
				t.Errorf("user parts = %d, want image + text", len(parts))
			}
			if len(parts) > 0 && parts[0].Type != "image_url" {
				t.Errorf("first part type = %q", parts[0].Type)
			}
			if len(parts) > 0 && parts[0].ImageURL != nil &&
				!strings.HasPrefix(parts[0].ImageURL.URL, "data:image/jpeg;base64,") {
				t.Errorf("first part not data uri: %q", parts[0].ImageURL.URL)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"index":0,"finish_reason":"stop",
			"message":{"role":"assistant","content":"画面：testsrc2 测试图\n文字：无"}}],
			"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}
		}`))
	}))
	defer srv.Close()

	// 4. vision.Service → Insights.
	vCfg := &config.VLMConfig{
		BaseURL:     srv.URL,
		APIKey:      "test-key",
		Model:       "test-vision",
		Concurrency: 2,
		Timeout:     5 * time.Second,
	}
	svc := New(vlm.New(vCfg), vCfg)
	insights, err := svc.Describe(context.Background(), frames, nil)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != int32(len(frames)) {
		t.Errorf("upstream calls = %d, want %d", got, len(frames))
	}
	if len(insights) != len(frames) {
		t.Fatalf("insights = %d, want %d", len(insights), len(frames))
	}
	prev := -1.0
	for i, in := range insights {
		if in.TimestampSec < prev {
			t.Errorf("not sorted at %d: %v < %v", i, in.TimestampSec, prev)
		}
		prev = in.TimestampSec
		if in.Caption == "" {
			t.Errorf("frame %d empty caption", i)
		}
		if in.Error != "" {
			t.Errorf("frame %d unexpected error: %s", i, in.Error)
		}
	}
}
