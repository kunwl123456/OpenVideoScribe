// Package summary — prompts.go centralises the four prompt templates.
// Each prompt is its own const so designers can iterate without
// touching code, and every template receives the same PromptVars set
// so a transcript only needs to be prepared once per Generate call.
//
// 全部使用简体中文。即使源视频是繁体或英文，我们也要求模型用简体中文
// 输出 — 这是国内用户的预期。
package summary

import (
	"bytes"
	"strings"
	"text/template"
)

// PromptVars feeds every prompt template. Keep the field set small and
// stable; adding a field is fine, removing one breaks templates.
type PromptVars struct {
	Title           string
	Uploader        string
	DurationSeconds int
	FullText        string // already truncated by Service.prepareText
	// VisualInsights is a pre-rendered block of per-timestamp visual
	// descriptions (caption + OCR text) produced by the VLM stage.
	// Empty when the visual stage didn't run; templates conditionally
	// include the block via `{{- if .VisualInsights}}`.
	VisualInsights string
}

// systemPrompt frames the assistant's role for every Kind. We bake the
// simplified-Chinese instruction here so individual user prompts can
// stay short.
const systemPrompt = `你是一位中文音视频内容编辑助手，擅长把口语化的转写文稿整理成结构化、可读性强的笔记。
严格遵守以下规则：
1. 全文输出必须是简体中文，即使原文是繁体或外语。
2. 不要捏造没有出现在转写中的事实；如果信息不足就明说"原文未提及"。
3. 直接输出结果，不要写"好的"、"以下是"之类的开场白。`

// briefPrompt — 一句话总结。控制在 50 字左右，方便首屏展示。
const briefPrompt = `请用一句话（控制在 50 个汉字以内）总结下面这段视频转写，提炼最核心的观点或结论。

视频标题：{{.Title}}
{{- if .Uploader}}
作者：{{.Uploader}}
{{- end}}
{{- if .DurationSeconds}}
时长：约 {{.DurationSeconds}} 秒
{{- end}}

转写正文：
"""
{{.FullText}}
"""
{{- if .VisualInsights}}

画面信息（按时间戳，由视觉模型生成，可能与转写互补）：
{{.VisualInsights}}
{{- end}}

只返回那一句总结，不要任何前缀。`

// detailedPrompt — ~300 字详细摘要。允许多段。
const detailedPrompt = `请基于下面这段视频转写，写一段约 300 字的详细摘要，覆盖：
- 视频的核心观点或主线；
- 作者给出的关键论据 / 案例 / 数据；
- 任何明确给出的结论、行动建议或反思。

摘要必须是连贯的散文（可分 1-3 段），不要使用列表符号或编号。

视频标题：{{.Title}}
{{- if .Uploader}}
作者：{{.Uploader}}
{{- end}}

转写正文：
"""
{{.FullText}}
"""
{{- if .VisualInsights}}

画面信息（按时间戳，由视觉模型生成，可能与转写互补）：
{{.VisualInsights}}
{{- end}}

只返回那段摘要，不要 Markdown 标题，也不要"以下是"等开场白。`

// outlinePrompt — markdown 列表大纲。
const outlinePrompt = `请把下面这段视频转写整理成一份层级清晰的大纲，使用 Markdown 列表语法。

要求：
- 顶层 3-6 个章节；每个章节用 ` + "`## 标题`" + ` 作为段落小标题；
- 每个章节下用 ` + "`-`" + ` 列出 2-5 条要点，要点要凝练，不要直接复述原文；
- 若原文出现具体数字、名词、引用，请在要点中保留；
- 最末尾加一行 ` + "`## 一句话总结`" + ` 给出整体概括。

视频标题：{{.Title}}
{{- if .Uploader}}
作者：{{.Uploader}}
{{- end}}

转写正文：
"""
{{.FullText}}
"""
{{- if .VisualInsights}}

画面信息（按时间戳，由视觉模型生成，可能与转写互补）：
{{.VisualInsights}}
{{- end}}

直接输出 Markdown，不要再额外解释。`

// mindmapPrompt — markmap 兼容的多级 markdown 树。
const mindmapPrompt = `请基于下面这段视频转写生成一份思维导图，使用 Markmap 兼容的 Markdown 语法。

格式约定（必须严格遵守，否则 Markmap 无法渲染）：
- 根节点用 ` + "`# 标题`" + ` 一行；
- 二级节点用 ` + "`## ...`" + `；
- 三级节点用 ` + "`### ...`" + ` 或者无序列表 ` + "`- ...`" + `；
- 四级及以下用嵌套的 ` + "`- ...`" + ` 列表（缩进两个空格）；
- 不要写正文段落，每个节点是一个短语（不超过 18 个汉字）；
- 整张导图覆盖视频的主线、支线、关键名词、结论。

视频标题：{{.Title}}
{{- if .Uploader}}
作者：{{.Uploader}}
{{- end}}

转写正文：
"""
{{.FullText}}
"""
{{- if .VisualInsights}}

画面信息（按时间戳，由视觉模型生成，可能与转写互补）：
{{.VisualInsights}}
{{- end}}

直接输出 Markdown，不要 ` + "```" + ` 代码围栏，也不要额外解释。`

const studyNotesPrompt = `请把视频内容整理成“学习笔记”Markdown。

结构要求：
- ` + "`## 核心概念`" + `：列出 3-8 个概念并解释；
- ` + "`## 关键结论`" + `：列出 3-6 条结论；
- ` + "`## 易错点与澄清`" + `：列出 2-5 条；
- ` + "`## 复习清单`" + `：给出可执行的 5 条复习动作。

视频标题：{{.Title}}
{{- if .Uploader}}
作者：{{.Uploader}}
{{- end}}

转写正文：
"""
{{.FullText}}
"""
{{- if .VisualInsights}}

画面信息（按时间戳，由视觉模型生成，可能与转写互补）：
{{.VisualInsights}}
{{- end}}

直接输出 Markdown，不要额外解释。`

const wechatArticlePrompt = `请把视频内容改写成“公众号文章”草稿（简体中文），用 Markdown 输出。

要求：
- 文章标题 + 导语 + 正文分节 + 结尾互动；
- 行文自然，适合公众号阅读；
- 保留原文关键事实，不得编造。

视频标题：{{.Title}}
{{- if .Uploader}}
作者：{{.Uploader}}
{{- end}}

转写正文：
"""
{{.FullText}}
"""
{{- if .VisualInsights}}

画面信息（按时间戳，由视觉模型生成，可能与转写互补）：
{{.VisualInsights}}
{{- end}}

直接输出文章 Markdown。`

const courseHandoutPrompt = `请基于视频转写生成“课程讲义”Markdown。

结构要求：
- ` + "`## 课程目标`" + `
- ` + "`## 知识结构`" + `（分点）
- ` + "`## 讲解要点`" + `（按章节）
- ` + "`## 课堂练习`" + `（至少 3 题）
- ` + "`## 课后作业`" + `

视频标题：{{.Title}}
{{- if .Uploader}}
作者：{{.Uploader}}
{{- end}}

转写正文：
"""
{{.FullText}}
"""
{{- if .VisualInsights}}

画面信息（按时间戳，由视觉模型生成，可能与转写互补）：
{{.VisualInsights}}
{{- end}}

直接输出讲义 Markdown。`

const shortVideoScriptPrompt = `请将视频要点改写为“短视频脚本”Markdown，适合 60-180 秒内容。

结构要求：
- ` + "`## 选题标题`" + `
- ` + "`## 口播脚本`" + `（分镜头/段落，标注时长）
- ` + "`## 屏幕字幕建议`" + `
- ` + "`## 结尾互动话术`" + `

视频标题：{{.Title}}
{{- if .Uploader}}
作者：{{.Uploader}}
{{- end}}

转写正文：
"""
{{.FullText}}
"""
{{- if .VisualInsights}}

画面信息（按时间戳，由视觉模型生成，可能与转写互补）：
{{.VisualInsights}}
{{- end}}

直接输出脚本 Markdown。`

const quoteCardsPrompt = `请从视频中提炼“金句卡片”Markdown。

要求：
- 输出 8-15 条金句；
- 每条包含：` + "`金句`" + ` + ` + "`解读`" + ` + ` + "`适用场景`" + `；
- 尽量保留原文表达，不要编造未出现观点。

视频标题：{{.Title}}
{{- if .Uploader}}
作者：{{.Uploader}}
{{- end}}

转写正文：
"""
{{.FullText}}
"""
{{- if .VisualInsights}}

画面信息（按时间戳，由视觉模型生成，可能与转写互补）：
{{.VisualInsights}}
{{- end}}

直接输出 Markdown。`

// promptForKind returns the template body for the given Kind, with
// systemPrompt always paired alongside.
func promptForKind(k Kind) string {
	switch k {
	case KindBrief:
		return briefPrompt
	case KindDetailed:
		return detailedPrompt
	case KindOutline:
		return outlinePrompt
	case KindMindmap:
		return mindmapPrompt
	case KindStudyNotes:
		return studyNotesPrompt
	case KindWechatArticle:
		return wechatArticlePrompt
	case KindCourseHandout:
		return courseHandoutPrompt
	case KindShortVideoScript:
		return shortVideoScriptPrompt
	case KindQuoteCards:
		return quoteCardsPrompt
	}
	return briefPrompt
}

// renderPrompt expands the chosen template against vars. Errors here
// only happen on template author mistakes, never on user input — so
// they surface as 500s, which is correct.
func renderPrompt(k Kind, v PromptVars) (string, error) {
	tmpl, err := template.New(string(k)).Parse(promptForKind(k))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, v); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}
