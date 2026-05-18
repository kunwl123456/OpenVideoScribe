package jobs

import (
	"errors"
	"testing"
	"time"

	"scribe-web/internal/config"
	"scribe-web/internal/store"
	"scribe-web/internal/vision"
)

func TestVisionCompletionDoesNotChangeMainPhase(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	finished := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	job := &store.Job{
		ID:           "done-job",
		URL:          "https://example.com/video",
		Model:        "base",
		Phase:        store.PhaseDone,
		FinishedAt:   &finished,
		VisionStatus: store.VisionRunning,
		CreatedAt:    finished.Add(-time.Minute),
	}
	if err := st.Create(job); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := NewManager(&config.Config{}, st, nil)

	m.finishVision("done-job", "frames/done-job", []vision.Insight{{Caption: "slide"}}, "画面理解完成：1 帧")

	got, ok := st.Get("done-job")
	if !ok {
		t.Fatal("job missing")
	}
	if got.Phase != store.PhaseDone {
		t.Fatalf("main phase = %s, want done", got.Phase)
	}
	if got.VisionStatus != store.VisionDone {
		t.Fatalf("vision status = %s, want done", got.VisionStatus)
	}
	if len(got.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(got.Frames))
	}
}

func TestVisionFailureDoesNotChangeMainPhase(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	finished := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	job := &store.Job{
		ID:           "done-job",
		URL:          "https://example.com/video",
		Model:        "base",
		Phase:        store.PhaseDone,
		FinishedAt:   &finished,
		VisionStatus: store.VisionRunning,
		CreatedAt:    finished.Add(-time.Minute),
	}
	if err := st.Create(job); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := NewManager(&config.Config{}, st, nil)

	m.failVision("done-job", errors.New("vlm timeout"))

	got, ok := st.Get("done-job")
	if !ok {
		t.Fatal("job missing")
	}
	if got.Phase != store.PhaseDone {
		t.Fatalf("main phase = %s, want done", got.Phase)
	}
	if got.VisionStatus != store.VisionFailed {
		t.Fatalf("vision status = %s, want failed", got.VisionStatus)
	}
	if got.VisionError != "vlm timeout" {
		t.Fatalf("vision error = %q, want vlm timeout", got.VisionError)
	}
}
