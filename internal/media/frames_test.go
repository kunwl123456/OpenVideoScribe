package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"scribe-web/internal/config"
)

// findFFmpeg locates the ffmpeg binary or skips the test cleanly.
func findFFmpeg(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH; skipping integration test")
	}
	return p
}

// synth10sVideo generates a deterministic 10s 25fps mp4 in tmpDir using
// the ffmpeg "testsrc2" lavfi source. Returns the file path.
func synth10sVideo(t *testing.T, ffmpegBin, tmpDir string) string {
	t.Helper()
	out := filepath.Join(tmpDir, "input.mp4")
	cmd := exec.Command(ffmpegBin,
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=10",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "ultrafast",
		out,
	)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg synth (libx264 missing?) failed: %v\n%s", err, data)
	}
	return out
}

func TestExtractKeyframes_RealFFmpeg(t *testing.T) {
	ffmpegBin := findFFmpeg(t)
	tmp := t.TempDir()
	video := synth10sVideo(t, ffmpegBin, tmp)

	cfg := &config.Config{FFmpegBin: ffmpegBin}
	outDir := filepath.Join(tmp, "frames")
	frames, err := ExtractKeyframes(context.Background(), cfg, video, outDir, FrameOptions{
		SceneThreshold: 0.3,
		IntervalSec:    2,
		MaxFrames:      10,
	})
	if err != nil {
		t.Fatalf("ExtractKeyframes: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("no frames extracted")
	}
	if len(frames) > 10 {
		t.Errorf("frames %d > MaxFrames", len(frames))
	}
	prev := -1.0
	for i, f := range frames {
		if f.TimestampSec < prev {
			t.Errorf("not monotonic at %d: %v < %v", i, f.TimestampSec, prev)
		}
		prev = f.TimestampSec
		info, err := os.Stat(f.ImagePath)
		if err != nil {
			t.Errorf("frame %d image missing: %v", i, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("frame %d zero-byte image", i)
		}
		if f.Index != i {
			t.Errorf("frame index %d, want %d", f.Index, i)
		}
	}
}

func TestExtractKeyframes_MissingFFmpeg(t *testing.T) {
	cfg := &config.Config{FFmpegBin: ""}
	_, err := ExtractKeyframes(context.Background(), cfg, "in.mp4", t.TempDir(), FrameOptions{})
	if err == nil {
		t.Fatal("expected error when ffmpeg unset")
	}
}

func TestDownsampleEvenly(t *testing.T) {
	// 11 frames, ts 0..10
	in := make([]Frame, 11)
	for i := 0; i < 11; i++ {
		in[i] = Frame{Index: i, TimestampSec: float64(i)}
	}
	out := downsampleEvenly(in, 5)
	if len(out) != 5 {
		t.Fatalf("len = %d", len(out))
	}
	// First and last must survive.
	if out[0].TimestampSec != 0 {
		t.Errorf("first ts = %v", out[0].TimestampSec)
	}
	if out[len(out)-1].TimestampSec != 10 {
		t.Errorf("last ts = %v", out[len(out)-1].TimestampSec)
	}
	// Strictly sorted + reindexed.
	if !sort.SliceIsSorted(out, func(i, j int) bool { return out[i].TimestampSec < out[j].TimestampSec }) {
		t.Errorf("not sorted: %#v", out)
	}
	for i, f := range out {
		if f.Index != i {
			t.Errorf("reindex %d: %d", i, f.Index)
		}
	}
	// Passthrough when already under cap.
	if got := downsampleEvenly(in, 100); len(got) != len(in) {
		t.Errorf("passthrough len = %d", len(got))
	}
	// Edge case: cap of 1 keeps only the first frame.
	if got := downsampleEvenly(in, 1); len(got) != 1 || got[0].TimestampSec != 0 {
		t.Errorf("max=1 got %#v", got)
	}
}

func TestParseFrameMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frames.txt")
	body := `frame:0    pts:0       pts_time:0
lavfi.scene_score=0.000
frame:1    pts:240     pts_time:0.96
lavfi.scene_score=0.123
frame:2    pts:480     pts_time:1.92
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := parseFrameMetadata(path)
	if err != nil {
		t.Fatalf("parseFrameMetadata: %v", err)
	}
	want := []float64{0, 0.96, 1.92}
	if len(got) != len(want) {
		t.Fatalf("len = %d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
