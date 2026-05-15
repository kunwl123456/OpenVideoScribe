// Package jobs ties the pipeline together: yt-dlp -> ffmpeg -> whisper.
// Single-worker queue, fan-out events to anyone subscribed for SSE.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"scribe-web/internal/asr"
	"scribe-web/internal/config"
	"scribe-web/internal/media"
	"scribe-web/internal/store"
	"scribe-web/internal/ytdlp"
)

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
