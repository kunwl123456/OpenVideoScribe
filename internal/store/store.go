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
	// PhaseAnalyzing covers keyframe extraction + per-frame VLM
	// description. It only fires when the visual stage is configured
	// (cfg.VLM.Enabled()); otherwise jobs jump from PhaseTranscribing
	// straight to PhaseDone.
	PhaseAnalyzing Phase = "analyzing"
	PhaseDone      Phase = "done"
	PhaseFailed    Phase = "failed"
)

// Job is the canonical record persisted on disk.
type Job struct {
	ID            string                   `json:"id"`
	URL           string                   `json:"url"`
	Model         string                   `json:"model"`
	Language      string                   `json:"language"`
	Phase         Phase                    `json:"phase"`
	Message       string                   `json:"message,omitempty"`
	Error         string                   `json:"error,omitempty"`
	CreatedAt     time.Time                `json:"created_at"`
	StartedAt     *time.Time               `json:"started_at,omitempty"`
	FinishedAt    *time.Time               `json:"finished_at,omitempty"`
	Source        *ytdlp.Info              `json:"source,omitempty"`
	Transcript    *asr.Result              `json:"transcript,omitempty"`
	MediaPath     string                   `json:"media_path,omitempty"`
	ThumbnailPath string                   `json:"thumbnail_path,omitempty"`
	// FramesDir is the per-job directory holding extracted keyframe
	// JPGs + ffmpeg's metadata sidecar. Empty when the visual stage
	// hasn't run for this job (vision disabled or skipped).
	FramesDir string `json:"frames_dir,omitempty"`
	// Frames are the per-timestamp visual insights produced by the
	// VLM stage. Sorted by TimestampSec ascending; empty when visual
	// stage didn't run or yielded nothing.
	Frames       []vision.Insight         `json:"frames,omitempty"`
	Logs         []LogLine                `json:"logs,omitempty"`
	Progress     map[Phase]int            `json:"progress,omitempty"` // 0..100 per phase
	SimplifiedAt *time.Time               `json:"simplified_at,omitempty"`
	Summaries    map[string]*SummaryEntry `json:"summaries,omitempty"` // key is summary.Kind string
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
		log.Printf("store: job recovery — marked %d zombie job(s) as failed (server restart while running)", n)
	}
	if legacy, stale := s.recoverStaleSummariesLocked(); legacy+stale > 0 {
		log.Printf("store: summary recovery — promoted %d legacy entr(ies) to done, marked %d stale pending as failed", legacy, stale)
	}
	return s, nil
}

// recoverStaleJobsLocked finalises any job whose phase indicates it
// was running when the server died. The previous goroutine is gone
// and we don't support crash-resume, so the only honest thing to do
// is mark them failed — otherwise the UI shows them spinning forever.
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
		if j.Phase == PhaseDone || j.Phase == PhaseFailed {
			continue
		}
		prev := j.Phase
		j.Phase = PhaseFailed
		if j.Error == "" {
			if prev == "" {
				j.Error = "interrupted (server restarted)"
			} else {
				j.Error = fmt.Sprintf("interrupted (server restarted while %s)", prev)
			}
		}
		if j.FinishedAt == nil {
			t := now
			j.FinishedAt = &t
		}
		j.Logs = append(j.Logs, LogLine{
			At:      now,
			Phase:   PhaseFailed,
			Message: "服务重启，任务被中断",
		})
		if err := s.persistLocked(j); err != nil {
			log.Printf("store: persist stale job recovery for %s: %v", j.ID, err)
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
