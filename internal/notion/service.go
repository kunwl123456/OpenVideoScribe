// Package notion exports job knowledge to real Notion pages.
package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"scribe-web/internal/config"
	"scribe-web/internal/store"
)

var ErrNotConfigured = errors.New("notion: not configured")

const (
	maxChildrenPerRequest = 100
	maxRichTextRunes      = 1900
	maxTranscriptRunes    = 16000
	maxSummaryRunes       = 6000
	maxQAMessages         = 12
)

type Service struct {
	cfg  *config.NotionConfig
	http *http.Client
}

type ExportResult struct {
	PageID     string    `json:"page_id"`
	PageURL    string    `json:"page_url"`
	ExportedAt time.Time `json:"exported_at"`
}

func New(cfg *config.NotionConfig) *Service {
	timeout := 25 * time.Second
	if cfg != nil && cfg.Timeout > 0 {
		timeout = cfg.Timeout
	}
	return &Service{
		cfg:  cfg,
		http: &http.Client{Timeout: timeout},
	}
}

func (s *Service) Enabled() bool {
	return s != nil && s.cfg != nil && s.cfg.Enabled()
}

func (s *Service) ExportJob(ctx context.Context, job *store.Job) (*ExportResult, error) {
	if !s.Enabled() {
		return nil, ErrNotConfigured
	}
	if job == nil {
		return nil, fmt.Errorf("notion: job is nil")
	}
	title := job.URL
	if job.Source != nil && strings.TrimSpace(job.Source.Title) != "" {
		title = strings.TrimSpace(job.Source.Title)
	}
	blocks := buildBlocks(job)
	chunks := chunkBlocks(blocks, maxChildrenPerRequest)

	reqBody := map[string]any{
		"parent":     buildParent(s.cfg),
		"properties": buildProperties(s.cfg, title, job),
	}
	if len(chunks) > 0 {
		reqBody["children"] = chunks[0]
	}
	var created struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := s.doJSON(ctx, http.MethodPost, s.cfg.BaseURL+"/pages", reqBody, &created); err != nil {
		return nil, err
	}
	pageID := strings.TrimSpace(created.ID)
	if pageID == "" {
		return nil, fmt.Errorf("notion: create page missing id")
	}
	for i := 1; i < len(chunks); i++ {
		appendBody := map[string]any{"children": chunks[i]}
		if err := s.doJSON(ctx, http.MethodPatch, s.cfg.BaseURL+"/blocks/"+pageID+"/children", appendBody, nil); err != nil {
			return nil, err
		}
	}
	return &ExportResult{
		PageID:     pageID,
		PageURL:    strings.TrimSpace(created.URL),
		ExportedAt: time.Now().UTC(),
	}, nil
}

func (s *Service) doJSON(ctx context.Context, method, url string, in any, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("notion: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("notion: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	req.Header.Set("Notion-Version", s.cfg.NotionVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("notion: http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("notion: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("notion: decode response: %w", err)
	}
	return nil
}

func buildParent(cfg *config.NotionConfig) map[string]any {
	if strings.TrimSpace(cfg.DatabaseID) != "" {
		return map[string]any{"database_id": strings.TrimSpace(cfg.DatabaseID)}
	}
	return map[string]any{"page_id": strings.TrimSpace(cfg.PageID)}
}

func buildProperties(cfg *config.NotionConfig, title string, job *store.Job) map[string]any {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Scribe 导出"
	}
	text := richTextObject(title)
	props := map[string]any{}
	if strings.TrimSpace(cfg.DatabaseID) != "" {
		props[cfg.TitleProperty] = map[string]any{"title": []map[string]any{text}}
		if strings.TrimSpace(cfg.URLProperty) != "" && strings.TrimSpace(job.URL) != "" {
			props[cfg.URLProperty] = map[string]any{"url": strings.TrimSpace(job.URL)}
		}
		return props
	}
	props["title"] = map[string]any{"title": []map[string]any{text}}
	return props
}

func buildBlocks(job *store.Job) []map[string]any {
	var out []map[string]any
	title := strings.TrimSpace(job.URL)
	if job.Source != nil && strings.TrimSpace(job.Source.Title) != "" {
		title = strings.TrimSpace(job.Source.Title)
	}
	out = append(out, headingBlock(1, title))
	meta := []string{
		fmt.Sprintf("任务 ID：%s", job.ID),
		fmt.Sprintf("来源 URL：%s", job.URL),
	}
	if job.Source != nil && strings.TrimSpace(job.Source.Uploader) != "" {
		meta = append(meta, "上传者："+strings.TrimSpace(job.Source.Uploader))
	}
	if job.Transcript != nil && strings.TrimSpace(job.Transcript.Language) != "" {
		meta = append(meta, "语言："+strings.TrimSpace(job.Transcript.Language))
	}
	out = append(out, bulletedBlocks(meta)...)

	if len(job.Chapters) > 0 {
		out = append(out, headingBlock(2, "章节时间轴"))
		for i, ch := range job.Chapters {
			out = append(out, headingBlock(3, fmt.Sprintf("%d. %s (%s - %s)", i+1, ch.Title, ts(ch.StartSec), ts(ch.EndSec))))
			out = append(out, bulletedBlocks(ch.Bullets)...)
			for _, q := range ch.KeyQuotes {
				txt := fmt.Sprintf("[%s-%s] %s", ts(q.StartSec), ts(q.EndSec), strings.TrimSpace(q.Text))
				out = append(out, quoteBlock(txt))
			}
		}
	}

	if sess := latestQASession(job.QASessions); sess != nil && len(sess.Messages) > 0 {
		out = append(out, headingBlock(2, "问答会话（最近轮次）"))
		msgs := sess.Messages
		if len(msgs) > maxQAMessages {
			msgs = msgs[len(msgs)-maxQAMessages:]
		}
		for _, msg := range msgs {
			role := "用户"
			if strings.TrimSpace(msg.Role) == "assistant" {
				role = "助手"
			}
			out = append(out, paragraphBlocks(fmt.Sprintf("%s：%s", role, strings.TrimSpace(msg.Content)))...)
			for _, c := range msg.Citations {
				out = append(out, bulletedBlocks([]string{
					fmt.Sprintf("证据 [%s-%s] %s", ts(c.Start), ts(c.End), strings.TrimSpace(c.Text)),
				})...)
			}
		}
	}

	if len(job.Summaries) > 0 {
		out = append(out, headingBlock(2, "摘要产物"))
		order := []string{
			"brief", "detailed", "outline", "mindmap",
			"study_notes", "wechat_article", "course_handout", "short_video_script", "quote_cards",
		}
		for _, kind := range order {
			entry := job.Summaries[kind]
			if entry == nil || entry.Status != store.SummaryDone || strings.TrimSpace(entry.Markdown) == "" {
				continue
			}
			out = append(out, headingBlock(3, summaryKindLabel(kind)))
			out = append(out, markdownLikeBlocks(truncateRunes(entry.Markdown, maxSummaryRunes))...)
		}
	}

	if job.Transcript != nil && strings.TrimSpace(job.Transcript.FullText) != "" {
		out = append(out, headingBlock(2, "Transcript（截断）"))
		out = append(out, paragraphBlocks(truncateRunes(job.Transcript.FullText, maxTranscriptRunes))...)
	}
	return out
}

func latestQASession(sessions []store.QASession) *store.QASession {
	if len(sessions) == 0 {
		return nil
	}
	cp := append([]store.QASession(nil), sessions...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].UpdatedAt.After(cp[j].UpdatedAt) })
	return &cp[0]
}

func summaryKindLabel(kind string) string {
	switch kind {
	case "brief":
		return "AI 总结"
	case "detailed":
		return "详细摘要"
	case "outline":
		return "大纲"
	case "mindmap":
		return "思维导图"
	case "study_notes":
		return "学习笔记"
	case "wechat_article":
		return "公众号文案"
	case "course_handout":
		return "课程讲义"
	case "short_video_script":
		return "短视频脚本"
	case "quote_cards":
		return "金句卡片"
	default:
		return kind
	}
}

func markdownLikeBlocks(md string) []map[string]any {
	lines := strings.Split(md, "\n")
	var out []map[string]any
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "- ") {
			out = append(out, bulletedBlocks([]string{strings.TrimSpace(strings.TrimPrefix(t, "- "))})...)
			continue
		}
		out = append(out, paragraphBlocks(t)...)
	}
	return out
}

func headingBlock(level int, text string) map[string]any {
	kind := "heading_2"
	switch level {
	case 1:
		kind = "heading_1"
	case 2:
		kind = "heading_2"
	default:
		kind = "heading_3"
	}
	return map[string]any{
		"type": kind,
		kind: map[string]any{
			"rich_text": []map[string]any{richTextObject(truncateRunes(strings.TrimSpace(text), maxRichTextRunes))},
		},
	}
}

func quoteBlock(text string) map[string]any {
	return map[string]any{
		"type": "quote",
		"quote": map[string]any{
			"rich_text": []map[string]any{richTextObject(truncateRunes(strings.TrimSpace(text), maxRichTextRunes))},
		},
	}
}

func bulletedBlocks(items []string) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		line := strings.TrimSpace(it)
		if line == "" {
			continue
		}
		parts := splitRunes(line, maxRichTextRunes)
		for _, p := range parts {
			out = append(out, map[string]any{
				"type": "bulleted_list_item",
				"bulleted_list_item": map[string]any{
					"rich_text": []map[string]any{richTextObject(p)},
				},
			})
		}
	}
	return out
}

func paragraphBlocks(text string) []map[string]any {
	parts := splitRunes(strings.TrimSpace(text), maxRichTextRunes)
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		out = append(out, map[string]any{
			"type": "paragraph",
			"paragraph": map[string]any{
				"rich_text": []map[string]any{richTextObject(p)},
			},
		})
	}
	return out
}

func richTextObject(text string) map[string]any {
	return map[string]any{
		"type": "text",
		"text": map[string]any{
			"content": text,
		},
	}
}

func chunkBlocks(blocks []map[string]any, size int) [][]map[string]any {
	if size <= 0 || len(blocks) == 0 {
		return nil
	}
	var out [][]map[string]any
	for i := 0; i < len(blocks); i += size {
		end := i + size
		if end > len(blocks) {
			end = len(blocks)
		}
		out = append(out, blocks[i:end])
	}
	return out
}

func splitRunes(s string, size int) []string {
	s = strings.TrimSpace(s)
	if s == "" || size <= 0 {
		return nil
	}
	r := []rune(s)
	if len(r) <= size {
		return []string{s}
	}
	out := make([]string, 0, (len(r)+size-1)/size)
	for i := 0; i < len(r); i += size {
		end := i + size
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func ts(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	h := int(sec) / 3600
	m := (int(sec) % 3600) / 60
	s := int(sec) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}
