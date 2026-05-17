// Package vision — prompt.go centralises the per-frame VLM instruction
// and the small parser that splits the model reply back into Caption +
// OCRText. Keeping prompt + parser together makes iteration safe: a
// template tweak that changes the output shape forces an obvious edit
// in this same file.
package vision

import "strings"

// systemPrompt frames the assistant for every frame call. We force
// simplified Chinese output and a fixed two-line shape so the parser
// stays trivial.
const systemPrompt = `你是一位中文视频画面分析助手。看到一张视频截图后，请用简体中文严格按下面两行格式回复，不要任何额外内容：
画面：<不超过 40 个汉字的一句话画面描述>
文字：<逐字抄写画面里出现的所有可见文字；多段文字用空格分隔；如果画面无文字就写：无>`

// userPromptText is the user-side text instruction paired with each
// frame image. Kept short on purpose — the system prompt does the
// heavy lifting; this one just nudges the model.
const userPromptText = `请描述这一帧的画面并抄写其中的文字。`

// parseReply splits the model's two-line answer into Caption + OCRText.
// Robust to:
//   - leading / trailing whitespace,
//   - the model writing "画面:" / "文字:" with ASCII colon instead of "：",
//   - the model bundling everything onto one line,
//   - the model emitting only "画面" when no text is present.
//
// If parsing fails completely we return the entire reply as the Caption
// so downstream summarisation still sees *something* useful.
func parseReply(raw string) (caption, ocr string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ""
	}
	// Normalise full-width colon to ASCII so the rest is one branch.
	normalised := strings.ReplaceAll(s, "：", ":")
	lines := strings.Split(normalised, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "画面:"):
			caption = strings.TrimSpace(strings.TrimPrefix(line, "画面:"))
		case strings.HasPrefix(line, "文字:"):
			ocr = strings.TrimSpace(strings.TrimPrefix(line, "文字:"))
		}
	}
	if ocr == "无" || ocr == "无。" {
		ocr = ""
	}
	if caption == "" {
		// Model went off-script. Take the whole reply as the caption so
		// the summary stage at least sees the model's words verbatim.
		caption = strings.TrimSpace(s)
	}
	return caption, ocr
}
