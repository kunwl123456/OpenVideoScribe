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
	"scribe-web/internal/ytdlp"
)

// Phase describes which step of the pipeline a job is currently in.
type Phase string

const (
	PhaseQueued       Phase = "queued"
	PhaseDownloading  Phase = "downloading"
	PhaseExtracting   Phase = "extracting"
	PhaseTranscribing Phase = "transcribing"
	PhaseDone         Phase = "done"
	PhaseFailed       Phase = "failed"
)

// Job is the canonical record persisted on disk.
type Job struct {
	ID            string        `json:"id"`
	URL           string        `json:"url"`
	Model         string        `json:"model"`
	Language      string        `json:"language"`
	Phase         Phase         `json:"phase"`
	Message       string        `json:"message,omitempty"`
	Error         string        `json:"error,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	StartedAt     *time.Time    `json:"started_at,omitempty"`
	FinishedAt    *time.Time    `json:"finished_at,omitempty"`
	Source        *ytdlp.Info   `json:"source,omitempty"`
	Transcript    *asr.Result   `json:"transcript,omitempty"`
	MediaPath     string        `json:"media_path,omitempty"`
	Logs          []LogLine     `json:"logs,omitempty"`
	Progress      map[Phase]int `json:"progress,omitempty"` // 0..100 per phase
	SimplifiedAt  *time.Time    `json:"simplified_at,omitempty"`
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
	return s, nil
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
		// Anything that was mid-flight when the server died is treated
		// as failed; we don't have crash-resume yet.
		if j.Phase != PhaseDone && j.Phase != PhaseFailed {
			j.Phase = PhaseFailed
			if j.Error == "" {
				j.Error = "interrupted (server restarted)"
			}
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
