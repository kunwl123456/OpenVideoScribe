package httpapi

import (
	"fmt"
	"strings"

	"scribe-web/internal/asr"
)

// formatExport renders a transcript in the requested format. Returns
// the bytes + content type.
func formatExport(res *asr.Result, format string) ([]byte, string, error) {
	switch format {
	case "srt":
		return []byte(toSRT(res)), "application/x-subrip; charset=utf-8", nil
	case "md":
		return []byte(toMarkdown(res)), "text/markdown; charset=utf-8", nil
	case "txt":
		return []byte(res.FullText), "text/plain; charset=utf-8", nil
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
