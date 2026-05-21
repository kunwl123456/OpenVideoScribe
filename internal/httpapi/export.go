package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"scribe-web/internal/asr"
	"scribe-web/internal/store"
)

// formatExport renders a transcript in the requested format. Returns
// the bytes + content type.
func formatExport(job *store.Job, format string) ([]byte, string, error) {
	if job == nil || job.Transcript == nil {
		return nil, "", fmt.Errorf("transcript not ready")
	}
	res := job.Transcript
	switch format {
	case "srt":
		return []byte(toSRT(res)), "application/x-subrip; charset=utf-8", nil
	case "md":
		return []byte(toMarkdown(res)), "text/markdown; charset=utf-8", nil
	case "txt":
		return []byte(res.FullText), "text/plain; charset=utf-8", nil
	case "md_bundle":
		return []byte(toMarkdownBundle(job)), "text/markdown; charset=utf-8", nil
	case "obsidian":
		return []byte(toObsidianMarkdown(job)), "text/markdown; charset=utf-8", nil
	case "notion_import":
		return []byte(toNotionImportMarkdown(job)), "text/markdown; charset=utf-8", nil
	case "xmind_outline":
		return []byte(toXmindOutline(job)), "text/markdown; charset=utf-8", nil
	case "xmind_json":
		outline := toXmindJSON(job)
		raw, err := json.MarshalIndent(outline, "", "  ")
		if err != nil {
			return nil, "", err
		}
		return raw, "application/json; charset=utf-8", nil
	default:
		return nil, "", fmt.Errorf("unsupported format: %s", format)
	}
}

func toSRT(res *asr.Result) string {
	var b strings.Builder
	for i, s := range res.Segments {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n",
			i+1,
			srtTimestamp(s.Start),
			srtTimestamp(s.End),
			strings.TrimSpace(s.Text),
		)
	}
	return b.String()
}

func toMarkdown(res *asr.Result) string {
	var b strings.Builder
	b.WriteString("# Transcript\n\n")
	if res.Language != "" {
		fmt.Fprintf(&b, "- Language: %s\n", res.Language)
	}
	if res.Model != "" {
		fmt.Fprintf(&b, "- Model: %s\n", res.Model)
	}
	if res.Duration > 0 {
		fmt.Fprintf(&b, "- Duration: %.1fs\n", res.Duration)
	}
	b.WriteString("\n## Segments\n\n")
	for _, s := range res.Segments {
		fmt.Fprintf(&b, "- `[%s -> %s]` %s\n",
			srtTimestamp(s.Start),
			srtTimestamp(s.End),
			strings.TrimSpace(s.Text),
		)
	}
	return b.String()
}

func toMarkdownBundle(job *store.Job) string {
	var b strings.Builder
	title := job.URL
	if job.Source != nil && strings.TrimSpace(job.Source.Title) != "" {
		title = job.Source.Title
	}
	b.WriteString("# Video Knowledge Bundle\n\n")
	fmt.Fprintf(&b, "- 任务ID: `%s`\n", job.ID)
	fmt.Fprintf(&b, "- 标题: %s\n", title)
	fmt.Fprintf(&b, "- 链接: %s\n", job.URL)
	fmt.Fprintf(&b, "- 生成时间: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("## Transcript\n\n")
	b.WriteString(toMarkdown(job.Transcript))
	b.WriteString("\n\n## Chapters\n\n")
	if len(job.Chapters) == 0 {
		b.WriteString("暂无章节。\n")
	} else {
		for i, ch := range job.Chapters {
			fmt.Fprintf(&b, "### %d. %s (%s - %s)\n", i+1, ch.Title, srtTimestamp(ch.StartSec), srtTimestamp(ch.EndSec))
			for _, bullet := range ch.Bullets {
				fmt.Fprintf(&b, "- %s\n", bullet)
			}
			for _, q := range ch.KeyQuotes {
				fmt.Fprintf(&b, "> [%s-%s] %s\n", srtTimestamp(q.StartSec), srtTimestamp(q.EndSec), q.Text)
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString("## Summaries\n\n")
	if len(job.Summaries) == 0 {
		b.WriteString("暂无摘要产物。\n")
	} else {
		for k, entry := range job.Summaries {
			if entry == nil || entry.Status != store.SummaryDone || strings.TrimSpace(entry.Markdown) == "" {
				continue
			}
			fmt.Fprintf(&b, "### %s\n\n%s\n\n", k, entry.Markdown)
		}
	}
	b.WriteString("## QA Sessions\n\n")
	if len(job.QASessions) == 0 {
		b.WriteString("暂无问答记录。\n")
	} else {
		for _, sess := range job.QASessions {
			fmt.Fprintf(&b, "### Session %s\n", sess.ID)
			for _, msg := range sess.Messages {
				if msg.Role == "assistant" {
					fmt.Fprintf(&b, "- 助手：%s\n", msg.Content)
				} else {
					fmt.Fprintf(&b, "- 用户：%s\n", msg.Content)
				}
				for _, c := range msg.Citations {
					fmt.Fprintf(&b, "  - 引用 [%s-%s] %s\n", srtTimestamp(c.Start), srtTimestamp(c.End), c.Text)
				}
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func toObsidianMarkdown(job *store.Job) string {
	var b strings.Builder
	title := job.URL
	if job.Source != nil && strings.TrimSpace(job.Source.Title) != "" {
		title = job.Source.Title
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "job_id: %s\n", job.ID)
	fmt.Fprintf(&b, "title: \"%s\"\n", strings.ReplaceAll(title, "\"", "'"))
	fmt.Fprintf(&b, "url: %s\n", job.URL)
	if job.Transcript != nil {
		fmt.Fprintf(&b, "language: %s\n", job.Transcript.Language)
	}
	b.WriteString("tags: [video, transcript, scribe-web]\n")
	b.WriteString("---\n\n")
	b.WriteString("# [[Video Notes]]\n\n")
	b.WriteString("## 概览\n\n")
	fmt.Fprintf(&b, "- [[%s]]\n", strings.ReplaceAll(title, "]", ""))
	b.WriteString("\n## 章节\n\n")
	if len(job.Chapters) == 0 {
		b.WriteString("- 暂无章节\n")
	} else {
		for _, ch := range job.Chapters {
			fmt.Fprintf(&b, "- %s (%s-%s)\n", ch.Title, srtTimestamp(ch.StartSec), srtTimestamp(ch.EndSec))
		}
	}
	b.WriteString("\n## 正文\n\n")
	if job.Transcript != nil {
		b.WriteString(job.Transcript.FullText)
	}
	b.WriteByte('\n')
	return b.String()
}

func toNotionImportMarkdown(job *store.Job) string {
	var b strings.Builder
	b.WriteString("# Notion Import Draft\n\n")
	b.WriteString("## Metadata\n")
	fmt.Fprintf(&b, "- Job ID: %s\n- URL: %s\n\n", job.ID, job.URL)
	b.WriteString("## Transcript\n\n")
	if job.Transcript != nil {
		b.WriteString(job.Transcript.FullText)
	}
	b.WriteString("\n\n## Chapters\n\n")
	for _, ch := range job.Chapters {
		fmt.Fprintf(&b, "### %s\n", ch.Title)
		for _, bullet := range ch.Bullets {
			fmt.Fprintf(&b, "- %s\n", bullet)
		}
	}
	return b.String()
}

func toXmindOutline(job *store.Job) string {
	var b strings.Builder
	b.WriteString("# XMind Outline\n\n")
	title := job.URL
	if job.Source != nil && job.Source.Title != "" {
		title = job.Source.Title
	}
	fmt.Fprintf(&b, "- %s\n", title)
	for _, ch := range job.Chapters {
		fmt.Fprintf(&b, "  - %s (%s-%s)\n", ch.Title, srtTimestamp(ch.StartSec), srtTimestamp(ch.EndSec))
		for _, bullet := range ch.Bullets {
			fmt.Fprintf(&b, "    - %s\n", bullet)
		}
	}
	return b.String()
}

func toXmindJSON(job *store.Job) map[string]any {
	title := job.URL
	if job.Source != nil && job.Source.Title != "" {
		title = job.Source.Title
	}
	children := make([]map[string]any, 0, len(job.Chapters))
	for _, ch := range job.Chapters {
		nodes := make([]map[string]any, 0, len(ch.Bullets))
		for _, bullet := range ch.Bullets {
			nodes = append(nodes, map[string]any{"title": bullet})
		}
		children = append(children, map[string]any{
			"title":    fmt.Sprintf("%s (%s-%s)", ch.Title, srtTimestamp(ch.StartSec), srtTimestamp(ch.EndSec)),
			"children": nodes,
		})
	}
	return map[string]any{
		"title":    title,
		"children": children,
	}
}

func srtTimestamp(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	h := int(sec) / 3600
	m := (int(sec) % 3600) / 60
	s := int(sec) % 60
	ms := int((sec - float64(int(sec))) * 1000)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}
