// Package store persists job state and transcripts as JSON files. No
// database for the MVP — one file per job under <data>/jobs/<id>.json
// keeps recovery and inspection trivial.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"scribe-web/internal/asr"
	"scribe-web/internal/vision"
	"scribe-web/internal/ytdlp"
)

// Phase describes which step of the pipeline a job is currently in.
type Phase string

const (
	PhaseQueued       Phase = "queued"
	PhaseDownloading  Phase = "downloading"
	PhaseExtracting   Phase = "extracting"
	PhaseTranscribing Phase = "transcribing"
	// PhaseAnalyzing is kept for live VLM log/events and legacy records.
	// New visual analysis runs in the background and reports lifecycle
	// through Job.VisionStatus so PhaseDone remains stable.
	PhaseAnalyzing Phase = "analyzing"
	PhaseDone      Phase = "done"
	PhaseFailed    Phase = "failed"
)

// VisionStatus tracks the optional visual-analysis enhancer separately
// from the main transcription job. A job can be PhaseDone while vision
// is still running in the background.
type VisionStatus string

const (
	VisionDisabled VisionStatus = "disabled"
	VisionPending  VisionStatus = "pending"
	VisionRunning  VisionStatus = "running"
	VisionDone     VisionStatus = "done"
	VisionFailed   VisionStatus = "failed"
)

// Job is the canonical record persisted on disk.
type Job struct {
	ID               string       `json:"id"`
	URL              string       `json:"url"`
	Model            string       `json:"model"`
	Language         string       `json:"language"`
	EnableVision     bool         `json:"enable_vision,omitempty"`
	Phase            Phase        `json:"phase"`
	Message          string       `json:"message,omitempty"`
	Error            string       `json:"error,omitempty"`
	CreatedAt        time.Time    `json:"created_at"`
	StartedAt        *time.Time   `json:"started_at,omitempty"`
	FinishedAt       *time.Time   `json:"finished_at,omitempty"`
	Source           *ytdlp.Info  `json:"source,omitempty"`
	Transcript       *asr.Result  `json:"transcript,omitempty"`
	TranscriptSource string       `json:"transcript_source,omitempty"`
	MediaPath        string       `json:"media_path,omitempty"`
	ThumbnailPath    string       `json:"thumbnail_path,omitempty"`
	VisionStatus     VisionStatus `json:"vision_status,omitempty"`
	VisionMessage    string       `json:"vision_message,omitempty"`
	VisionError      string       `json:"vision_error,omitempty"`
	VisionStartedAt  *time.Time   `json:"vision_started_at,omitempty"`
	VisionFinishedAt *time.Time   `json:"vision_finished_at,omitempty"`
	// FramesDir is the per-job directory holding extracted keyframe
	// JPGs + ffmpeg's metadata sidecar. Empty when the visual stage
	// hasn't run for this job (vision disabled or skipped).
	FramesDir string `json:"frames_dir,omitempty"`
	// Frames are the per-timestamp visual insights produced by the
	// VLM stage. Sorted by TimestampSec ascending; empty when visual
	// stage didn't run or yielded nothing.
	Frames              []vision.Insight         `json:"frames,omitempty"`
	Chapters            []Chapter                `json:"chapters,omitempty"`
	ChaptersModel       string                   `json:"chapters_model,omitempty"`
	ChaptersGeneratedAt *time.Time               `json:"chapters_generated_at,omitempty"`
	QASessions          []QASession              `json:"qa_sessions,omitempty"`
	Logs                []LogLine                `json:"logs,omitempty"`
	Progress            map[Phase]int            `json:"progress,omitempty"` // 0..100 per phase
	SimplifiedAt        *time.Time               `json:"simplified_at,omitempty"`
	Summaries           map[string]*SummaryEntry `json:"summaries,omitempty"` // key is summary.Kind string
}

// Chapter is one chapterized section generated from transcript segments.
// StartSec/EndSec are seconds from video start; Bullets keeps 3-5 key points.
type Chapter struct {
	Title     string         `json:"title"`
	StartSec  float64        `json:"start_sec"`
	EndSec    float64        `json:"end_sec"`
	Bullets   []string       `json:"bullets"`
	KeyQuotes []ChapterQuote `json:"key_quotes,omitempty"`
}

// ChapterQuote is one evidence sentence tied to a chapter time range.
type ChapterQuote struct {
	Text     string  `json:"text"`
	StartSec float64 `json:"start_sec"`
	EndSec   float64 `json:"end_sec"`
}

// QASession stores a multi-turn QA conversation within one job.
type QASession struct {
	ID        string      `json:"id"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
	Messages  []QAMessage `json:"messages"`
}

// QAMessage is one turn in a QA session.
type QAMessage struct {
	Role      string       `json:"role"` // user | assistant
	Content   string       `json:"content"`
	At        time.Time    `json:"at"`
	Citations []QACitation `json:"citations,omitempty"`
}

// QACitation links an answer sentence to transcript evidence.
type QACitation struct {
	JobID    string  `json:"job_id,omitempty"`
	JobTitle string  `json:"job_title,omitempty"`
	Text     string  `json:"text"`
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	Score    float64 `json:"score,omitempty"`
}

// SummaryStatus tracks the lifecycle of one summary artefact. Persisted
// so a page refresh / route change can recover the in-flight UI without
// having to keep ephemeral React state.
//
//	pending — POST /summarize accepted, goroutine running
//	done    — markdown + token counts populated
//	failed  — Error explains why; UI shows "重新生成"
type SummaryStatus string

const (
	SummaryPending SummaryStatus = "pending"
	SummaryDone    SummaryStatus = "done"
	SummaryFailed  SummaryStatus = "failed"
)

// SummaryEntry is one LLM-generated artefact attached to a job. Lives
// in the store package (not in summary/) to keep the persistence type
// dependency-free; the summary package converts to/from this shape.
//
// Markdown / TokensUsed / DurationMs etc are only meaningful when
// Status == SummaryDone. For Pending the only fields filled are Kind,
// Status, GeneratedAt (= start time). For Failed: Error.
type SummaryEntry struct {
	Kind              string        `json:"kind"`
	Status            SummaryStatus `json:"status"`
	Markdown          string        `json:"markdown,omitempty"`
	Model             string        `json:"model,omitempty"`
	TokensUsed        int           `json:"tokens_used,omitempty"`
	PromptTokens      int           `json:"prompt_tokens,omitempty"`
	CompletionTokens  int           `json:"completion_tokens,omitempty"`
	EstimatedCost     float64       `json:"estimated_cost,omitempty"`
	EstimatedCostText string        `json:"estimated_cost_text,omitempty"`
	DurationMs        int64         `json:"duration_ms,omitempty"`
	Error             string        `json:"error,omitempty"`
	GeneratedAt       time.Time     `json:"generated_at"`
}

// LogLine is a timestamped UI log entry. Kept short on purpose; the
// detail page renders these as a vertical timeline.
type LogLine struct {
	At      time.Time `json:"at"`
	Phase   Phase     `json:"phase"`
	Message string    `json:"message"`
}

// Store owns the on-disk job records + an in-memory cache.
type Store struct {
	dir string

	mu   sync.Mutex
	jobs map[string]*Job
}

func New(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, jobs: map[string]*Job{}}
	if err := s.loadAll(); err != nil {
		return nil, err
	}
	if n := s.migrateSimplifiedLocked(); n > 0 {
		log.Printf("store: migrated %d transcript(s) to simplified Chinese", n)
	}
	if n := s.recoverStaleJobsLocked(); n > 0 {
		log.Printf("store: job recovery — requeued %d interrupted job(s) after restart", n)
	}
	if n := s.recoverStaleVisionLocked(); n > 0 {
		log.Printf("store: vision recovery — marked %d interrupted visual job(s) as failed", n)
	}
	if legacy, stale := s.recoverStaleSummariesLocked(); legacy+stale > 0 {
		log.Printf("store: summary recovery — promoted %d legacy entr(ies) to done, marked %d stale pending as failed", legacy, stale)
	}
	return s, nil
}

// recoverStaleJobsLocked moves in-flight jobs back to queued after a
// restart. The previous worker goroutine is gone, so we re-enter the
// pipeline from the top rather than leaving stale "running" states.
//
// Mirrors recoverStaleSummariesLocked: every transition is persisted
// to disk so memory and storage stay consistent across restarts, and
// the UI gets a real FinishedAt + a final log line explaining what
// happened.
//
// Called from New() before the store is exposed, so the public mutex
// is not held.
func (s *Store) recoverStaleJobsLocked() int {
	now := time.Now().UTC()
	var n int
	for _, j := range s.jobs {
		if j.Phase == PhaseDone || j.Phase == PhaseFailed || j.Phase == PhaseQueued {
			continue
		}
		j.Phase = PhaseQueued
		j.Message = "服务重启后重新排队"
		j.Error = ""
		j.FinishedAt = nil
		j.Logs = append(j.Logs, LogLine{
			At:      now,
			Phase:   PhaseQueued,
			Message: "服务重启，任务已重新排队",
		})
		if err := s.persistLocked(j); err != nil {
			log.Printf("store: persist stale job recovery for %s: %v", j.ID, err)
			continue
		}
		n++
	}
	return n
}

// recoverStaleVisionLocked finalises visual-analysis goroutines that
// were pending/running when the server stopped. The main job may already
// be done; only the independent vision fields are touched.
func (s *Store) recoverStaleVisionLocked() int {
	now := time.Now().UTC()
	var n int
	for _, j := range s.jobs {
		if j.VisionStatus != VisionPending && j.VisionStatus != VisionRunning {
			continue
		}
		prev := j.VisionStatus
		j.VisionStatus = VisionFailed
		if j.VisionError == "" {
			j.VisionError = fmt.Sprintf("interrupted (server restarted while vision %s)", prev)
		}
		j.VisionMessage = "画面理解被服务重启中断"
		if j.VisionFinishedAt == nil {
			t := now
			j.VisionFinishedAt = &t
		}
		j.Logs = append(j.Logs, LogLine{
			At:      now,
			Phase:   PhaseAnalyzing,
			Message: "服务重启，画面理解被中断",
		})
		if err := s.persistLocked(j); err != nil {
			log.Printf("store: persist stale vision recovery for %s: %v", j.ID, err)
			continue
		}
		n++
	}
	return n
}

// migrateSimplifiedLocked walks every loaded job and rewrites any
// transcript whose language is "zh" to Simplified Chinese, in place.
// It's idempotent: jobs already marked SimplifiedAt are skipped, and
// new jobs (where ASR already normalises to "zh-Hans") never match.
//
// Called from New() before the store is exposed, so we don't bother
// with the public mutex.
func (s *Store) migrateSimplifiedLocked() int {
	now := time.Now().UTC()
	var migrated int
	for _, j := range s.jobs {
		if j.SimplifiedAt != nil || j.Transcript == nil {
			continue
		}
		if j.Transcript.Language != "zh" {
			continue
		}
		for i, seg := range j.Transcript.Segments {
			j.Transcript.Segments[i].Text = asr.ToSimplified(seg.Text)
		}
		j.Transcript.FullText = asr.ToSimplified(j.Transcript.FullText)
		j.Transcript.Language = "zh-Hans"
		t := now
		j.SimplifiedAt = &t
		if err := s.persistLocked(j); err != nil {
			log.Printf("store: persist simplified job %s: %v", j.ID, err)
			continue
		}
		migrated++
	}
	return migrated
}

func (s *Store) loadAll() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var j Job
		if err := json.Unmarshal(raw, &j); err != nil {
			continue
		}
		s.jobs[j.ID] = &j
	}
	return nil
}

func (s *Store) Create(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j.ID == "" {
		return errors.New("job id required")
	}
	if _, exists := s.jobs[j.ID]; exists {
		return fmt.Errorf("job %s already exists", j.ID)
	}
	s.jobs[j.ID] = j
	return s.persistLocked(j)
}

// Update applies mutator to the job in place under the lock and writes
// it back to disk. Returns an updated copy.
func (s *Store) Update(id string, mutate func(*Job)) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s not found", id)
	}
	mutate(j)
	if err := s.persistLocked(j); err != nil {
		return nil, err
	}
	clone := *j
	return &clone, nil
}

func (s *Store) Get(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	clone := *j
	return &clone, true
}

func (s *Store) List() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		clone := *j
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.After(out[k].CreatedAt) })
	return out
}

// SetSummary stores (or overwrites) one summary artefact under the
// job. kind is the summary.Kind string value; the wire format never
// touches the summary package so this method stays import-loop-free.
func (s *Store) SetSummary(id, kind string, entry *SummaryEntry) error {
	_, err := s.Update(id, func(j *Job) {
		if j.Summaries == nil {
			j.Summaries = map[string]*SummaryEntry{}
		}
		j.Summaries[kind] = entry
	})
	return err
}

// SummaryStatusOf returns the persisted status for one kind without
// loading the full job. Returns ("", false) when no entry exists yet.
// Used by the HTTP layer for the in-flight concurrency check.
func (s *Store) SummaryStatusOf(id, kind string) (SummaryStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.Summaries == nil {
		return "", false
	}
	entry, ok := j.Summaries[kind]
	if !ok || entry == nil {
		return "", false
	}
	return entry.Status, true
}

// SetSummaryStatus mutates only the status (+ error message on
// failure) of an existing entry. Use when the entry already exists
// (e.g. pending → failed transition); for the pending → done case
// callers pass the full new entry via SetSummary so all the result
// fields land atomically.
func (s *Store) SetSummaryStatus(id, kind string, status SummaryStatus, errMsg string) error {
	_, err := s.Update(id, func(j *Job) {
		if j.Summaries == nil {
			j.Summaries = map[string]*SummaryEntry{}
		}
		entry := j.Summaries[kind]
		if entry == nil {
			entry = &SummaryEntry{Kind: kind, GeneratedAt: time.Now().UTC()}
			j.Summaries[kind] = entry
		}
		entry.Status = status
		entry.Error = errMsg
	})
	return err
}

// SetChapters overwrites chapterized output for a job atomically.
func (s *Store) SetChapters(id string, chapters []Chapter, model string, generatedAt time.Time) error {
	_, err := s.Update(id, func(j *Job) {
		if len(chapters) == 0 {
			j.Chapters = nil
		} else {
			j.Chapters = append([]Chapter(nil), chapters...)
		}
		j.ChaptersModel = model
		t := generatedAt.UTC()
		j.ChaptersGeneratedAt = &t
	})
	return err
}

// AppendQAMessages appends turns into a per-job QA session. When the
// session doesn't exist yet, it is created.
func (s *Store) AppendQAMessages(id, sessionID string, msgs ...QAMessage) (*QASession, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session id required")
	}
	updated, err := s.Update(id, func(j *Job) {
		now := time.Now().UTC()
		idx := -1
		for i := range j.QASessions {
			if j.QASessions[i].ID == sessionID {
				idx = i
				break
			}
		}
		if idx < 0 {
			j.QASessions = append(j.QASessions, QASession{
				ID:        sessionID,
				CreatedAt: now,
				UpdatedAt: now,
				Messages:  nil,
			})
			idx = len(j.QASessions) - 1
		}
		sess := &j.QASessions[idx]
		for _, m := range msgs {
			if m.Role == "" || m.Content == "" {
				continue
			}
			if m.At.IsZero() {
				m.At = now
			}
			sess.Messages = append(sess.Messages, m)
		}
		sess.UpdatedAt = now
	})
	if err != nil {
		return nil, err
	}
	for i := range updated.QASessions {
		if updated.QASessions[i].ID == sessionID {
			cp := updated.QASessions[i]
			cp.Messages = append([]QAMessage(nil), cp.Messages...)
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("session %s not found after update", sessionID)
}

// recoverStaleSummariesLocked normalises summary entries at boot.
// Returns the counts of each transition for the caller to log.
//
//	pre-status records (Markdown filled but Status empty) → done
//	pending records left over from a crashed/restarted process → failed
//
// Without this a server restart in the middle of a generate would
// leave the UI spinning forever, and entries written before the
// Status field existed would render as "still generating".
func (s *Store) recoverStaleSummariesLocked() (legacy, stale int) {
	for _, j := range s.jobs {
		if j.Summaries == nil {
			continue
		}
		dirty := false
		for k, e := range j.Summaries {
			if e == nil {
				continue
			}
			switch {
			case e.Status == "":
				// Legacy entry written before Status existed —
				// any record on disk implies completion.
				e.Status = SummaryDone
				dirty = true
				legacy++
			case e.Status == SummaryPending:
				e.Status = SummaryFailed
				if e.Error == "" {
					e.Error = "server restarted while generating"
				}
				dirty = true
				stale++
			}
			j.Summaries[k] = e
		}
		if dirty {
			if err := s.persistLocked(j); err != nil {
				log.Printf("store: persist stale summary recovery for %s: %v", j.ID, err)
			}
		}
	}
	return legacy, stale
}

// Delete removes the job from memory and unlinks its on-disk JSON file.
// It returns a copy of the deleted job so callers (jobs.Manager) can
// reach Source.ID / MediaPath to clean up media files outside the lock.
//
// We intentionally do NOT touch downloads / models / work here; the
// store only owns <dir>/<id>.json. Media cleanup is the jobs layer's
// job, so it can apply its own safety policy.
func (s *Store) Delete(id string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, fmt.Errorf("job %s not found", id)
	}
	clone := *j
	path := filepath.Join(s.dir, id+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove %s: %w", path, err)
	}
	delete(s.jobs, id)
	return &clone, nil
}

func (s *Store) persistLocked(j *Job) error {
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, j.ID+".json.tmp")
	final := filepath.Join(s.dir, j.ID+".json")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}
