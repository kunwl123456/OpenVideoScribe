// Package vision — service.go fans frames out to the VLM provider with
// a bounded concurrency, surfaces per-frame progress, and returns the
// results sorted by timestamp.
//
// Failure policy: a single frame failing to be described must NEVER
// fail the whole batch. The pipeline upstream (internal/jobs) is
// already best-effort about the entire visual stage, and we keep the
// same posture here so one transient 5xx doesn't kill a 60-frame run.
package vision

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"scribe-web/internal/config"
	"scribe-web/internal/media"
	"scribe-web/internal/vlm"
)

// Service binds a VLM provider to the vision-analysis flow. Construct
// once at boot and reuse for the process lifetime.
type Service struct {
	prov vlm.Provider
	cfg  *config.VLMConfig
}

// New returns a Service. nil Provider / nil cfg are both allowed —
// Describe will return an error in those cases, mirroring the LLM
// service so callers stay symmetric.
func New(p vlm.Provider, cfg *config.VLMConfig) *Service {
	return &Service{prov: p, cfg: cfg}
}

// Enabled is a sugar wrapper over cfg.Enabled() that also covers the
// "constructed with nil provider" case.
func (s *Service) Enabled() bool {
	return s != nil && s.prov != nil && s.cfg.Enabled()
}

// Describe runs one VLM call per frame and returns the insights in
// timestamp order. onProgress (optional) is invoked once per frame
// from a single dedicated goroutine so callers don't need to lock.
//
// Errors:
//   - returns nil, ErrDisabled if the service isn't usable at all.
//   - returns the populated insights slice + nil if some frames failed
//     (their Caption is filled with a fallback marker and Error has
//     the underlying message).
//   - returns nil, error only for ctx cancellation.
func (s *Service) Describe(ctx context.Context, frames []media.Frame, onProgress func(done, total int)) ([]Insight, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if len(frames) == 0 {
		return nil, nil
	}

	conc := s.cfg.Concurrency
	if conc <= 0 {
		conc = 4
	}
	if conc > len(frames) {
		conc = len(frames)
	}

	insights := make([]Insight, len(frames))
	sem := make(chan struct{}, conc)

	// progressCh sequences progress callbacks through one goroutine so
	// users never see done count regress under racy writes.
	progressCh := make(chan struct{}, len(frames))
	progDone := make(chan struct{})
	go func() {
		defer close(progDone)
		var done int
		total := len(frames)
		for range progressCh {
			done++
			if onProgress != nil {
				onProgress(done, total)
			}
		}
	}()

	var wg sync.WaitGroup
	for i, frame := range frames {
		select {
		case <-ctx.Done():
			// We still wait for in-flight workers to drain (they pick
			// up the same ctx and return promptly).
		case sem <- struct{}{}:
		}
		if ctx.Err() != nil {
			break
		}
		i, frame := i, frame
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			insights[i] = s.describeOne(ctx, frame)
			progressCh <- struct{}{}
		}()
	}
	wg.Wait()
	close(progressCh)
	<-progDone

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Filter out zero-value slots (would only happen if ctx cancelled
	// mid-flight, which we already returned above; defensive).
	out := make([]Insight, 0, len(insights))
	for _, in := range insights {
		if in.ImagePath == "" && in.Caption == "" && in.Error == "" {
			continue
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimestampSec < out[j].TimestampSec })
	for i := range out {
		out[i].Index = i
	}
	return out, nil
}

func (s *Service) describeOne(ctx context.Context, f media.Frame) Insight {
	start := time.Now()
	res := Insight{
		Index:        f.Index,
		TimestampSec: f.TimestampSec,
		ImagePath:    f.ImagePath,
	}
	imgPart, err := vlm.EncodeImage(f.ImagePath)
	if err != nil {
		res.Caption = "(画面描述失败)"
		res.Error = err.Error()
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}
	req := vlm.ChatRequest{
		Messages: []vlm.ChatMessage{
			{Role: "system", Content: []vlm.ContentPart{vlm.TextPart(systemPrompt)}},
			{Role: "user", Content: []vlm.ContentPart{imgPart, vlm.TextPart(userPromptText)}},
		},
	}
	resp, err := s.prov.Chat(ctx, req)
	res.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		res.Caption = "(画面描述失败)"
		res.Error = err.Error()
		return res
	}
	res.TokensUsed = resp.Usage.TotalTokens
	caption, ocr := parseReply(resp.Choices[0].Message.Content)
	res.Caption = caption
	res.OCRText = ocr
	return res
}

// ErrDisabled is returned by Describe when the service has no usable
// provider (nil cfg, missing API key, etc.). Callers check this with
// errors.Is so the HTTP layer can answer 503 cleanly.
var ErrDisabled = errors.New("vision: service is not enabled (configure scribe-vlm.json)")

// FormatTimestamp turns 73.4s into "01:13". Lives here (not in summary)
// so any future caller of the visual data — frontend stub, CLI, …
// — gets the same wall-clock formatting without re-deriving it.
func FormatTimestamp(sec float64) string {
	if sec < 0 || sec != sec { // NaN guard
		sec = 0
	}
	total := int(sec + 0.5)
	m := total / 60
	s := total % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}
