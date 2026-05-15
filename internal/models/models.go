// Package models manages the Whisper ggml model files on disk. Mirror
// of the upstream Scribe model package, slimmed down for the web server
// (no Wails event emit; progress is a plain callback).
package models

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"scribe-web/internal/config"
)

// Spec describes one downloadable model.
type Spec struct {
	Key      string `json:"key"`
	Filename string `json:"filename"`
	Bytes    int64  `json:"bytes"`
	Label    string `json:"label"`
}

// Known is the fixed catalogue exposed to the UI.
var Known = []Spec{
	{Key: "tiny", Filename: "ggml-tiny.bin", Bytes: 77 * 1024 * 1024, Label: "Tiny · 77 MB · 极快"},
	{Key: "base", Filename: "ggml-base.bin", Bytes: 148 * 1024 * 1024, Label: "Base · 148 MB · 快"},
	{Key: "small", Filename: "ggml-small.bin", Bytes: 488 * 1024 * 1024, Label: "Small · 488 MB · 均衡"},
	{Key: "medium", Filename: "ggml-medium.bin", Bytes: 1530 * 1024 * 1024, Label: "Medium · 1.5 GB · 慢"},
}

// Status is what /api/models returns.
type Status struct {
	Spec
	Installed bool `json:"installed"`
}

func SpecByKey(key string) (Spec, bool) {
	for _, s := range Known {
		if s.Key == key {
			return s, true
		}
	}
	return Spec{}, false
}

func List(cfg *config.Config) []Status {
	out := make([]Status, 0, len(Known))
	for _, s := range Known {
		_, err := os.Stat(cfg.ModelFilePath(s.Key))
		out = append(out, Status{Spec: s, Installed: err == nil})
	}
	return out
}

// Manager guards concurrent downloads and exposes progress to anyone
// who subscribes (today: the SSE handler).
type Manager struct {
	cfg *config.Config

	mu       sync.Mutex
	active   map[string]*Download
	progress map[string]Progress
}

// Progress is a snapshot of the current state for one model key.
type Progress struct {
	Key      string  `json:"key"`
	Fraction float64 `json:"fraction"`
	Message  string  `json:"message"`
	Done     bool    `json:"done"`
	Error    string  `json:"error,omitempty"`
}

// Download tracks a single in-flight download.
type Download struct {
	cancel context.CancelFunc
}

func NewManager(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg, active: map[string]*Download{}, progress: map[string]Progress{}}
}

// Snapshot returns the latest known progress for every model that's
// either downloading or recently completed. Used by the UI on first
// load.
func (m *Manager) Snapshot() []Progress {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Progress, 0, len(m.progress))
	for _, p := range m.progress {
		out = append(out, p)
	}
	return out
}

// Start kicks off a background download. Re-issuing while a download
// is active is a no-op.
func (m *Manager) Start(key string, onUpdate func(Progress)) error {
	spec, ok := SpecByKey(key)
	if !ok {
		return fmt.Errorf("unknown model: %s", key)
	}

	m.mu.Lock()
	if _, busy := m.active[key]; busy {
		m.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.active[key] = &Download{cancel: cancel}
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.active, key)
			m.mu.Unlock()
		}()
		err := m.download(ctx, spec, func(p Progress) {
			m.mu.Lock()
			m.progress[p.Key] = p
			m.mu.Unlock()
			if onUpdate != nil {
				onUpdate(p)
			}
		})
		final := Progress{Key: key, Fraction: 1.0, Message: "完成", Done: true}
		if err != nil {
			final = Progress{Key: key, Done: true, Error: err.Error(), Message: err.Error()}
		}
		m.mu.Lock()
		m.progress[key] = final
		m.mu.Unlock()
		if onUpdate != nil {
			onUpdate(final)
		}
	}()
	return nil
}

func (m *Manager) download(ctx context.Context, spec Spec, cb func(Progress)) error {
	finalPath := m.cfg.ModelFilePath(spec.Key)
	if _, err := os.Stat(finalPath); err == nil {
		cb(Progress{Key: spec.Key, Fraction: 1.0, Message: "已安装"})
		return nil
	}

	url := fmt.Sprintf("%s/%s", m.cfg.ModelBaseURL, spec.Filename)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch model: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	partPath := finalPath + ".part"
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	total := spec.Bytes
	if resp.ContentLength > 0 {
		total = resp.ContentLength
	}

	buf := make([]byte, 1<<20)
	var written int64
	last := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return werr
			}
			written += int64(n)
			if time.Since(last) > 200*time.Millisecond || rerr != nil {
				frac := 0.0
				if total > 0 {
					frac = float64(written) / float64(total)
				}
				cb(Progress{
					Key:      spec.Key,
					Fraction: frac,
					Message:  fmt.Sprintf("%s / %s", humanBytes(written), humanBytes(total)),
				})
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return rerr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(partPath, finalPath)
}

func humanBytes(n int64) string {
	const (
		mb = 1 << 20
		gb = 1 << 30
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
