// Package jobs ties the pipeline together: yt-dlp -> ffmpeg -> whisper.
// Concurrent worker queue, fan-out events to anyone subscribed for SSE.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"scribe-web/internal/asr"
	"scribe-web/internal/config"
	"scribe-web/internal/media"
	"scribe-web/internal/store"
	"scribe-web/internal/subtitle"
	"scribe-web/internal/vision"
	"scribe-web/internal/ytdlp"
)

// thumbnailHTTPClient is a process-wide tiny client for fetching
// poster images. We deliberately use a short timeout: the thumbnail
// is best-effort and must never block job completion.
var thumbnailHTTPClient = &http.Client{Timeout: 8 * time.Second}

var (
	downloadPercentRe    = regexp.MustCompile(`(\d{1,3}(?:\.\d+)?)%`)
	transcribeProgressRe = regexp.MustCompile(`至\s*([0-9]+(?:\.[0-9]+)?)s`)
)

// ErrJobInProgress is returned by Delete when the caller tries to remove
// a job that hasn't reached a terminal phase yet. The HTTP layer maps
// this to 409 Conflict so the UI can show "wait until it finishes".
var ErrJobInProgress = errors.New("job is still in progress")

// Manager owns the queue, worker, and event fan-out.
type Manager struct {
	cfg    *config.Config
	store  *store.Store
	vision *vision.Service // optional; nil disables the visual stage

	queue     chan string
	visionSem chan struct{}
	wg        sync.WaitGroup

	mu      sync.Mutex
	subs    map[string]map[chan Event]struct{} // jobID -> subscribers
	allSubs map[chan Event]struct{}            // global subscribers (job list page)
}

// Event is what the API streams via SSE.
type Event struct {
	JobID        string             `json:"job_id"`
	Phase        store.Phase        `json:"phase"`
	Message      string             `json:"message,omitempty"`
	Done         bool               `json:"done,omitempty"`
	Error        string             `json:"error,omitempty"`
	VisionStatus store.VisionStatus `json:"vision_status,omitempty"`
}

// NewManager builds a Manager. visionSvc may be nil — when so (or when
// it reports !Enabled()) the pipeline skips the visual-analysis stage
// entirely and behaves identically to the pre-VLM build.
func NewManager(cfg *config.Config, st *store.Store, visionSvc *vision.Service) *Manager {
	workers := 1
	if cfg != nil && cfg.WorkerConcurrency > 0 {
		workers = cfg.WorkerConcurrency
	}
	queueCap := workers * 32
	if queueCap < 32 {
		queueCap = 32
	}
	return &Manager{
		cfg:       cfg,
		store:     st,
		vision:    visionSvc,
		queue:     make(chan string, queueCap),
		visionSem: make(chan struct{}, 1),
		subs:      map[string]map[chan Event]struct{}{},
		allSubs:   map[chan Event]struct{}{},
	}
}

// Start spins up the worker goroutine. Stop with the returned context's
// cancel — when it fires the worker drains and exits.
func (m *Manager) Start(ctx context.Context) {
	workers := 1
	if m.cfg != nil && m.cfg.WorkerConcurrency > 0 {
		workers = m.cfg.WorkerConcurrency
	}
	m.requeueUnfinished()
	for i := 0; i < workers; i++ {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-m.queue:
					m.run(ctx, id)
				}
			}
		}()
	}
}

// Wait blocks until the worker exits. Useful for graceful shutdown.
func (m *Manager) Wait() { m.wg.Wait() }

// Submit creates a new job record + enqueues it.
func (m *Manager) Submit(url, model, language string, enableVision bool) (*store.Job, error) {
	if url == "" {
		return nil, fmt.Errorf("url required")
	}
	if model == "" {
		model = "base"
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	job := &store.Job{
		ID:           id,
		URL:          url,
		Model:        model,
		Language:     language,
		EnableVision: enableVision,
		Phase:        store.PhaseQueued,
		CreatedAt:    time.Now().UTC(),
		Progress:     map[store.Phase]int{},
	}
	if m.jobVisionEnabled(job) {
		job.VisionStatus = store.VisionPending
		job.VisionMessage = "等待转写完成后进行画面理解"
	} else {
		job.VisionStatus = store.VisionDisabled
	}
	if err := m.store.Create(job); err != nil {
		return nil, err
	}
	select {
	case m.queue <- id:
	default:
		_, _ = m.store.Update(id, func(j *store.Job) {
			j.Phase = store.PhaseFailed
			j.Error = "queue is full"
		})
		return nil, fmt.Errorf("queue full")
	}
	return job, nil
}

func (m *Manager) run(parent context.Context, id string) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	now := time.Now().UTC()
	_, err := m.store.Update(id, func(j *store.Job) {
		j.Phase = store.PhaseDownloading
		j.StartedAt = &now
		j.Message = "starting"
		if j.Progress == nil {
			j.Progress = map[store.Phase]int{}
		}
		j.Progress[store.PhaseDownloading] = 1
	})
	if err != nil {
		log.Printf("jobs: update failed: %v", err)
		return
	}
	m.emit(id, Event{JobID: id, Phase: store.PhaseDownloading, Message: "starting"})

	job, ok := m.store.Get(id)
	if !ok {
		return
	}

	// 1. Download.
	//
	// Pick the format up-front based on whether the visual stage will
	// run for this job: audio-only keeps downloads tiny when VLM is off
	// or unchecked, but the keyframe extractor needs a real video stream.
	jobVisionEnabled := m.jobVisionEnabled(job)
	format := ytdlp.FormatAudioOnly
	if jobVisionEnabled {
		format = ytdlp.FormatVideoPlusAudio
	}
	dlRes, err := retryWithBackoff(ctx, m.retryCount(), m.retryBackoff(), func(attempt int, e error) {
		msg := fmt.Sprintf("下载失败，准备重试 (%d/%d): %v", attempt, m.retryCount(), e)
		m.appendLog(id, store.PhaseDownloading, msg)
		m.emit(id, Event{JobID: id, Phase: store.PhaseDownloading, Message: msg})
	}, func() (*ytdlp.Result, error) {
		return ytdlp.Download(ctx, m.cfg, job.URL, m.cfg.DownloadsDir, format, func(line string) {
			m.appendLog(id, store.PhaseDownloading, line)
			m.emit(id, Event{JobID: id, Phase: store.PhaseDownloading, Message: line})
			if p, ok := parseDownloadPercent(line); ok {
				_ = m.setProgress(id, store.PhaseDownloading, p)
			}
		})
	})
	if err != nil {
		m.fail(id, fmt.Errorf("download: %w", err))
		return
	}
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.Source = &dlRes.Info
		j.MediaPath = dlRes.FilePath
		if j.Progress == nil {
			j.Progress = map[store.Phase]int{}
		}
		j.Progress[store.PhaseDownloading] = 100
		j.Progress[store.PhaseExtracting] = 1
	})

	// 1b. Best-effort poster fetch. Bilibili thumbnails are
	// referer-protected, but yt-dlp's URL works from us too because
	// we don't send Referer. Failures here are non-fatal.
	if thumbPath, err := m.downloadThumbnail(ctx, &dlRes.Info); err != nil {
		log.Printf("jobs: thumbnail fetch for %s skipped: %v", id, err)
	} else if thumbPath != "" {
		_, _ = m.store.Update(id, func(j *store.Job) {
			j.ThumbnailPath = thumbPath
		})
	}

	// 2. Prefer platform-provided subtitles. If none are available, fall
	// back to the historical ffmpeg + Whisper ASR path.
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.Phase = store.PhaseTranscribing
		j.Message = "checking platform subtitles"
		if j.Progress == nil {
			j.Progress = map[store.Phase]int{}
		}
		j.Progress[store.PhaseTranscribing] = 10
	})
	m.emit(id, Event{JobID: id, Phase: store.PhaseTranscribing, Message: "checking platform subtitles"})
	res, transcriptSource, subErr := m.tryPlatformSubtitle(ctx, id, job, dlRes.Info.ID)
	if subErr != nil {
		m.appendLog(id, store.PhaseTranscribing, "未找到可用平台字幕，回退 Whisper ASR："+subErr.Error())

		// 2b. Extract audio.
		_, _ = m.store.Update(id, func(j *store.Job) {
			j.Phase = store.PhaseExtracting
			j.Message = "extracting audio"
			if j.Progress == nil {
				j.Progress = map[store.Phase]int{}
			}
			j.Progress[store.PhaseExtracting] = 20
		})
		m.emit(id, Event{JobID: id, Phase: store.PhaseExtracting, Message: "extracting audio"})
		wav, err := retryWithBackoff(ctx, m.retryCount(), m.retryBackoff(), func(attempt int, e error) {
			msg := fmt.Sprintf("抽取音频失败，准备重试 (%d/%d): %v", attempt, m.retryCount(), e)
			m.appendLog(id, store.PhaseExtracting, msg)
			m.emit(id, Event{JobID: id, Phase: store.PhaseExtracting, Message: msg})
		}, func() (string, error) {
			return media.ExtractAudio(ctx, m.cfg, dlRes.FilePath, m.cfg.WorkDir)
		})
		if err != nil {
			m.fail(id, fmt.Errorf("extract: %w", err))
			return
		}
		_ = m.setProgress(id, store.PhaseExtracting, 100)

		// 3. Transcribe with Whisper.
		_, _ = m.store.Update(id, func(j *store.Job) {
			j.Phase = store.PhaseTranscribing
			j.Message = "transcribing"
			if j.Progress == nil {
				j.Progress = map[store.Phase]int{}
			}
			if j.Progress[store.PhaseTranscribing] < 30 {
				j.Progress[store.PhaseTranscribing] = 30
			}
		})
		m.emit(id, Event{JobID: id, Phase: store.PhaseTranscribing, Message: "transcribing"})
		res, err = retryWithBackoff(ctx, m.retryCount(), m.retryBackoff(), func(attempt int, e error) {
			msg := fmt.Sprintf("转写失败，准备重试 (%d/%d): %v", attempt, m.retryCount(), e)
			m.appendLog(id, store.PhaseTranscribing, msg)
			m.emit(id, Event{JobID: id, Phase: store.PhaseTranscribing, Message: msg})
		}, func() (*asr.Result, error) {
			return asr.Transcribe(ctx, m.cfg, asr.Request{
				AudioPath: wav,
				Model:     job.Model,
				Language:  job.Language,
				OnProgress: func(msg string) {
					m.appendLog(id, store.PhaseTranscribing, msg)
					m.emit(id, Event{JobID: id, Phase: store.PhaseTranscribing, Message: msg})
					if p, ok := parseTranscribePercent(msg, job.Source); ok {
						_ = m.setProgress(id, store.PhaseTranscribing, p)
					}
				},
			})
		})
		if err != nil {
			m.fail(id, fmt.Errorf("transcribe: %w", err))
			return
		}
		transcriptSource = "whisper_asr"
	}

	finished := time.Now().UTC()
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.Transcript = res
		j.TranscriptSource = transcriptSource
		j.Phase = store.PhaseDone
		j.FinishedAt = &finished
		j.Message = "done"
		if j.Progress == nil {
			j.Progress = map[store.Phase]int{}
		}
		j.Progress[j.Phase] = 100
		j.Progress[store.PhaseTranscribing] = 100
		if jobVisionEnabled {
			j.VisionStatus = store.VisionPending
			j.VisionMessage = "画面理解将在后台继续进行"
		}
	})
	m.emit(id, Event{JobID: id, Phase: store.PhaseDone, Message: "done", Done: true})

	// 4. Visual analysis — detached best-effort enhancer. It deliberately
	// uses a background context so the main worker can return immediately
	// and pick up the next queued transcription job.
	if jobVisionEnabled {
		go m.runVision(context.Background(), id, dlRes.FilePath)
	}
}

func (m *Manager) tryPlatformSubtitle(ctx context.Context, id string, job *store.Job, sourceID string) (*asr.Result, string, error) {
	subDir := filepath.Join(m.cfg.WorkDir, id+"-subtitles")
	sub, err := ytdlp.DownloadSubtitle(ctx, m.cfg, job.URL, subDir, sourceID, job.Language, func(line string) {
		m.appendLog(id, store.PhaseTranscribing, line)
	})
	if err != nil {
		return nil, "", err
	}
	res, err := subtitle.ParseFile(sub.Path, sub.Language)
	if err != nil {
		return nil, "", err
	}
	msg := fmt.Sprintf("使用平台字幕（%s，%s）", sub.Language, sub.Kind)
	m.appendLog(id, store.PhaseTranscribing, msg)
	m.emit(id, Event{JobID: id, Phase: store.PhaseTranscribing, Message: msg})
	_ = m.setProgress(id, store.PhaseTranscribing, 100)
	return res, "platform_subtitle", nil
}

// runVision extracts keyframes and asks the VLM provider to caption each
// one. It never mutates the main job Phase: visual analysis is a
// background enhancer and failures are recorded only in vision fields.
func (m *Manager) runVision(ctx context.Context, id, videoPath string) {
	if !m.visionEnabled() {
		return
	}
	m.visionSem <- struct{}{}
	defer func() { <-m.visionSem }()

	started := time.Now().UTC()
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.VisionStatus = store.VisionRunning
		j.VisionMessage = "抽取关键帧"
		j.VisionStartedAt = &started
		j.VisionFinishedAt = nil
		j.VisionError = ""
	})
	m.appendLog(id, store.PhaseAnalyzing, "抽取关键帧")
	m.emit(id, Event{JobID: id, Phase: store.PhaseAnalyzing, Message: "抽取关键帧", VisionStatus: store.VisionRunning})

	framesDir := filepath.Join(m.cfg.FramesDir, id)
	frames, err := media.ExtractKeyframes(ctx, m.cfg, videoPath, framesDir, media.FrameOptions{
		SceneThreshold: m.cfg.VLM.SceneThreshold,
		IntervalSec:    m.cfg.VLM.FrameIntervalSeconds,
		MaxFrames:      m.cfg.VLM.MaxFrames,
	})
	if err != nil {
		log.Printf("jobs %s: keyframes failed (non-fatal): %v", id, err)
		m.appendLog(id, store.PhaseAnalyzing, "抽帧失败（不影响整体）："+err.Error())
		m.failVision(id, err)
		return
	}
	if len(frames) == 0 {
		msg := "未能从视频抽取关键帧（不影响整体）"
		m.appendLog(id, store.PhaseAnalyzing, msg)
		m.finishVision(id, framesDir, nil, msg)
		return
	}
	msg := fmt.Sprintf("抽到 %d 帧，开始画面理解", len(frames))
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.VisionMessage = msg
	})
	m.emit(id, Event{
		JobID:        id,
		Phase:        store.PhaseAnalyzing,
		Message:      msg,
		VisionStatus: store.VisionRunning,
	})

	insights, vErr := m.vision.Describe(ctx, frames, func(done, total int) {
		msg := fmt.Sprintf("画面理解 %d/%d", done, total)
		_, _ = m.store.Update(id, func(j *store.Job) {
			j.VisionMessage = msg
		})
		m.emit(id, Event{
			JobID:        id,
			Phase:        store.PhaseAnalyzing,
			Message:      msg,
			VisionStatus: store.VisionRunning,
		})
	})
	if vErr != nil {
		log.Printf("jobs %s: vision failed (non-fatal): %v", id, vErr)
		m.appendLog(id, store.PhaseAnalyzing, "画面理解失败（不影响整体）："+vErr.Error())
		m.failVision(id, vErr)
		return
	}
	m.finishVision(id, framesDir, insights, fmt.Sprintf("画面理解完成：%d 帧", len(insights)))
}

func (m *Manager) visionEnabled() bool {
	return m.vision != nil && m.vision.Enabled()
}

func (m *Manager) jobVisionEnabled(job *store.Job) bool {
	return job != nil && job.EnableVision && m.visionEnabled()
}

func (m *Manager) finishVision(id, framesDir string, insights []vision.Insight, msg string) {
	finished := time.Now().UTC()
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.VisionStatus = store.VisionDone
		j.VisionMessage = msg
		j.VisionFinishedAt = &finished
		j.VisionError = ""
		j.FramesDir = framesDir
		j.Frames = insights
	})
	m.appendLog(id, store.PhaseAnalyzing, msg)
	m.emit(id, Event{JobID: id, Phase: store.PhaseDone, Message: msg, VisionStatus: store.VisionDone})
}

func (m *Manager) failVision(id string, err error) {
	finished := time.Now().UTC()
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.VisionStatus = store.VisionFailed
		j.VisionMessage = "画面理解失败（不影响转写）"
		j.VisionError = err.Error()
		j.VisionFinishedAt = &finished
	})
	m.emit(id, Event{
		JobID:        id,
		Phase:        store.PhaseDone,
		Message:      "画面理解失败（不影响转写）",
		Error:        err.Error(),
		VisionStatus: store.VisionFailed,
	})
}

// Delete removes a finished/failed job along with any media files it
// owns. Refuses jobs that are still being processed (returns
// ErrJobInProgress) so a user can't pull the rug out from the worker.
//
// Media cleanup scope (anything else is OFF LIMITS):
//   - <downloads>/<Source.ID>.* (e.g. .m4a, .webm, .info.json)
//   - <MediaPath> fallback if Source is nil but a partial download
//     left a file behind
//
// We never touch <models> or other jobs' assets.
func (m *Manager) Delete(id string) error {
	job, ok := m.store.Get(id)
	if !ok {
		return fmt.Errorf("job %s not found", id)
	}
	if job.Phase != store.PhaseDone && job.Phase != store.PhaseFailed {
		return ErrJobInProgress
	}

	deleted, err := m.store.Delete(id)
	if err != nil {
		return err
	}

	m.cleanupMedia(deleted)
	m.dropSubscribers(id)
	return nil
}

func (m *Manager) cleanupMedia(j *store.Job) {
	if j == nil {
		return
	}
	// Thumbnail first — independent of the media file, scoped to
	// ThumbnailsDir so even an attacker-controlled ThumbnailPath
	// can't escape.
	if j.ThumbnailPath != "" && m.cfg.ThumbnailsDir != "" {
		abs, err := filepath.Abs(j.ThumbnailPath)
		thDir, dirErr := filepath.Abs(m.cfg.ThumbnailsDir)
		if err == nil && dirErr == nil && filepath.Dir(abs) == thDir {
			if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("jobs: remove thumbnail %s: %v", abs, err)
			}
		}
	}
	// Frames directory — scoped to FramesDir, same defence-in-depth
	// as the thumbnail check. We RemoveAll the per-job subdir so the
	// JPGs + frames.txt sidecar disappear together.
	if j.FramesDir != "" && m.cfg.FramesDir != "" {
		abs, err := filepath.Abs(j.FramesDir)
		frDir, dirErr := filepath.Abs(m.cfg.FramesDir)
		if err == nil && dirErr == nil && filepath.Dir(abs) == frDir {
			if err := os.RemoveAll(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("jobs: remove frames %s: %v", abs, err)
			}
		}
	}
	dl := m.cfg.DownloadsDir
	if j.Source != nil && j.Source.ID != "" {
		// Glob downloads/<sourceID>.* — covers .m4a / .webm / .wav /
		// .info.json / .part etc. Restrict to this dir; never recurse.
		pattern := filepath.Join(dl, j.Source.ID+".*")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			log.Printf("jobs: glob %s: %v", pattern, err)
		}
		for _, p := range matches {
			// Defence in depth: confirm the file really sits in
			// DownloadsDir before unlinking. Glob can't escape, but
			// a future refactor might pass a tainted pattern.
			abs, err := filepath.Abs(p)
			if err != nil {
				continue
			}
			absDir, err := filepath.Abs(dl)
			if err != nil || filepath.Dir(abs) != absDir {
				log.Printf("jobs: skip %s outside %s", abs, absDir)
				continue
			}
			if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("jobs: remove %s: %v", abs, err)
			}
		}
		return
	}
	// Fallback: no Source (e.g. failed before yt-dlp returned info),
	// but we may still have an explicit MediaPath in the record.
	if j.MediaPath != "" {
		abs, err := filepath.Abs(j.MediaPath)
		if err != nil {
			return
		}
		absDir, err := filepath.Abs(dl)
		if err != nil || filepath.Dir(abs) != absDir {
			return
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("jobs: remove %s: %v", abs, err)
		}
	}
}

// downloadThumbnail fetches info.Thumbnail to
// <ThumbnailsDir>/<sourceID>.<ext>. The extension is inferred from the
// URL path (jpg / png / webp); we default to .jpg when ambiguous.
// Returns the absolute file path on success.
func (m *Manager) downloadThumbnail(ctx context.Context, info *ytdlp.Info) (string, error) {
	if info == nil || info.Thumbnail == "" || info.ID == "" {
		return "", nil
	}
	if m.cfg.ThumbnailsDir == "" {
		return "", nil
	}
	if err := os.MkdirAll(m.cfg.ThumbnailsDir, 0o755); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.Thumbnail, nil)
	if err != nil {
		return "", err
	}
	// Looks like a normal browser request — some CDNs 403 on empty UA.
	req.Header.Set("User-Agent", "Mozilla/5.0 ScribeWeb/1.0")
	resp, err := thumbnailHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("thumbnail http %d", resp.StatusCode)
	}
	ext := thumbnailExt(info.Thumbnail, resp.Header.Get("Content-Type"))
	dst := filepath.Join(m.cfg.ThumbnailsDir, info.ID+ext)
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	const max = 4 << 20 // 4 MiB ceiling, plenty for a poster
	if _, err := io.Copy(f, io.LimitReader(resp.Body, max)); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

// thumbnailExt picks a sensible extension. Prefers the URL path; falls
// back to MIME hints; lastly defaults to .jpg (good enough for the UI
// because <img> sniffs the bytes anyway).
func thumbnailExt(rawURL, mime string) string {
	lower := strings.ToLower(rawURL)
	// Trim query string before sniffing extension.
	if i := strings.IndexAny(lower, "?#"); i >= 0 {
		lower = lower[:i]
	}
	switch {
	case strings.HasSuffix(lower, ".webp"):
		return ".webp"
	case strings.HasSuffix(lower, ".png"):
		return ".png"
	case strings.HasSuffix(lower, ".jpeg"):
		return ".jpeg"
	case strings.HasSuffix(lower, ".jpg"):
		return ".jpg"
	}
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "image/webp":
		return ".webp"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	}
	return ".jpg"
}

// dropSubscribers boots any SSE clients currently watching this job by
// sending a terminal Event and closing their channels. Without this the
// client would hang on a dead job until its 15s keepalive timed out.
func (m *Manager) dropSubscribers(id string) {
	m.mu.Lock()
	set := m.subs[id]
	delete(m.subs, id)
	m.mu.Unlock()
	for ch := range set {
		// Best-effort terminal event so the client can react. The HTTP
		// handler closes the channel itself via its unsub closure, so
		// we don't close here to avoid double-close panics.
		select {
		case ch <- Event{JobID: id, Phase: store.PhaseFailed, Message: "deleted", Done: true}:
		default:
		}
	}
}

func (m *Manager) fail(id string, err error) {
	finished := time.Now().UTC()
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.Phase = store.PhaseFailed
		j.Error = err.Error()
		j.Message = err.Error()
		j.FinishedAt = &finished
	})
	m.emit(id, Event{JobID: id, Phase: store.PhaseFailed, Message: err.Error(), Done: true, Error: err.Error()})
}

func (m *Manager) appendLog(id string, phase store.Phase, msg string) {
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.Logs = append(j.Logs, store.LogLine{At: time.Now().UTC(), Phase: phase, Message: msg})
		// Cap log lines so a chatty job doesn't bloat the JSON file
		// indefinitely (SSE still sees every line live).
		const max = 500
		if len(j.Logs) > max {
			j.Logs = j.Logs[len(j.Logs)-max:]
		}
	})
}

// Subscribe returns an event channel for one job. Unsubscribe when the
// HTTP handler exits.
func (m *Manager) Subscribe(id string) (chan Event, func()) {
	ch := make(chan Event, 32)
	m.mu.Lock()
	if _, ok := m.subs[id]; !ok {
		m.subs[id] = map[chan Event]struct{}{}
	}
	m.subs[id][ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		if set, ok := m.subs[id]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(m.subs, id)
			}
		}
		m.mu.Unlock()
		close(ch)
	}
}

// SubscribeAll feeds events for every job — handy for a "jobs list"
// page that wants live phase changes.
func (m *Manager) SubscribeAll() (chan Event, func()) {
	ch := make(chan Event, 64)
	m.mu.Lock()
	m.allSubs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		delete(m.allSubs, ch)
		m.mu.Unlock()
		close(ch)
	}
}

func (m *Manager) emit(id string, ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if set, ok := m.subs[id]; ok {
		for ch := range set {
			select {
			case ch <- ev:
			default:
			}
		}
	}
	for ch := range m.allSubs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (m *Manager) requeueUnfinished() {
	jobs := m.store.List()
	if len(jobs) == 0 {
		return
	}
	for _, j := range jobs {
		if j == nil || j.ID == "" {
			continue
		}
		if j.Phase != store.PhaseQueued {
			continue
		}
		select {
		case m.queue <- j.ID:
			m.emit(j.ID, Event{JobID: j.ID, Phase: store.PhaseQueued, Message: "服务重启后恢复排队"})
		default:
			log.Printf("jobs: resume queue full, skip %s", j.ID)
		}
	}
}

func (m *Manager) retryCount() int {
	if m.cfg == nil || m.cfg.JobRetryCount < 0 {
		return 0
	}
	return m.cfg.JobRetryCount
}

func (m *Manager) retryBackoff() time.Duration {
	if m.cfg == nil || m.cfg.RetryBackoff <= 0 {
		return 1200 * time.Millisecond
	}
	return m.cfg.RetryBackoff
}

func (m *Manager) setProgress(id string, phase store.Phase, value int) error {
	v := int(math.Max(0, math.Min(100, float64(value))))
	_, err := m.store.Update(id, func(j *store.Job) {
		if j.Progress == nil {
			j.Progress = map[store.Phase]int{}
		}
		if prev := j.Progress[phase]; v > prev {
			j.Progress[phase] = v
		}
	})
	return err
}

func parseDownloadPercent(line string) (int, bool) {
	m := downloadPercentRe.FindStringSubmatch(line)
	if len(m) < 2 {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return int(math.Round(f)), true
}

func parseTranscribePercent(msg string, source *ytdlp.Info) (int, bool) {
	m := transcribeProgressRe.FindStringSubmatch(msg)
	if len(m) < 2 || source == nil || source.Duration <= 0 {
		return 0, false
	}
	done, err := strconv.ParseFloat(m[1], 64)
	if err != nil || done <= 0 {
		return 0, false
	}
	p := int(math.Round(done / source.Duration * 100))
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p, true
}

func retryWithBackoff[T any](
	ctx context.Context,
	maxRetries int,
	baseBackoff time.Duration,
	onRetry func(attempt int, err error),
	fn func() (T, error),
) (T, error) {
	var zero T
	if maxRetries < 0 {
		maxRetries = 0
	}
	if baseBackoff <= 0 {
		baseBackoff = time.Second
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		lastErr = err
		if attempt == maxRetries {
			break
		}
		if onRetry != nil {
			onRetry(attempt+1, err)
		}
		wait := baseBackoff * time.Duration(1<<attempt)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	return zero, lastErr
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102-150405-") + hex.EncodeToString(b), nil
}
