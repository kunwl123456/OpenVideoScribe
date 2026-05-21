package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"scribe-web/internal/config"
	"scribe-web/internal/store"
	"scribe-web/internal/vision"
	"scribe-web/internal/vlm"
)

type stubVisionProvider struct{}

func (stubVisionProvider) Chat(context.Context, vlm.ChatRequest) (*vlm.ChatResponse, error) {
	return nil, nil
}

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

func TestJobVisionEnabledRequiresPerJobOptIn(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	vs := vision.New(stubVisionProvider{}, &config.VLMConfig{
		BaseURL: "http://example.invalid",
		APIKey:  "key",
		Model:   "vision-model",
	})
	m := NewManager(&config.Config{}, st, vs)

	if m.jobVisionEnabled(&store.Job{EnableVision: false}) {
		t.Fatal("vision enabled for unchecked job")
	}
	if !m.jobVisionEnabled(&store.Job{EnableVision: true}) {
		t.Fatal("vision disabled for checked job with configured service")
	}
}

func TestSubmitStoresUncheckedVisionAsDisabled(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	m := NewManager(&config.Config{}, st, nil)

	job, err := m.Submit("https://example.com/video", "tiny", "auto", false)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job.EnableVision {
		t.Fatal("EnableVision = true, want false")
	}
	if job.VisionStatus != store.VisionDisabled {
		t.Fatalf("VisionStatus = %s, want disabled", job.VisionStatus)
	}
}

func TestRetryWithBackoffEventuallySucceeds(t *testing.T) {
	attempts := 0
	got, err := retryWithBackoff(context.Background(), 2, time.Millisecond, nil, func() (string, error) {
		attempts++
		if attempts < 3 {
			return "", errors.New("temporary")
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("retryWithBackoff err: %v", err)
	}
	if got != "ok" {
		t.Fatalf("result = %q, want ok", got)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}
