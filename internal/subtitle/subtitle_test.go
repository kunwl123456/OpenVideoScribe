package subtitle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFileSRT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "video.zh.srt")
	body := "1\n00:00:01,200 --> 00:00:03,400\n你好，世界\n\n2\n00:00:04,000 --> 00:00:05,500\n第二句\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write subtitle: %v", err)
	}

	res, err := ParseFile(path, "zh")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if res.Language != "zh" {
		t.Fatalf("language = %q, want zh", res.Language)
	}
	if res.Model != "platform-subtitle:zh" {
		t.Fatalf("model = %q, want platform-subtitle:zh", res.Model)
	}
	if len(res.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(res.Segments))
	}
	if res.Segments[0].Start != 1.2 || res.Segments[0].End != 3.4 {
		t.Fatalf("first timing = %.1f..%.1f, want 1.2..3.4", res.Segments[0].Start, res.Segments[0].End)
	}
	if res.FullText != "你好，世界\n第二句" {
		t.Fatalf("full text = %q", res.FullText)
	}
}

func TestParseFileWebVTT(t *testing.T) {
	path := filepath.Join(t.TempDir(), "video.en.vtt")
	body := "WEBVTT\n\ncue-1\n00:00:00.500 --> 00:00:02.000 align:start\n<c>Hello</c> &amp; welcome\n\n00:00:02.000 --> 00:00:04.250\nnext line\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write subtitle: %v", err)
	}

	res, err := ParseFile(path, "en")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(res.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(res.Segments))
	}
	if res.Segments[0].Text != "Hello & welcome" {
		t.Fatalf("first text = %q", res.Segments[0].Text)
	}
	if res.Duration != 4.25 {
		t.Fatalf("duration = %.2f, want 4.25", res.Duration)
	}
}
