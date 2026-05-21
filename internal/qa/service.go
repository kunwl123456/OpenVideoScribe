// Package qa provides lightweight transcript RAG QA + chapter generation.
package qa

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"scribe-web/internal/asr"
	"scribe-web/internal/config"
	"scribe-web/internal/llm"
	"scribe-web/internal/store"
)

const (
	defaultTopK                = 5
	maxQAPromptContextRunes    = 9000
	maxChapterPromptRunes      = 10000
	chapterBulletPadText       = "要点待补充"
	chapterMinBulletsRequired  = 3
	chapterMaxBulletsAllowed   = 5
	chapterMaxQuotesAllowed    = 3
	maxHistoryTurnsForPrompt   = 8
	evidenceScoreStrictMinimum = 2.2
)

type Service struct {
	prov llm.Provider
	cfg  *config.LLMConfig
}

type Citation struct {
	JobID    string  `json:"job_id,omitempty"`
	JobTitle string  `json:"job_title,omitempty"`
	Text     string  `json:"text"`
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	Score    float64 `json:"score,omitempty"`
}

type ChatMessage struct {
	Role    string
	Content string
}

type AnswerResult struct {
	Answer        string     `json:"answer"`
	Citations     []Citation `json:"citations"`
	Model         string     `json:"model,omitempty"`
	EvidenceFound bool       `json:"evidence_found"`
}

type ChapterResult struct {
	Chapters    []store.Chapter `json:"chapters"`
	Model       string          `json:"model,omitempty"`
	GeneratedAt time.Time       `json:"generated_at"`
}

type RetrievalHit struct {
	Index   int
	Segment asr.Segment
	Score   float64
}

type GlobalDocument struct {
	JobID    string
	JobTitle string
	Segments []asr.Segment
}

func New(p llm.Provider, cfg *config.LLMConfig) *Service {
	return &Service{prov: p, cfg: cfg}
}

func (s *Service) Answer(ctx context.Context, transcript *asr.Result, question string, topK int) (*AnswerResult, error) {
	return s.AnswerWithContext(ctx, transcript, question, nil, topK)
}

func (s *Service) AnswerWithContext(ctx context.Context, transcript *asr.Result, question string, history []ChatMessage, topK int) (*AnswerResult, error) {
	if s == nil || s.prov == nil {
		return nil, llm.ErrNoAPIKey
	}
	q := strings.TrimSpace(question)
	if q == "" {
		return nil, fmt.Errorf("qa: question is empty")
	}
	if transcript == nil || len(transcript.Segments) == 0 {
		return nil, fmt.Errorf("qa: transcript segments are empty")
	}

	hits := RetrieveTopSegments(q, transcript.Segments, topK)
	if len(hits) == 0 {
		return nil, fmt.Errorf("qa: no retrievable segments")
	}
	citations := citationsFromHits(hits, "", "")
	// Enforce evidence grounding: if confidence is too low, do not ask LLM
	// to "guess"; return a clear fallback answer with nearest evidence.
	if hits[0].Score < evidenceScoreStrictMinimum {
		return &AnswerResult{
			Answer:        "当前问题在视频中无法定位到明确证据片段，请换个更具体的问法。",
			Citations:     citations[:1],
			EvidenceFound: false,
		}, nil
	}

	var contextBuilder strings.Builder
	for i, hit := range hits {
		contextBuilder.WriteString(fmt.Sprintf("[%d] %s-%s %s\n",
			i+1,
			formatTimestamp(hit.Segment.Start),
			formatTimestamp(hit.Segment.End),
			strings.TrimSpace(hit.Segment.Text),
		))
	}
	historyBlock := renderHistory(history, maxHistoryTurnsForPrompt)
	userPrompt := fmt.Sprintf(`用户问题：
%s

%s
可用片段（按相关度排序）：
%s

输出要求：
1) 用简洁中文回答；
2) 关键结论后标注证据编号，如 [1][2]；
3) 只能基于给定片段，不得编造。`,
		q,
		historyBlock,
		strings.TrimSpace(truncateRunes(contextBuilder.String(), maxQAPromptContextRunes)),
	)

	resp, err := s.prov.Chat(ctx, llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "你是视频问答助手，只能根据给定片段回答，不允许编造事实。"},
			{Role: "user", Content: userPrompt},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("qa: empty completion")
	}
	return &AnswerResult{
		Answer:        strings.TrimSpace(stripCodeFence(resp.Choices[0].Message.Content)),
		Citations:     citations,
		Model:         resp.Model,
		EvidenceFound: true,
	}, nil
}

func (s *Service) AnswerAcrossJobs(ctx context.Context, question string, docs []GlobalDocument, topK int) (*AnswerResult, error) {
	if s == nil || s.prov == nil {
		return nil, llm.ErrNoAPIKey
	}
	q := strings.TrimSpace(question)
	if q == "" {
		return nil, fmt.Errorf("qa: question is empty")
	}
	hits := retrieveAcrossDocs(q, docs, topK)
	if len(hits) == 0 {
		return nil, fmt.Errorf("qa: no retrievable segments")
	}
	if hits[0].Score < evidenceScoreStrictMinimum {
		c := hitToCitation(hits[0])
		return &AnswerResult{
			Answer:        "当前问题在现有视频集合中无法定位到明确证据片段，请补充关键词后再试。",
			Citations:     []Citation{c},
			EvidenceFound: false,
		}, nil
	}

	var contextBuilder strings.Builder
	for i, hit := range hits {
		contextBuilder.WriteString(fmt.Sprintf("[%d] [%s] %s-%s %s\n",
			i+1,
			hit.DocTitle,
			formatTimestamp(hit.Segment.Start),
			formatTimestamp(hit.Segment.End),
			strings.TrimSpace(hit.Segment.Text),
		))
	}
	userPrompt := fmt.Sprintf(`用户问题：
%s

请基于以下“跨视频”证据片段回答，并明确综合结论。

证据片段（按相关度排序）：
%s

输出要求：
1) 用简洁中文回答；
2) 关键结论标注证据编号，如 [1][3]；
3) 不得编造不存在的事实。`,
		q,
		strings.TrimSpace(truncateRunes(contextBuilder.String(), maxQAPromptContextRunes)),
	)
	resp, err := s.prov.Chat(ctx, llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "你是跨视频知识库问答助手，只能依据证据片段回答。"},
			{Role: "user", Content: userPrompt},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("qa: empty completion")
	}
	citations := make([]Citation, 0, len(hits))
	for _, hit := range hits {
		citations = append(citations, hitToCitation(hit))
	}
	return &AnswerResult{
		Answer:        strings.TrimSpace(stripCodeFence(resp.Choices[0].Message.Content)),
		Citations:     citations,
		Model:         resp.Model,
		EvidenceFound: true,
	}, nil
}

func (s *Service) GenerateChapters(ctx context.Context, transcript *asr.Result, maxChapters int) (*ChapterResult, error) {
	if s == nil || s.prov == nil {
		return nil, llm.ErrNoAPIKey
	}
	if transcript == nil || len(transcript.Segments) == 0 {
		return nil, fmt.Errorf("chapters: transcript segments are empty")
	}
	segmentsText := renderSegmentsForPrompt(transcript.Segments, maxChapterPromptRunes)
	if segmentsText == "" {
		return nil, fmt.Errorf("chapters: transcript segments are empty")
	}

	userPrompt := fmt.Sprintf(`请把下面视频转写分成若干章节，并严格输出 JSON（不要 markdown 代码块）。

JSON 结构：
{
  "chapters": [
    {
      "title": "章节标题",
      "start_sec": 0,
      "end_sec": 120,
      "bullets": ["要点1", "要点2", "要点3"],
      "key_quotes": [
        {"text":"关键句", "start_sec": 12, "end_sec": 18}
      ]
    }
  ]
}

约束：
- 章节按时间升序；
- 每章 bullets 3-5 条；
- 每章 key_quotes 至少 1 条；
- title 要简洁；
- 时间单位是秒。

转写片段：
%s`, segmentsText)

	resp, err := s.prov.Chat(ctx, llm.ChatRequest{
		Messages: []llm.ChatMessage{
			{Role: "system", Content: "你是视频结构化助手，擅长把转写整理成章节化时间轴。"},
			{Role: "user", Content: userPrompt},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("chapters: empty completion")
	}

	chapters, err := ParseChapters(strings.TrimSpace(resp.Choices[0].Message.Content), transcript.Duration, transcript.Segments)
	if err != nil {
		return nil, err
	}
	if maxChapters > 0 && len(chapters) > maxChapters {
		chapters = chapters[:maxChapters]
	}
	return &ChapterResult{
		Chapters:    chapters,
		Model:       resp.Model,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

func RetrieveTopSegments(question string, segments []asr.Segment, topK int) []RetrievalHit {
	if len(segments) == 0 {
		return nil
	}
	if topK <= 0 {
		topK = defaultTopK
	}

	qNorm := normaliseForMatch(question)
	keywords := extractKeywords(qNorm)
	qRunes := runeSet(strings.ReplaceAll(qNorm, " ", ""))

	hits := make([]RetrievalHit, 0, len(segments))
	for idx, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		segNorm := normaliseForMatch(text)
		if segNorm == "" {
			continue
		}
		score := scoreSegment(qNorm, keywords, qRunes, segNorm)
		if score <= 0 {
			continue
		}
		hits = append(hits, RetrievalHit{Index: idx, Segment: seg, Score: score})
	}
	if len(hits) == 0 {
		for idx, seg := range segments {
			if strings.TrimSpace(seg.Text) == "" {
				continue
			}
			hits = append(hits, RetrievalHit{Index: idx, Segment: seg, Score: 0.1})
			if len(hits) >= topK {
				break
			}
		}
	}
	if len(hits) < topK {
		picked := make(map[int]struct{}, len(hits))
		for _, hit := range hits {
			picked[hit.Index] = struct{}{}
		}
		for idx, seg := range segments {
			if len(hits) >= topK {
				break
			}
			if _, ok := picked[idx]; ok {
				continue
			}
			if strings.TrimSpace(seg.Text) == "" {
				continue
			}
			hits = append(hits, RetrievalHit{Index: idx, Segment: seg, Score: 0.05})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			if hits[i].Segment.Start == hits[j].Segment.Start {
				return hits[i].Index < hits[j].Index
			}
			return hits[i].Segment.Start < hits[j].Segment.Start
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

type globalHit struct {
	DocID    string
	DocTitle string
	RetrievalHit
}

func retrieveAcrossDocs(question string, docs []GlobalDocument, topK int) []globalHit {
	if topK <= 0 {
		topK = defaultTopK
	}
	var all []globalHit
	for _, doc := range docs {
		if doc.JobID == "" || len(doc.Segments) == 0 {
			continue
		}
		perDoc := RetrieveTopSegments(question, doc.Segments, minInt(2, topK))
		for _, hit := range perDoc {
			all = append(all, globalHit{
				DocID:        doc.JobID,
				DocTitle:     strings.TrimSpace(doc.JobTitle),
				RetrievalHit: hit,
			})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score == all[j].Score {
			if all[i].DocID == all[j].DocID {
				return all[i].Segment.Start < all[j].Segment.Start
			}
			return all[i].DocID < all[j].DocID
		}
		return all[i].Score > all[j].Score
	})
	if len(all) > topK {
		all = all[:topK]
	}
	return all
}

func hitToCitation(hit globalHit) Citation {
	return Citation{
		JobID:    hit.DocID,
		JobTitle: hit.DocTitle,
		Text:     strings.TrimSpace(hit.Segment.Text),
		Start:    hit.Segment.Start,
		End:      hit.Segment.End,
		Score:    hit.Score,
	}
}

func citationsFromHits(hits []RetrievalHit, jobID, title string) []Citation {
	out := make([]Citation, 0, len(hits))
	for _, hit := range hits {
		out = append(out, Citation{
			JobID:    jobID,
			JobTitle: title,
			Text:     strings.TrimSpace(hit.Segment.Text),
			Start:    hit.Segment.Start,
			End:      hit.Segment.End,
			Score:    hit.Score,
		})
	}
	return out
}

func ParseChapters(raw string, duration float64, segments []asr.Segment) ([]store.Chapter, error) {
	body := stripCodeFence(strings.TrimSpace(raw))
	payload := extractJSONObject(body)

	var wrapper struct {
		Chapters []store.Chapter `json:"chapters"`
	}
	if err := json.Unmarshal([]byte(payload), &wrapper); err != nil || len(wrapper.Chapters) == 0 {
		var plain []store.Chapter
		if err2 := json.Unmarshal([]byte(payload), &plain); err2 != nil || len(plain) == 0 {
			if err != nil {
				return nil, fmt.Errorf("chapters: decode json: %w", err)
			}
			return nil, fmt.Errorf("chapters: empty chapters")
		}
		wrapper.Chapters = plain
	}

	out := make([]store.Chapter, 0, len(wrapper.Chapters))
	for i, ch := range wrapper.Chapters {
		title := strings.TrimSpace(ch.Title)
		if title == "" {
			title = fmt.Sprintf("第 %d 章", i+1)
		}
		start := clampSec(ch.StartSec, 0, duration)
		end := clampSec(ch.EndSec, 0, duration)
		if end <= start {
			end = start + 1
			if duration > 0 && end > duration {
				end = duration
			}
		}
		bullets := normaliseBullets(ch.Bullets)
		quotes := normaliseKeyQuotes(ch.KeyQuotes, segments, start, end)
		out = append(out, store.Chapter{
			Title:     title,
			StartSec:  start,
			EndSec:    end,
			Bullets:   bullets,
			KeyQuotes: quotes,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("chapters: empty chapters")
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].StartSec < out[j].StartSec })
	for i := range out {
		if i+1 < len(out) {
			nextStart := out[i+1].StartSec
			if nextStart > out[i].StartSec && out[i].EndSec > nextStart {
				out[i].EndSec = nextStart
			}
		}
		if out[i].EndSec <= out[i].StartSec {
			if i+1 < len(out) && out[i+1].StartSec > out[i].StartSec {
				out[i].EndSec = out[i+1].StartSec
			} else if duration > out[i].StartSec {
				out[i].EndSec = duration
			} else {
				out[i].EndSec = out[i].StartSec + 1
			}
		}
		if len(out[i].KeyQuotes) == 0 {
			out[i].KeyQuotes = normaliseKeyQuotes(nil, segments, out[i].StartSec, out[i].EndSec)
		}
	}
	return out, nil
}

func scoreSegment(question string, keywords []string, qRunes map[rune]struct{}, seg string) float64 {
	var score float64
	if question != "" && strings.Contains(seg, question) {
		score += 30
	}
	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		if strings.Contains(seg, kw) {
			score += 9 + float64(len([]rune(kw)))*0.7
		}
	}
	if len(qRunes) > 0 {
		segSet := runeSet(strings.ReplaceAll(seg, " ", ""))
		var overlap int
		for r := range qRunes {
			if _, ok := segSet[r]; ok {
				overlap++
			}
		}
		score += float64(overlap) / float64(len(qRunes)) * 8
	}
	return score
}

func normaliseBullets(in []string) []string {
	out := make([]string, 0, len(in))
	for _, b := range in {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		out = append(out, b)
		if len(out) >= chapterMaxBulletsAllowed {
			break
		}
	}
	for len(out) < chapterMinBulletsRequired {
		out = append(out, chapterBulletPadText)
	}
	return out
}

func normaliseKeyQuotes(in []store.ChapterQuote, segs []asr.Segment, start, end float64) []store.ChapterQuote {
	out := make([]store.ChapterQuote, 0, len(in))
	for _, q := range in {
		text := strings.TrimSpace(q.Text)
		if text == "" {
			continue
		}
		qs := clampSec(q.StartSec, start, end)
		qe := clampSec(q.EndSec, start, end)
		if qe <= qs {
			qe = minFloat(end, qs+1)
		}
		out = append(out, store.ChapterQuote{
			Text:     text,
			StartSec: qs,
			EndSec:   qe,
		})
		if len(out) >= chapterMaxQuotesAllowed {
			break
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, seg := range segs {
		if strings.TrimSpace(seg.Text) == "" {
			continue
		}
		if seg.End < start || seg.Start > end {
			continue
		}
		txt := strings.TrimSpace(seg.Text)
		txt = truncateRunes(txt, 90)
		out = append(out, store.ChapterQuote{
			Text:     txt,
			StartSec: clampSec(seg.Start, start, end),
			EndSec:   clampSec(seg.End, start, end),
		})
		if len(out) >= 1 {
			break
		}
	}
	if len(out) == 0 {
		out = append(out, store.ChapterQuote{
			Text:     "本章节关键句待补充",
			StartSec: start,
			EndSec:   minFloat(end, start+1),
		})
	}
	return out
}

func renderSegmentsForPrompt(segments []asr.Segment, maxRunes int) string {
	var b strings.Builder
	for i, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("[%d] %s-%s %s\n",
			i+1,
			formatTimestamp(seg.Start),
			formatTimestamp(seg.End),
			text,
		))
	}
	return strings.TrimSpace(truncateRunes(b.String(), maxRunes))
}

func renderHistory(history []ChatMessage, limit int) string {
	if len(history) == 0 || limit <= 0 {
		return ""
	}
	start := 0
	if len(history) > limit {
		start = len(history) - limit
	}
	var b strings.Builder
	b.WriteString("历史对话（供追问上下文参考）：\n")
	for _, h := range history[start:] {
		role := strings.TrimSpace(h.Role)
		if role == "" {
			continue
		}
		content := strings.TrimSpace(h.Content)
		if content == "" {
			continue
		}
		if role == "assistant" {
			b.WriteString("助手：")
		} else {
			b.WriteString("用户：")
		}
		b.WriteString(content)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

func extractKeywords(q string) []string {
	fields := strings.Fields(q)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(fields)+6)
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if len([]rune(f)) < 2 {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	// Chinese questions often have no spaces; add coarse n-grams.
	if len(out) <= 1 {
		base := strings.ReplaceAll(q, " ", "")
		runes := []rune(base)
		for i := 0; i+1 < len(runes); i += 2 {
			n := 2
			if i+3 <= len(runes) {
				n = 3
			}
			kw := string(runes[i : i+n])
			if len([]rune(kw)) < 2 {
				continue
			}
			if _, ok := seen[kw]; ok {
				continue
			}
			seen[kw] = struct{}{}
			out = append(out, kw)
			if len(out) >= 8 {
				break
			}
		}
	}
	return out
}

func normaliseForMatch(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		case unicode.IsPunct(r):
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		default:
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

func formatTimestamp(sec float64) string {
	if !isFinitePositive(sec) {
		return "00:00"
	}
	total := int(math.Round(sec))
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

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

func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	start := strings.IndexAny(s, "[{")
	endObj := strings.LastIndexAny(s, "]}")
	if start >= 0 && endObj > start {
		return strings.TrimSpace(s[start : endObj+1])
	}
	return s
}

func runeSet(s string) map[rune]struct{} {
	out := make(map[rune]struct{}, len(s))
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		out[r] = struct{}{}
	}
	return out
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func clampSec(v, min, max float64) float64 {
	if !isFinitePositive(v) {
		return min
	}
	if v < min {
		return min
	}
	if max > 0 && v > max {
		return max
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func isFinitePositive(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}
