// Package subtitle parses platform-provided subtitle files into the same
// transcript shape produced by Whisper.
package subtitle

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"scribe-web/internal/asr"
)

var tagRe = regexp.MustCompile(`<[^>]+>`)

// ParseFile reads an SRT or WebVTT subtitle file and converts its cues
// into asr.Result so the rest of the pipeline can reuse export and
// summary code unchanged.
func ParseFile(path, language string) (*asr.Result, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	segments, err := parseCues(string(raw))
	if err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("subtitle contains no cues")
	}
	var full strings.Builder
	var maxEnd float64
	for i, seg := range segments {
		if i > 0 {
			full.WriteByte('\n')
		}
		full.WriteString(seg.Text)
		if seg.End > maxEnd {
			maxEnd = seg.End
		}
	}
	modelSuffix := language
	if language == "" {
		language = "unknown"
		modelSuffix = strings.TrimPrefix(filepath.Ext(path), ".")
	}
	return &asr.Result{
		Language: language,
		Model:    "platform-subtitle:" + modelSuffix,
		Duration: maxEnd,
		Segments: segments,
		FullText: full.String(),
	}, nil
}

func parseCues(raw string) ([]asr.Segment, error) {
	raw = strings.TrimPrefix(raw, "\ufeff")
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")

	var segments []asr.Segment
	for _, block := range splitCueBlocks(raw) {
		lines := cleanBlockLines(block)
		if len(lines) == 0 {
			continue
		}
		if isWebVTTHeader(lines[0]) {
			lines = lines[1:]
			if len(lines) == 0 {
				continue
			}
		}
		if isWebVTTMetadataBlock(lines[0]) {
			continue
		}
		timeIdx := -1
		for i, line := range lines {
			if strings.Contains(line, "-->") {
				timeIdx = i
				break
			}
		}
		if timeIdx < 0 {
			continue
		}
		start, end, err := parseTimeRange(lines[timeIdx])
		if err != nil {
			continue
		}
		text := normalizeCueText(lines[timeIdx+1:])
		if text == "" {
			continue
		}
		segments = append(segments, asr.Segment{Start: start, End: end, Text: text})
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("no subtitle cues parsed")
	}
	return segments, nil
}

func splitCueBlocks(raw string) []string {
	var blocks []string
	for _, block := range strings.Split(raw, "\n\n") {
		if b := strings.TrimSpace(block); b != "" {
			blocks = append(blocks, b)
		}
	}
	return blocks
}

func cleanBlockLines(block string) []string {
	rawLines := strings.Split(block, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func isWebVTTHeader(line string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "WEBVTT")
}

func isWebVTTMetadataBlock(line string) bool {
	upper := strings.ToUpper(strings.TrimSpace(line))
	return strings.HasPrefix(upper, "NOTE") ||
		strings.HasPrefix(upper, "STYLE") ||
		strings.HasPrefix(upper, "REGION")
}

func parseTimeRange(line string) (float64, float64, error) {
	parts := strings.Split(line, "-->")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid timestamp range")
	}
	start, err := parseTimestamp(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	endField := strings.Fields(strings.TrimSpace(parts[1]))
	if len(endField) == 0 {
		return 0, 0, fmt.Errorf("missing end timestamp")
	}
	end, err := parseTimestamp(endField[0])
	if err != nil {
		return 0, 0, err
	}
	if end < start {
		return 0, 0, fmt.Errorf("subtitle end before start")
	}
	return start, end, nil
}

func parseTimestamp(raw string) (float64, error) {
	raw = strings.ReplaceAll(strings.TrimSpace(raw), ",", ".")
	parts := strings.Split(raw, ":")
	var h, m float64
	var sPart string
	switch len(parts) {
	case 3:
		var err error
		h, err = strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, err
		}
		m, err = strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, err
		}
		sPart = parts[2]
	case 2:
		var err error
		m, err = strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, err
		}
		sPart = parts[1]
	case 1:
		sPart = parts[0]
	default:
		return 0, fmt.Errorf("invalid timestamp %q", raw)
	}
	s, err := strconv.ParseFloat(sPart, 64)
	if err != nil {
		return 0, err
	}
	return h*3600 + m*60 + s, nil
}

func normalizeCueText(lines []string) string {
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = tagRe.ReplaceAllString(line, "")
		line = html.UnescapeString(line)
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			parts = append(parts, line)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}
