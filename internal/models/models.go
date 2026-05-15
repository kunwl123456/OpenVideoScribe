// Package models manages the Whisper ggml model files on disk. Mirror
// of the upstream Scribe model package, slimmed down for the web server
// (no Wails event emit; progress is a plain callback).
package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"scribe-web/internal/config"
)

// httpClient is shared across downloads. We deliberately set a generous
// per-request body timeout (large models stream for minutes) but cap
// connect/TLS handshake so a dead mirror falls through to the next one
// in seconds rather than minutes.
var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   8 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	},
}

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

	bases := m.cfg.ModelBaseURLs
	if len(bases) == 0 {
		return errors.New("no model base URL configured")
	}

	var lastErr error
	for i, base := range bases {
		url := fmt.Sprintf("%s/%s", base, spec.Filename)
		mirrorTag := ""
		if len(bases) > 1 {
			mirrorTag = fmt.Sprintf("镜像 %d/%d · ", i+1, len(bases))
		}
		cb(Progress{Key: spec.Key, Message: mirrorTag + "连接中…"})

		err := fetchToFile(ctx, url, finalPath, spec, mirrorTag, cb)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil { // user cancelled — don't fall through
			return err
		}
		log.Printf("models: download %s from %s failed: %v", spec.Key, base, err)
		lastErr = err
	}
	return fmt.Errorf("all mirrors failed: %w", lastErr)
}

// fetchToFile streams one URL into <finalPath>.part and renames it on
// success. It is the only place that touches the network for a given
// mirror attempt; callers loop over mirrors.
func fetchToFile(ctx context.Context, url, finalPath string, spec Spec, mirrorTag string, cb func(Progress)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
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
	// Make sure we never leak a half-written file across mirror retries.
	defer func() {
		if err != nil {
			_ = os.Remove(partPath)
		}
	}()

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
				err = werr
				return err
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
					Message:  fmt.Sprintf("%s%s / %s", mirrorTag, humanBytes(written), humanBytes(total)),
				})
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			err = rerr
			return err
		}
	}
	if cerr := f.Close(); cerr != nil {
		err = cerr
		return err
	}
	if rerr := os.Rename(partPath, finalPath); rerr != nil {
		err = rerr
		return err
	}
	return nil
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
