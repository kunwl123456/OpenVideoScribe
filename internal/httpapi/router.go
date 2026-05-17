// Package httpapi exposes the REST + SSE surface that the React UI
// talks to. We use net/http + a tiny mux on purpose; no framework
// dependency for the MVP.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"scribe-web/internal/asr"
	"scribe-web/internal/config"
	"scribe-web/internal/jobs"
	"scribe-web/internal/llm"
	"scribe-web/internal/models"
	"scribe-web/internal/store"
	"scribe-web/internal/summary"
)

// Server bundles the dependencies the routes need. New() returns an
// http.Handler ready to mount.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	jobs    *jobs.Manager
	models  *models.Manager
	summary *summary.Service
	static  fs.FS // optional embedded UI; nil means dev-mode (no UI served)
}

func New(cfg *config.Config, st *store.Store, jm *jobs.Manager, mm *models.Manager, sm *summary.Service, static fs.FS) http.Handler {
	s := &Server{cfg: cfg, store: st, jobs: jm, models: mm, summary: sm, static: static}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/models/", s.handleModelDownload) // POST /api/models/{key}/download
	mux.HandleFunc("/api/jobs", s.handleJobs)             // GET list, POST create
	mux.HandleFunc("/api/jobs/", s.handleJobByID)         // GET / DELETE / events / export / summarize

	if static != nil {
		mux.Handle("/", s.handleStatic())
	}

	return withCORS(mux)
}

// ---------------- handlers ----------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	llmInfo := map[string]any{"enabled": false}
	if s.cfg.LLM != nil && s.cfg.LLM.Enabled() {
		red := s.cfg.LLM.Redacted()
		llmInfo = map[string]any{
			"enabled":     true,
			"base_url":    red.BaseURL,
			"model":       red.Model,
			"api_key":     red.APIKey,
			"timeout_s":   red.TimeoutSeconds,
			"max_tokens":  red.MaxTokens,
			"temperature": red.Temperature,
		}
	} else {
		llmInfo["hint"] = "复制 scribe-llm.example.json 为 scribe-llm.json，填入 base_url / api_key / model；或设置 SCRIBE_LLM_API_KEY 等环境变量"
	}
	vlmInfo := map[string]any{"enabled": false}
	if s.cfg.VLM != nil && s.cfg.VLM.Enabled() {
		red := s.cfg.VLM.Redacted()
		vlmInfo = map[string]any{
			"enabled":          true,
			"base_url":         red.BaseURL,
			"model":            red.Model,
			"api_key":          red.APIKey,
			"timeout_s":        red.TimeoutSeconds,
			"frame_interval_s": red.FrameIntervalSeconds,
			"scene_threshold":  red.SceneThreshold,
			"max_frames":       red.MaxFrames,
			"concurrency":      red.Concurrency,
		}
	} else {
		vlmInfo["hint"] = "可选：复制 scribe-vlm.example.json 为 scribe-vlm.json 启用画面理解"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"time":     time.Now().UTC(),
		"data_dir": s.cfg.DataDir,
		"binaries": s.binaryStatus(),
		"llm":      llmInfo,
		"vlm":      vlmInfo,
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
	case len(parts) == 1 && r.Method == http.MethodDelete:
		if err := s.jobs.Delete(id); err != nil {
			if errors.Is(err, jobs.ErrJobInProgress) {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 1:
		writeJSON(w, http.StatusOK, job)
	case len(parts) == 2 && parts[1] == "events":
		s.streamEvents(w, r, id)
	case len(parts) == 2 && parts[1] == "export":
		s.exportTranscript(w, r, job)
	case len(parts) == 2 && parts[1] == "summarize":
		s.handleSummarize(w, r, job)
	case len(parts) == 2 && parts[1] == "thumbnail":
		s.handleThumbnail(w, r, job)
	case len(parts) == 3 && parts[1] == "frames":
		s.handleFrame(w, r, job, parts[2])
	default:
		http.NotFound(w, r)
	}
}

// handleThumbnail serves the on-disk poster file. Returns 404 when the
// job has no thumbnail recorded (older records, fetch failed, or the
// upstream simply didn't expose one) so the React UI can fall back to
// its gradient placeholder via the <img> onError hook.
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request, job *store.Job) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if job.ThumbnailPath == "" {
		http.NotFound(w, r)
		return
	}
	// Path-traversal defence: file must live under ThumbnailsDir.
	abs, err := filepath.Abs(job.ThumbnailPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	thDir, err := filepath.Abs(s.cfg.ThumbnailsDir)
	if err != nil || filepath.Dir(abs) != thDir {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, abs, info.ModTime(), f)
}

// handleFrame serves one extracted keyframe JPEG identified by its
// index in job.Frames. Indexes outside the slice answer 404. Same
// path-traversal defence as handleThumbnail: the file must live
// directly inside the per-job subdir under cfg.FramesDir.
func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request, job *store.Job, idxStr string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 || idx >= len(job.Frames) {
		http.NotFound(w, r)
		return
	}
	imgPath := job.Frames[idx].ImagePath
	if imgPath == "" || job.FramesDir == "" || s.cfg.FramesDir == "" {
		http.NotFound(w, r)
		return
	}
	abs, err := filepath.Abs(imgPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobDir, err := filepath.Abs(job.FramesDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	framesRoot, err := filepath.Abs(s.cfg.FramesDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Two layers of containment:
	//   1. The image must sit directly inside the job's frames dir.
	//   2. That job dir must sit directly inside cfg.FramesDir.
	if filepath.Dir(abs) != jobDir || filepath.Dir(jobDir) != framesRoot {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, abs, info.ModTime(), f)
}

// handleSummarize triggers an LLM summary asynchronously. The request
// is acknowledged with 202 + a pending entry written to the store; a
// background goroutine then runs the actual Chat call and persists the
// final entry (done or failed). This lets the React UI recover state
// after a route change / page refresh by simply re-fetching the job —
// in-flight generates are no longer ephemeral browser state.
//
// Concurrency:
//   - If a pending entry already exists for (job, kind), we return
//     409 Conflict with the pending body so the client recognises the
//     in-flight state without dispatching a duplicate goroutine.
//   - Re-running an already-done kind is allowed and clobbers the
//     prior entry; clients explicitly opt in via "重新生成".
func (s *Server) handleSummarize(w http.ResponseWriter, r *http.Request, job *store.Job) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.LLM == nil || !s.cfg.LLM.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "LLM 未配置",
			"hint":  "复制 scribe-llm.example.json 为 scribe-llm.json，填入 api_key 和 model；或设置 SCRIBE_LLM_API_KEY / SCRIBE_LLM_MODEL 环境变量",
		})
		return
	}
	if job.Transcript == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":  "transcript 尚未生成，无法总结",
			"status": string(store.SummaryFailed),
		})
		return
	}
	kind, err := summary.ParseKind(r.URL.Query().Get("kind"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	kindStr := string(kind)

	// Concurrency guard: bail early if another goroutine is already
	// generating this kind for this job. The check + atomic transition
	// to pending live in the store layer so two simultaneous POSTs
	// can't both win.
	if status, ok := s.store.SummaryStatusOf(job.ID, kindStr); ok && status == store.SummaryPending {
		writeJSON(w, http.StatusConflict, map[string]string{
			"status": string(store.SummaryPending),
			"kind":   kindStr,
			"error":  "已有同类总结正在生成，请稍候",
		})
		return
	}

	meta := summary.Metadata{
		Frames: job.Frames,
	}
	if job.Source != nil {
		meta.Title = job.Source.Title
		meta.Uploader = job.Source.Uploader
		meta.DurationSeconds = int(job.Source.Duration)
	}
	if meta.Title == "" {
		meta.Title = job.URL
	}

	startedAt := time.Now().UTC()
	pending := &store.SummaryEntry{
		Kind:        kindStr,
		Status:      store.SummaryPending,
		GeneratedAt: startedAt,
	}
	if err := s.store.SetSummary(job.ID, kindStr, pending); err != nil {
		http.Error(w, "persist pending summary: "+err.Error(), http.StatusInternalServerError)
		return
	}

	go s.runSummary(job.ID, kindStr, job.Transcript, meta, startedAt)

	writeJSON(w, http.StatusAccepted, pending)
}

// runSummary executes a summary generation on a detached background
// context (we deliberately don't bind to the HTTP request context —
// the caller is long gone by the time we return) and persists the
// terminal entry.
func (s *Server) runSummary(jobID, kindStr string, transcript *asr.Result, meta summary.Metadata, startedAt time.Time) {
	timeout := s.cfg.LLM.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	kind, err := summary.ParseKind(kindStr)
	if err != nil {
		_ = s.store.SetSummaryStatus(jobID, kindStr, store.SummaryFailed, err.Error())
		return
	}

	res, err := s.summary.Generate(ctx, transcript, kind, meta)
	if err != nil {
		_, body := mapSummaryError(err)
		msg := body["error"]
		if detail := body["detail"]; detail != "" {
			msg = msg + ": " + detail
		}
		log.Printf("summary: %s/%s failed: %v", jobID, kindStr, err)
		_ = s.store.SetSummaryStatus(jobID, kindStr, store.SummaryFailed, msg)
		return
	}

	entry := &store.SummaryEntry{
		Kind:              kindStr,
		Status:            store.SummaryDone,
		Markdown:          res.Markdown,
		Model:             res.Model,
		TokensUsed:        res.TokensUsed,
		PromptTokens:      res.PromptTokens,
		CompletionTokens:  res.CompletionTokens,
		EstimatedCost:     res.EstimatedCost,
		EstimatedCostText: res.EstimatedCostText,
		DurationMs:        res.DurationMs,
		GeneratedAt:       res.GeneratedAt,
	}
	if err := s.store.SetSummary(jobID, kindStr, entry); err != nil {
		log.Printf("summary: persist done entry for %s/%s: %v", jobID, kindStr, err)
	}
}

func mapSummaryError(err error) (int, map[string]string) {
	switch {
	case errors.Is(err, llm.ErrNoAPIKey):
		return http.StatusUnauthorized, map[string]string{"error": "LLM 鉴权失败", "detail": err.Error()}
	case errors.Is(err, llm.ErrRateLimited):
		return http.StatusTooManyRequests, map[string]string{"error": "LLM 限流，请稍后重试", "detail": err.Error()}
	case errors.Is(err, llm.ErrTimeout):
		return http.StatusGatewayTimeout, map[string]string{"error": "LLM 响应超时", "detail": err.Error()}
	case errors.Is(err, llm.ErrUpstream):
		return http.StatusBadGateway, map[string]string{"error": "LLM 上游错误", "detail": err.Error()}
	}
	return http.StatusInternalServerError, map[string]string{"error": err.Error()}
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
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
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
