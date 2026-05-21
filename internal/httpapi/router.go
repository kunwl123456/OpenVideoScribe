// Package httpapi exposes the REST + SSE surface that the React UI
// talks to. We use net/http + a tiny mux on purpose; no framework
// dependency for the MVP.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"scribe-web/internal/notion"
	"scribe-web/internal/qa"
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
	qa      *qa.Service
	notion  *notion.Service
	static  fs.FS // optional embedded UI; nil means dev-mode (no UI served)
}

func New(cfg *config.Config, st *store.Store, jm *jobs.Manager, mm *models.Manager, sm *summary.Service, qaSvc *qa.Service, notionSvc *notion.Service, static fs.FS) http.Handler {
	s := &Server{cfg: cfg, store: st, jobs: jm, models: mm, summary: sm, qa: qaSvc, notion: notionSvc, static: static}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/models", s.handleModels)
	mux.HandleFunc("/api/models/", s.handleModelDownload) // POST /api/models/{key}/download
	mux.HandleFunc("/api/qa", s.handleGlobalQA)           // POST global KB QA
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
	notionInfo := map[string]any{"enabled": false}
	if s.cfg.Notion != nil && s.cfg.Notion.Enabled() {
		red := s.cfg.Notion.Redacted()
		notionInfo = map[string]any{
			"enabled":        true,
			"token":          red.Token,
			"page_id":        red.PageID,
			"database_id":    red.DatabaseID,
			"notion_version": red.NotionVersion,
		}
	} else {
		notionInfo["hint"] = "可选：复制 scribe-notion.example.json 为 scribe-notion.json，填写 token 与 page_id 或 database_id"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"time":     time.Now().UTC(),
		"data_dir": s.cfg.DataDir,
		"binaries": s.binaryStatus(),
		"tasks": map[string]any{
			"worker_concurrency":  s.cfg.WorkerConcurrency,
			"job_retry_count":     s.cfg.JobRetryCount,
			"summary_retry_count": s.cfg.SummaryRetryCount,
		},
		"llm":    llmInfo,
		"vlm":    vlmInfo,
		"notion": notionInfo,
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
			URL          string `json:"url"`
			Model        string `json:"model"`
			Language     string `json:"language"`
			EnableVision bool   `json:"enable_vision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}
		j, err := s.jobs.Submit(strings.TrimSpace(body.URL), body.Model, body.Language, body.EnableVision)
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
	case len(parts) == 3 && parts[1] == "export" && parts[2] == "notion":
		s.handleExportNotion(w, r, job)
	case len(parts) == 2 && parts[1] == "export":
		s.exportTranscript(w, r, job)
	case len(parts) == 2 && parts[1] == "summarize":
		s.handleSummarize(w, r, job)
	case len(parts) == 2 && parts[1] == "qa":
		s.handleQA(w, r, job)
	case len(parts) == 2 && parts[1] == "chapters":
		s.handleChapters(w, r, job)
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
		writeJSON(w, http.StatusServiceUnavailable, llmUnconfiguredBody())
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

func (s *Server) handleQA(w http.ResponseWriter, r *http.Request, job *store.Job) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.LLM == nil || !s.cfg.LLM.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, llmUnconfiguredBody())
		return
	}
	if s.qa == nil {
		writeJSON(w, http.StatusServiceUnavailable, llmUnconfiguredBody())
		return
	}
	if job.Transcript == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "transcript 尚未生成，无法问答",
		})
		return
	}
	if len(job.Transcript.Segments) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "transcript 无可检索分段，无法问答",
		})
		return
	}
	var body struct {
		Question     string `json:"question"`
		TopK         int    `json:"top_k,omitempty"`
		SessionID    string `json:"session_id,omitempty"`
		HistoryLimit int    `json:"history_limit,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Question) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "question 不能为空"})
		return
	}
	if body.HistoryLimit <= 0 {
		body.HistoryLimit = 8
	}
	if body.HistoryLimit > 30 {
		body.HistoryLimit = 30
	}
	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID == "" {
		sessionID = newSessionID()
	}
	var history []qa.ChatMessage
	for _, sess := range job.QASessions {
		if sess.ID != sessionID {
			continue
		}
		start := 0
		if len(sess.Messages) > body.HistoryLimit {
			start = len(sess.Messages) - body.HistoryLimit
		}
		for _, m := range sess.Messages[start:] {
			role := strings.TrimSpace(m.Role)
			content := strings.TrimSpace(m.Content)
			if content == "" {
				continue
			}
			history = append(history, qa.ChatMessage{Role: role, Content: content})
		}
		break
	}

	timeout := 60 * time.Second
	if s.cfg.LLM != nil && s.cfg.LLM.Timeout > 0 {
		timeout = s.cfg.LLM.Timeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	res, err := s.qa.AnswerWithContext(ctx, job.Transcript, body.Question, history, body.TopK)
	if err != nil {
		switch {
		case errors.Is(err, llm.ErrNoAPIKey):
			writeJSON(w, http.StatusServiceUnavailable, llmUnconfiguredBody())
		case errors.Is(err, llm.ErrRateLimited), errors.Is(err, llm.ErrUpstream), errors.Is(err, llm.ErrTimeout):
			code, body := mapSummaryError(err)
			writeJSON(w, code, body)
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	assistantCitations := make([]store.QACitation, 0, len(res.Citations))
	for _, c := range res.Citations {
		assistantCitations = append(assistantCitations, store.QACitation{
			JobID:    c.JobID,
			JobTitle: c.JobTitle,
			Text:     c.Text,
			Start:    c.Start,
			End:      c.End,
			Score:    c.Score,
		})
	}
	sess, err := s.store.AppendQAMessages(job.ID, sessionID,
		store.QAMessage{
			Role:    "user",
			Content: strings.TrimSpace(body.Question),
			At:      time.Now().UTC(),
		},
		store.QAMessage{
			Role:      "assistant",
			Content:   strings.TrimSpace(res.Answer),
			At:        time.Now().UTC(),
			Citations: assistantCitations,
		},
	)
	if err != nil {
		http.Error(w, "persist qa session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":     sessionID,
		"answer":         res.Answer,
		"citations":      res.Citations,
		"model":          res.Model,
		"evidence_found": res.EvidenceFound,
		"messages":       sess.Messages,
	})
}

func (s *Server) handleGlobalQA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.LLM == nil || !s.cfg.LLM.Enabled() || s.qa == nil {
		writeJSON(w, http.StatusServiceUnavailable, llmUnconfiguredBody())
		return
	}
	var body struct {
		Question string `json:"question"`
		TopK     int    `json:"top_k,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Question) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "question 不能为空"})
		return
	}
	allJobs := s.store.List()
	docs := make([]qa.GlobalDocument, 0, len(allJobs))
	for _, j := range allJobs {
		if j == nil || j.Transcript == nil || len(j.Transcript.Segments) == 0 {
			continue
		}
		title := j.URL
		if j.Source != nil && strings.TrimSpace(j.Source.Title) != "" {
			title = j.Source.Title
		}
		docs = append(docs, qa.GlobalDocument{
			JobID:    j.ID,
			JobTitle: title,
			Segments: j.Transcript.Segments,
		})
	}
	if len(docs) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "暂无可检索的视频转写数据"})
		return
	}
	timeout := 60 * time.Second
	if s.cfg.LLM != nil && s.cfg.LLM.Timeout > 0 {
		timeout = s.cfg.LLM.Timeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	res, err := s.qa.AnswerAcrossJobs(ctx, body.Question, docs, body.TopK)
	if err != nil {
		switch {
		case errors.Is(err, llm.ErrNoAPIKey):
			writeJSON(w, http.StatusServiceUnavailable, llmUnconfiguredBody())
		case errors.Is(err, llm.ErrRateLimited), errors.Is(err, llm.ErrUpstream), errors.Is(err, llm.ErrTimeout):
			code, body := mapSummaryError(err)
			writeJSON(w, code, body)
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answer":         res.Answer,
		"citations":      res.Citations,
		"model":          res.Model,
		"evidence_found": res.EvidenceFound,
	})
}

func (s *Server) handleChapters(w http.ResponseWriter, r *http.Request, job *store.Job) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.LLM == nil || !s.cfg.LLM.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, llmUnconfiguredBody())
		return
	}
	if job.Transcript == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "transcript 尚未生成，无法生成章节",
		})
		return
	}
	if len(job.Transcript.Segments) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "transcript 无可用分段，无法生成章节",
		})
		return
	}

	timeout := 60 * time.Second
	if s.cfg.LLM != nil && s.cfg.LLM.Timeout > 0 {
		timeout = s.cfg.LLM.Timeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	res, err := s.qa.GenerateChapters(ctx, job.Transcript, 12)
	if err != nil {
		switch {
		case errors.Is(err, llm.ErrNoAPIKey):
			writeJSON(w, http.StatusServiceUnavailable, llmUnconfiguredBody())
		case errors.Is(err, llm.ErrRateLimited), errors.Is(err, llm.ErrUpstream), errors.Is(err, llm.ErrTimeout):
			code, body := mapSummaryError(err)
			writeJSON(w, code, body)
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	if err := s.store.SetChapters(job.ID, res.Chapters, res.Model, res.GeneratedAt); err != nil {
		http.Error(w, "persist chapters: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
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

	kind, parseErr := summary.ParseKind(kindStr)
	if parseErr != nil {
		_ = s.store.SetSummaryStatus(jobID, kindStr, store.SummaryFailed, parseErr.Error())
		return
	}

	maxRetry := 0
	if s.cfg != nil && s.cfg.SummaryRetryCount > 0 {
		maxRetry = s.cfg.SummaryRetryCount
	}
	backoff := 1200 * time.Millisecond
	if s.cfg != nil && s.cfg.RetryBackoff > 0 {
		backoff = s.cfg.RetryBackoff
	}
	var res *summary.Result
	var err error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		res, err = s.summary.Generate(ctx, transcript, kind, meta)
		if err == nil {
			break
		}
		retryable := errors.Is(err, llm.ErrTimeout) || errors.Is(err, llm.ErrRateLimited) || errors.Is(err, llm.ErrUpstream)
		if !retryable || attempt == maxRetry {
			break
		}
		wait := backoff * time.Duration(1<<attempt)
		log.Printf("summary: %s/%s retry %d/%d after error: %v", jobID, kindStr, attempt+1, maxRetry, err)
		select {
		case <-ctx.Done():
			err = ctx.Err()
			attempt = maxRetry
		case <-time.After(wait):
		}
	}
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

func llmUnconfiguredBody() map[string]string {
	return map[string]string{
		"error": "LLM 未配置",
		"hint":  "复制 scribe-llm.example.json 为 scribe-llm.json，填入 api_key 和 model；或设置 SCRIBE_LLM_API_KEY / SCRIBE_LLM_MODEL 环境变量",
	}
}

func notionUnconfiguredBody() map[string]string {
	return map[string]string{
		"error": "Notion 未配置",
		"hint":  "复制 scribe-notion.example.json 为 scribe-notion.json，填入 token 与 page_id 或 database_id；或设置 SCRIBE_NOTION_TOKEN 等环境变量",
	}
}

func newSessionID() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	return "sess-" + time.Now().UTC().Format("20060102-150405-") + hex.EncodeToString(buf)
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
	body, ct, err := formatExport(job, format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ext := format
	if format == "md_bundle" || format == "obsidian" || format == "notion_import" || format == "xmind_outline" {
		ext = "md"
	}
	if format == "xmind_json" {
		ext = "json"
	}
	filename := fmt.Sprintf("%s.%s", job.ID, ext)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	_, _ = w.Write(body)
}

func (s *Server) handleExportNotion(w http.ResponseWriter, r *http.Request, job *store.Job) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.notion == nil || !s.notion.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, notionUnconfiguredBody())
		return
	}
	if job.Transcript == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "transcript 尚未生成，无法导出到 Notion",
		})
		return
	}
	res, err := s.notion.ExportJob(r.Context(), job)
	if err != nil {
		if errors.Is(err, notion.ErrNotConfigured) {
			writeJSON(w, http.StatusServiceUnavailable, notionUnconfiguredBody())
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":  "Notion 导出失败",
			"detail": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, res)
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
