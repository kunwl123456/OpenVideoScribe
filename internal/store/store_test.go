package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeJobJSON drops a minimal job record onto disk under <dir>/jobs/<id>.json
// so we can exercise loadAll + recovery without going through Create().
func writeJobJSON(t *testing.T, dataDir string, j *Job) {
	t.Helper()
	dir := filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir jobs: %v", err)
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, j.ID+".json"), raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestRecoverStaleJobs_MarksMidFlightAsFailedAndPersists(t *testing.T) {
	dataDir := t.TempDir()
	created := time.Date(2026, 5, 16, 19, 9, 34, 0, time.UTC)

	// Three jobs that were in motion when the server died.
	writeJobJSON(t, dataDir, &Job{ID: "zombie-dl", Phase: PhaseDownloading, CreatedAt: created})
	writeJobJSON(t, dataDir, &Job{ID: "zombie-tx", Phase: PhaseTranscribing, CreatedAt: created})
	writeJobJSON(t, dataDir, &Job{ID: "zombie-an", Phase: PhaseAnalyzing, CreatedAt: created})
	// One healthy job that must NOT be touched.
	finished := created.Add(2 * time.Minute)
	writeJobJSON(t, dataDir, &Job{ID: "healthy", Phase: PhaseDone, CreatedAt: created, FinishedAt: &finished})
	// One pre-existing failure that must NOT be re-written either.
	writeJobJSON(t, dataDir, &Job{ID: "old-fail", Phase: PhaseFailed, Error: "boom", CreatedAt: created, FinishedAt: &finished})

	s, err := New(dataDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		id      string
		wantErr string
	}{
		{"zombie-dl", "interrupted (server restarted while downloading)"},
		{"zombie-tx", "interrupted (server restarted while transcribing)"},
		{"zombie-an", "interrupted (server restarted while analyzing)"},
	}
	for _, tc := range cases {
		j, ok := s.Get(tc.id)
		if !ok {
			t.Fatalf("%s: not in store", tc.id)
		}
		if j.Phase != PhaseFailed {
			t.Errorf("%s: phase = %s, want failed", tc.id, j.Phase)
		}
		if j.Error != tc.wantErr {
			t.Errorf("%s: error = %q, want %q", tc.id, j.Error, tc.wantErr)
		}
		if j.FinishedAt == nil {
			t.Errorf("%s: FinishedAt nil, want set", tc.id)
		}
		if len(j.Logs) == 0 || j.Logs[len(j.Logs)-1].Message != "服务重启，任务被中断" {
			t.Errorf("%s: expected final log line about restart, got %+v", tc.id, j.Logs)
		}
	}

	// Healthy + pre-failed jobs must be untouched.
	if h, _ := s.Get("healthy"); h.Phase != PhaseDone || len(h.Logs) != 0 {
		t.Errorf("healthy mutated: phase=%s logs=%d", h.Phase, len(h.Logs))
	}
	if f, _ := s.Get("old-fail"); f.Phase != PhaseFailed || f.Error != "boom" {
		t.Errorf("old-fail mutated: phase=%s err=%q", f.Phase, f.Error)
	}

	// Critical: recovery must be persisted, so a second New() over the
	// same dir is a no-op and doesn't re-recover the same jobs.
	s2, err := New(dataDir)
	if err != nil {
		t.Fatalf("New (2nd): %v", err)
	}
	for _, tc := range cases {
		j, _ := s2.Get(tc.id)
		if j.Phase != PhaseFailed || j.Error != tc.wantErr {
			t.Errorf("%s: state lost across restart (phase=%s err=%q)", tc.id, j.Phase, j.Error)
		}
	}

	// And verify the on-disk file truly carries the failed state.
	raw, err := os.ReadFile(filepath.Join(dataDir, "jobs", "zombie-tx.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var disk Job
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if disk.Phase != PhaseFailed || disk.FinishedAt == nil {
		t.Errorf("on-disk job not persisted: phase=%s finished=%v", disk.Phase, disk.FinishedAt)
	}
}

func TestRecoverStaleJobs_PreservesExistingError(t *testing.T) {
	dataDir := t.TempDir()
	writeJobJSON(t, dataDir, &Job{
		ID:    "with-err",
		Phase: PhaseTranscribing,
		Error: "whisper segfault",
	})
	s, err := New(dataDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	j, _ := s.Get("with-err")
	if j.Phase != PhaseFailed {
		t.Errorf("phase = %s, want failed", j.Phase)
	}
	if j.Error != "whisper segfault" {
		t.Errorf("error overwritten: %q", j.Error)
	}
}
