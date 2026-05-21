// Package summary turns a finished transcript into structured artefacts
// (brief / detailed / outline / mindmap / study_notes / ...) by
// prompting an OpenAI-compatible LLM provider. Stateless; persistence
// is the caller's job (see internal/store SetSummary).
package summary

import (
	"context"
	"fmt"
	"strings"
	"time"

	"scribe-web/internal/asr"
	"scribe-web/internal/config"
	"scribe-web/internal/llm"
	"scribe-web/internal/vision"
)

// Kind picks the prompt template. The wire value is exactly the string
// constant — the HTTP layer reads ?kind=outline and feeds it through
// ParseKind, so renaming a Kind here is a breaking API change.
type Kind string

const (
	KindBrief            Kind = "brief"
	KindDetailed         Kind = "detailed"
	KindOutline          Kind = "outline"
	KindMindmap          Kind = "mindmap"
	KindStudyNotes       Kind = "study_notes"
	KindWechatArticle    Kind = "wechat_article"
	KindCourseHandout    Kind = "course_handout"
	KindShortVideoScript Kind = "short_video_script"
	KindQuoteCards       Kind = "quote_cards"
)

// AllKinds is the canonical iteration order — used by tests and any
// future "generate everything at once" endpoint.
var AllKinds = []Kind{
	KindBrief,
	KindDetailed,
	KindOutline,
	KindMindmap,
	KindStudyNotes,
	KindWechatArticle,
	KindCourseHandout,
	KindShortVideoScript,
	KindQuoteCards,
}

// ParseKind validates the wire string. Returns an error for unknown
// values so the HTTP layer can answer 400 with a usable message.
func ParseKind(s string) (Kind, error) {
	k := Kind(strings.ToLower(strings.TrimSpace(s)))
	for _, ok := range AllKinds {
		if k == ok {
			return k, nil
		}
	}
	return "", fmt.Errorf("unknown summary kind %q", s)
}

// Result is what Generate returns and what the API persists.
type Result struct {
	Kind              Kind      `json:"kind"`
	Markdown          string    `json:"markdown"`
	Model             string    `json:"model"`
	TokensUsed        int       `json:"tokens_used"`
	PromptTokens      int       `json:"prompt_tokens,omitempty"`
	CompletionTokens  int       `json:"completion_tokens,omitempty"`
	EstimatedCost     float64   `json:"estimated_cost,omitempty"`
	EstimatedCostText string    `json:"estimated_cost_text,omitempty"`
	DurationMs        int64     `json:"duration_ms"`
	GeneratedAt       time.Time `json:"generated_at"`
}

// MaxTranscriptChars caps how much transcript text we feed into a
// single prompt. Doubao / DeepSeek default contexts comfortably hold
// 12k Chinese characters of body plus our ~400-char system+wrapping.
// If a video is longer we keep the head and tail and elide the middle
// — this preserves opener/closer cues that summaries rely on most.
const MaxTranscriptChars = 12000

// Service wires a Provider to the prompt templates. Construct one at
// boot and reuse it for the process lifetime.
type Service struct {
	prov llm.Provider
	cfg  *config.LLMConfig // optional; only used for cost estimation
}

// New returns a Service backed by the given Provider. nil Provider is
// allowed for tests where Generate isn't called. The optional cfg
// drives EstimateCost; pass nil to disable cost estimation entirely.
func New(p llm.Provider, cfg *config.LLMConfig) *Service {
	return &Service{prov: p, cfg: cfg}
}

// Generate prompts the LLM with the chosen Kind's template and returns
// a Result. The caller persists it via store.SetSummary.
func (s *Service) Generate(ctx context.Context, t *asr.Result, kind Kind, meta Metadata) (*Result, error) {
	if s == nil || s.prov == nil {
		return nil, llm.ErrNoAPIKey
	}
	if t == nil || strings.TrimSpace(t.FullText) == "" {
		return nil, fmt.Errorf("summary: transcript is empty")
	}
	prepared := prepareText(t.FullText, MaxTranscriptChars)
	vars := PromptVars{
		Title:           strings.TrimSpace(meta.Title),
		Uploader:        strings.TrimSpace(meta.Uploader),
		DurationSeconds: int(t.Duration),
		FullText:        prepared,
		VisualInsights:  renderVisualInsights(meta.Frames, MaxVisualInsightsChars),
	}
	if vars.DurationSeconds == 0 && meta.DurationSeconds > 0 {
		vars.DurationSeconds = meta.DurationSeconds
	}
	userPrompt, err := renderPrompt(kind, vars)
	if err != nil {
		return nil, fmt.Errorf("summary: render prompt: %w", err)
	}

	start := time.Now()
	resp, err := s.prov.Chat(ctx, llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("summary: empty completion")
	}
	md := strings.TrimSpace(resp.Choices[0].Message.Content)
	md = stripCodeFence(md)
	cost, costText := EstimateCost(resp.Usage.PromptTokens, resp.Usage.CompletionTokens, s.cfg)
	return &Result{
		Kind:              kind,
		Markdown:          md,
		Model:             resp.Model,
		TokensUsed:        resp.Usage.TotalTokens,
		PromptTokens:      resp.Usage.PromptTokens,
		CompletionTokens:  resp.Usage.CompletionTokens,
		EstimatedCost:     cost,
		EstimatedCostText: costText,
		DurationMs:        time.Since(start).Milliseconds(),
		GeneratedAt:       time.Now().UTC(),
	}, nil
}

// Metadata is the video-level info passed alongside the transcript.
// We keep it separate from asr.Result so the summary package doesn't
// import ytdlp — the HTTP layer fills these from job.Source.
//
// Frames is the optional per-timestamp visual-analysis output. Pass an
// empty slice (or nil) to omit the visual block from the LLM prompt;
// the templates conditionally include it via `{{- if .VisualInsights}}`.
type Metadata struct {
	Title           string
	Uploader        string
	DurationSeconds int
	Frames          []vision.Insight
}

// MaxVisualInsightsChars caps how much visual text we feed the LLM in
// the dedicated VisualInsights block. Mirrors MaxTranscriptChars but
// keeps a smaller budget — vision lines are short and noisy lines
// (failed frames) shouldn't dominate the prompt.
const MaxVisualInsightsChars = 4000

// renderVisualInsights turns a list of Insights into a per-line block
// like:
//
//	[00:15] 画面：开场幻灯片，写着「大模型 101」  文字：大模型 101
//	[01:02] 画面：演讲者在白板前  文字：
//
// Frames with empty Caption (and no OCR) or with a populated Error are
// silently dropped — they would only confuse the LLM. When the rendered
// block exceeds the cap we keep head + tail, mirroring prepareText.
func renderVisualInsights(frames []vision.Insight, max int) string {
	if len(frames) == 0 {
		return ""
	}
	var b strings.Builder
	for _, f := range frames {
		caption := strings.TrimSpace(f.Caption)
		ocr := strings.TrimSpace(f.OCRText)
		if caption == "" && ocr == "" {
			continue
		}
		if strings.HasPrefix(caption, "(画面描述失败)") {
			continue
		}
		ts := vision.FormatTimestamp(f.TimestampSec)
		b.WriteString(fmt.Sprintf("[%s] 画面：%s", ts, caption))
		if ocr != "" {
			b.WriteString("  文字：")
			b.WriteString(ocr)
		}
		b.WriteByte('\n')
	}
	rendered := strings.TrimRight(b.String(), "\n")
	if max <= 0 {
		return rendered
	}
	runes := []rune(rendered)
	if len(runes) <= max {
		return rendered
	}
	headLen := max * 6 / 10
	tailLen := max - headLen - 6
	if tailLen < 0 {
		tailLen = 0
	}
	return string(runes[:headLen]) + "\n……（中段省略）……\n" + string(runes[len(runes)-tailLen:])
}

// prepareText trims whitespace and (when needed) folds an over-long
// transcript by keeping ~60% head and ~40% tail, joined by an ellipsis
// marker. Visible to tests via the unexported name — exported through
// a thin wrapper if a downstream ever needs it.
func prepareText(s string, max int) string {
	clean := normaliseWhitespace(s)
	runes := []rune(clean)
	if len(runes) <= max {
		return clean
	}
	headLen := max * 6 / 10
	tailLen := max - headLen - 6 // 6 = length of separator marker
	if tailLen < 0 {
		tailLen = 0
	}
	head := string(runes[:headLen])
	tail := string(runes[len(runes)-tailLen:])
	return head + "\n……（中段省略）……\n" + tail
}

// normaliseWhitespace collapses runs of whitespace; whisper emits raw
// "\n" between every chunk which wastes tokens.
func normaliseWhitespace(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	// Replace any whitespace run with a single space, except keep
	// paragraph breaks (double newline) as one newline. The model
	// doesn't need fine-grained spacing.
	var b strings.Builder
	b.Grow(len(s))
	var prevSpace bool
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}

// stripCodeFence removes a wrapping ```...``` if the model decided to
// quote its markdown despite our instructions. We keep nested fences
// (inside the body) intact.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s
	}
	rest := s[nl+1:]
	if i := strings.LastIndex(rest, "```"); i >= 0 {
		return strings.TrimSpace(rest[:i])
	}
	return strings.TrimSpace(rest)
}
