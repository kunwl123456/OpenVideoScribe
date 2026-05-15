// Package jobs ties the pipeline together: yt-dlp -> ffmpeg -> whisper.
// Single-worker queue, fan-out events to anyone subscribed for SSE.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"scribe-web/internal/asr"
	"scribe-web/internal/config"
	"scribe-web/internal/media"
	"scribe-web/internal/store"
	"scribe-web/internal/ytdlp"
)

// thumbnailHTTPClient is a process-wide tiny client for fetching
// poster images. We deliberately use a short timeout: the thumbnail
// is best-effort and must never block job completion.
var thumbnailHTTPClient = &http.Client{Timeout: 8 * time.Second}

// ErrJobInProgress is returned by Delete when the caller tries to remove
// a job that hasn't reached a terminal phase yet. The HTTP layer maps
// this to 409 Conflict so the UI can show "wait until it finishes".
var ErrJobInProgress = errors.New("job is still in progress")

// Manager owns the queue, worker, and event fan-out.
type Manager struct {
	cfg   *config.Config
	store *store.Store

	queue chan string
	wg    sync.WaitGroup

	mu       sync.Mutex
	subs     map[string]map[chan Event]struct{} // jobID -> subscribers
	allSubs  map[chan Event]struct{}            // global subscribers (job list page)
}

// Event is what the API streams via SSE.
type Event struct {
	JobID   string      `json:"job_id"`
	Phase   store.Phase `json:"phase"`
	Message string      `json:"message,omitempty"`
	Done    bool        `json:"done,omitempty"`
	Error   string      `json:"error,omitempty"`
}

func NewManager(cfg *config.Config, st *store.Store) *Manager {
	return &Manager{
		cfg:     cfg,
		store:   st,
		queue:   make(chan string, 32),
		subs:    map[string]map[chan Event]struct{}{},
		allSubs: map[chan Event]struct{}{},
	}
}

// Start spins up the worker goroutine. Stop with the returned context's
// cancel — when it fires the worker drains and exits.
func (m *Manager) Start(ctx context.Context) {
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

// Wait blocks until the worker exits. Useful for graceful shutdown.
func (m *Manager) Wait() { m.wg.Wait() }

// Submit creates a new job record + enqueues it.
func (m *Manager) Submit(url, model, language string) (*store.Job, error) {
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
		ID:        id,
		URL:       url,
		Model:     model,
		Language:  language,
		Phase:     store.PhaseQueued,
		CreatedAt: time.Now().UTC(),
		Progress:  map[store.Phase]int{},
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
	dlRes, err := ytdlp.Download(ctx, m.cfg, job.URL, m.cfg.DownloadsDir, func(line string) {
		m.appendLog(id, store.PhaseDownloading, line)
		m.emit(id, Event{JobID: id, Phase: store.PhaseDownloading, Message: line})
	})
	if err != nil {
		m.fail(id, fmt.Errorf("download: %w", err))
		return
	}
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.Source = &dlRes.Info
		j.MediaPath = dlRes.FilePath
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

	// 2. Extract audio.
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.Phase = store.PhaseExtracting
		j.Message = "extracting audio"
	})
	m.emit(id, Event{JobID: id, Phase: store.PhaseExtracting, Message: "extracting audio"})
	wav, err := media.ExtractAudio(ctx, m.cfg, dlRes.FilePath, m.cfg.WorkDir)
	if err != nil {
		m.fail(id, fmt.Errorf("extract: %w", err))
		return
	}

	// 3. Transcribe.
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.Phase = store.PhaseTranscribing
		j.Message = "transcribing"
	})
	m.emit(id, Event{JobID: id, Phase: store.PhaseTranscribing, Message: "transcribing"})
	res, err := asr.Transcribe(ctx, m.cfg, asr.Request{
		AudioPath: wav,
		Model:     job.Model,
		Language:  job.Language,
		OnProgress: func(msg string) {
			m.appendLog(id, store.PhaseTranscribing, msg)
			m.emit(id, Event{JobID: id, Phase: store.PhaseTranscribing, Message: msg})
		},
	})
	if err != nil {
		m.fail(id, fmt.Errorf("transcribe: %w", err))
		return
	}

	finished := time.Now().UTC()
	_, _ = m.store.Update(id, func(j *store.Job) {
		j.Phase = store.PhaseDone
		j.Transcript = res
		j.FinishedAt = &finished
		j.Message = "done"
	})
	m.emit(id, Event{JobID: id, Phase: store.PhaseDone, Message: "done", Done: true})
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

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102-150405-") + hex.EncodeToString(b), nil
}
