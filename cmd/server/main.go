// Scribe Web server: HTTP API + embedded React UI.
package main

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"scribe-web/internal/config"
	"scribe-web/internal/httpapi"
	"scribe-web/internal/jobs"
	"scribe-web/internal/llm"
	"scribe-web/internal/models"
	"scribe-web/internal/notion"
	"scribe-web/internal/qa"
	"scribe-web/internal/store"
	"scribe-web/internal/summary"
	"scribe-web/internal/vision"
	"scribe-web/internal/vlm"
)

// staticFS is the React build output. The web/ frontend writes into
// web/dist; we mount that subtree so URLs are served from /.
//
// During plain `go build` (no UI built yet) this stays empty and the
// server runs API-only — handy in CI and for the smoke-test step.
//
//go:embed all:web_dist
var staticFS embed.FS

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("scribe-web starting; data_dir=%s addr=%s", cfg.DataDir, cfg.Addr)
	log.Printf("scribe-web: workers=%d job_retries=%d summary_retries=%d backoff=%s",
		cfg.WorkerConcurrency, cfg.JobRetryCount, cfg.SummaryRetryCount, cfg.RetryBackoff)
	logBin("ffmpeg", cfg.FFmpegBin)
	logBin("yt-dlp", cfg.YtDlpBin)
	logBin("whisper-cli", cfg.WhisperBin)

	st, err := store.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	mm := models.NewManager(cfg)
	vs := vision.New(vlm.New(cfg.VLM), cfg.VLM)
	jm := jobs.NewManager(cfg, st, vs)
	llmProv := llm.New(cfg.LLM)
	sm := summary.New(llmProv, cfg.LLM)
	qaSvc := qa.New(llmProv, cfg.LLM)
	notionSvc := notion.New(cfg.Notion)
	if cfg.LLM != nil && cfg.LLM.Enabled() {
		red := cfg.LLM.Redacted()
		log.Printf("scribe-web: llm enabled (base_url=%s model=%s key=%s)", red.BaseURL, red.Model, red.APIKey)
	} else {
		log.Printf("scribe-web: llm disabled (no api_key/model configured) — summary endpoints will return 503")
	}
	if cfg.VLM != nil && cfg.VLM.Enabled() {
		red := cfg.VLM.Redacted()
		log.Printf("scribe-web: vlm enabled (base_url=%s model=%s key=%s frames<=%d concurrency=%d)",
			red.BaseURL, red.Model, red.APIKey, red.MaxFrames, red.Concurrency)
	} else {
		log.Printf("scribe-web: vlm disabled (no api_key/model configured) — visual analysis stage will be skipped")
	}
	if cfg.Notion != nil && cfg.Notion.Enabled() {
		red := cfg.Notion.Redacted()
		log.Printf("scribe-web: notion enabled (base_url=%s version=%s token=%s database_id=%s page_id=%s)",
			red.BaseURL, red.NotionVersion, red.Token, red.DatabaseID, red.PageID)
	} else {
		log.Printf("scribe-web: notion disabled (no token or parent configured) — /export/notion will return 503")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	jm.Start(ctx)

	uiFS, ok := uiSubFS(staticFS)
	if !ok {
		log.Printf("scribe-web: no embedded UI found (web_dist/index.html missing); running API-only")
	}

	handler := httpapi.New(cfg, st, jm, mm, sm, qaSvc, notionSvc, uiFS)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("scribe-web: listening on %s", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("scribe-web: shutting down")
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	_ = srv.Shutdown(shutdownCtx)
	jm.Wait()
}

// uiSubFS returns the embedded UI as a file system rooted at index.html.
// Returns ok=false if the embed directory is missing or empty (a fresh
// `go build` before the UI is assembled).
func uiSubFS(root embed.FS) (fs.FS, bool) {
	sub, err := fs.Sub(root, "web_dist")
	if err != nil {
		return nil, false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, false
	}
	return sub, true
}

func logBin(name, path string) {
	if path == "" {
		log.Printf("scribe-web: %s NOT FOUND on PATH (set SCRIBE_%s_BIN to override)", name, envSuffix(name))
		return
	}
	log.Printf("scribe-web: %s -> %s", name, path)
}

func envSuffix(name string) string {
	switch name {
	case "ffmpeg":
		return "FFMPEG"
	case "yt-dlp":
		return "YTDLP"
	case "whisper-cli":
		return "WHISPER"
	}
	return name
}
