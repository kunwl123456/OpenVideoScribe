// Package httpapi exposes the REST + SSE surface that the React UI
// talks to. We use net/http + a tiny mux on purpose; no framework
// dependency for the MVP.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"scribe-web/internal/config"
	"scribe-web/internal/jobs"
	"scribe-web/internal/models"
	"scribe-web/internal/store"
)

// Server bundles the dependencies the routes need. New() returns an
// http.Handler ready to mount.
type Server struct {
	cfg    *config.Config
	store  *store.Store
	jobs   *jobs.Manager
	models *models.Manager
	static fs.FS // optional embedded UI; nil means dev-mode (no UI served)
}

func New(cfg *config.Config, st *store.Store, jm *jobs.Manager, mm *models.Manager, static fs.FS) http.Handler {
	s := &Server{cfg: cfg, store: st, jobs: jm, models: mm, static: static}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/models/", s.handleModelDownload) // POST /api/models/{key}/download
	mux.HandleFunc("/api/jobs", s.handleJobs)             // GET list, POST create
	mux.HandleFunc("/api/jobs/", s.handleJobByID)         // GET /api/jobs/{id}, /api/jobs/{id}/events, /api/jobs/{id}/export

	if static != nil {
		mux.Handle("/", s.handleStatic())
	}

	return withCORS(mux)
}

// ---------------- handlers ----------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"time":       time.Now().UTC(),
		"data_dir":   s.cfg.DataDir,
		"binaries":   s.binaryStatus(),
	})
}

func (s *Server) binaryStatus() map[string]string {
	return map[string]string{
		"ffmpeg":      s.cfg.FFmpegBin,
		"whisper-cli": s.cfg.WhisperBin,
		"yt-dlp":      s.cfg.YtDlpBin,
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":   models.List(s.cfg),
		"progress": s.models.Snapshot(),
	})
}

// handleModelDownload accepts POST /api/models/{key}/download.
func (s *Server) handleModelDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/models/"), "/")
	if len(parts) != 2 || parts[1] != "download" {
		http.Error(w, "expected /api/models/{key}/download", http.StatusBadRequest)
		return
	}
	key := parts[0]
	if err := s.models.Start(key, nil); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"jobs": s.store.List()})
	case http.MethodPost:
		var body struct {
			URL      string `json:"url"`
			Model    string `json:"model"`
			Language string `json:"language"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		j, err := s.jobs.Submit(strings.TrimSpace(body.URL), body.Model, body.Language)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusAccepted, j)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleJobByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	job, ok := s.store.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch {
	case len(parts) == 1:
		writeJSON(w, http.StatusOK, job)
	case len(parts) == 2 && parts[1] == "events":
		s.streamEvents(w, r, id)
	case len(parts) == 2 && parts[1] == "export":
		s.exportTranscript(w, r, job)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, id string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send the current snapshot first so the client doesn't need a
	// separate GET.
	if job, ok := s.store.Get(id); ok {
		writeSSE(w, "snapshot", job)
		flusher.Flush()
	}

	ch, unsub := s.jobs.Subscribe(id)
	defer unsub()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			writeSSE(w, "event", ev)
			flusher.Flush()
			if ev.Done {
				return
			}
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) exportTranscript(w http.ResponseWriter, r *http.Request, job *store.Job) {
	if job.Transcript == nil {
		http.Error(w, "transcript not ready", http.StatusConflict)
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "srt"
	}
	body, ct, err := formatExport(job.Transcript, format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filename := fmt.Sprintf("%s.%s", job.ID, format)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	_, _ = w.Write(body)
}

// ---------------- static UI ----------------

func (s *Server) handleStatic() http.Handler {
	fileServer := http.FileServer(http.FS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Anything under /api/* should never reach here, but guard
		// just in case the mux misroutes.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		// SPA fallback: if the requested path doesn't exist as a
		// real asset, serve index.html so React Router can take over.
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(s.static, path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/"
				fileServer.ServeHTTP(w, r2)
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

// ---------------- helpers ----------------

func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeSSE(w http.ResponseWriter, event string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\n", event)
	fmt.Fprintf(w, "data: %s\n\n", raw)
}
