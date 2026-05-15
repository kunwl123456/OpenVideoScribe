// Package asr — zh.go provides best-effort Traditional → Simplified
// Chinese normalization for whisper.cpp output. whisper.cpp's training
// data leans Traditional, so most "zh" jobs come back as Big5-style
// glyphs even when the speaker is using Mandarin/Putonghua. We post-
// process here so the UI sees Simplified text by default.
//
// We use longbridgeapp/opencc which is pure-Go (no cgo) and ships its
// own t2s dictionaries, so deployment stays a single binary.
package asr

import (
	"log"
	"sync"

	"github.com/longbridgeapp/opencc"
)

var (
	t2sOnce sync.Once
	t2s     *opencc.OpenCC
	t2sErr  error
)

func ensureT2S() (*opencc.OpenCC, error) {
	t2sOnce.Do(func() {
		t2s, t2sErr = opencc.New("t2s")
		if t2sErr != nil {
			log.Printf("opencc t2s init failed: %v (transcripts will stay as-is)", t2sErr)
		}
	})
	return t2s, t2sErr
}

// ToSimplified converts a Traditional Chinese string to Simplified.
// On any failure it returns the input untouched — never blocks the
// transcription pipeline.
func ToSimplified(text string) string {
	cc, err := ensureT2S()
	if err != nil || cc == nil {
		return text
	}
	out, err := cc.Convert(text)
	if err != nil {
		return text
	}
	return out
}
