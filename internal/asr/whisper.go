// Package asr drives whisper.cpp's CLI. Output is a Result with
// per-segment timings + the joined text.
package asr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"scribe-web/internal/config"
)

// Segment is one timestamped chunk of speech.
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// Result is what one transcription produces.
type Result struct {
	Language string    `json:"language"`
	Model    string    `json:"model"`
	Duration float64   `json:"duration"`
	Segments []Segment `json:"segments"`
	FullText string    `json:"full_text"`
}

// Request is what callers fill in.
type Request struct {
	AudioPath  string
	Model      string
	Language   string
	OnProgress func(message string)
}

// progressRe extracts whisper-cli's `[mm:ss.ms --> mm:ss.ms]` segment
// markers. We use the latest end timestamp as a coarse progress signal
// (whisper-cli doesn't emit a real percentage).
var progressRe = regexp.MustCompile(`\[\s*(\d+):(\d+\.\d+)\s*-->\s*(\d+):(\d+\.\d+)`)

// Transcribe shells out to whisper-cli. The model file must exist on
// disk under cfg.ModelsDir.
func Transcribe(ctx context.Context, cfg *config.Config, req Request) (*Result, error) {
	if err := config.RequireBin(cfg.WhisperBin, "whisper-cli"); err != nil {
		return nil, err
	}

	model := req.Model
	if model == "" {
		model = "base"
	}
	modelPath := cfg.ModelFilePath(model)
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("whisper model %s missing at %s", model, modelPath)
	}
	lang := req.Language
	if lang == "" {
		lang = "auto"
	}

	outPrefix := strings.TrimSuffix(req.AudioPath, filepath.Ext(req.AudioPath))
	args := []string{
		"-m", modelPath,
		"-f", req.AudioPath,
		"-l", lang,
		"-oj",
		"-of", outPrefix,
		"-nt",
	}
	cmd := exec.CommandContext(ctx, cfg.WhisperBin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go watchProgress(stderr, req.OnProgress)
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("whisper-cli failed: %w", err)
	}

	jsonPath := outPrefix + ".json"
	defer os.Remove(jsonPath)
	res, err := parseJSON(jsonPath, model)
	if err != nil {
		return nil, err
	}
	if req.OnProgress != nil {
		req.OnProgress("done")
	}
	return res, nil
}

func watchProgress(r io.Reader, cb func(string)) {
	if cb == nil {
		_, _ = io.Copy(io.Discard, r)
		return
	}
	sc := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1<<20)
	for sc.Scan() {
		m := progressRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		mins, _ := strconv.ParseFloat(m[3], 64)
		secs, _ := strconv.ParseFloat(m[4], 64)
		cb(fmt.Sprintf("至 %.1fs", mins*60+secs))
	}
}

type rawJSON struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Transcription []struct {
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		Text string `json:"text"`
	} `json:"transcription"`
}

func parseJSON(path, model string) (*Result, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read whisper json: %w", err)
	}
	var data rawJSON
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse whisper json: %w", err)
	}
	lang := data.Result.Language
	normalize := func(s string) string { return s }
	if lang == "zh" {
		normalize = ToSimplified
		lang = "zh-Hans"
	}

	segs := make([]Segment, 0, len(data.Transcription))
	var full strings.Builder
	var maxEnd float64
	for _, t := range data.Transcription {
		start := float64(t.Offsets.From) / 1000
		end := float64(t.Offsets.To) / 1000
		if end > maxEnd {
			maxEnd = end
		}
		text := normalize(strings.TrimSpace(t.Text))
		segs = append(segs, Segment{Start: start, End: end, Text: text})
		if full.Len() > 0 {
			full.WriteByte('\n')
		}
		full.WriteString(text)
	}
	return &Result{
		Language: lang,
		Model:    "whisper-cpp:" + model,
		Duration: maxEnd,
		Segments: segs,
		FullText: full.String(),
	}, nil
}
