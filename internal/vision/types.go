// Package vision turns a set of extracted keyframes into structured
// per-timestamp insights (caption + OCR) by prompting an OpenAI-vision
// compatible provider. Stateless; persistence is the caller's job.
//
// types.go isolates the on-the-wire types so internal/store can import
// them without dragging in net/http or the actual Service.
package vision

// Insight is one analysed keyframe. Persisted verbatim inside
// store.Job.Frames; the JSON field names form a public contract with
// the React frontend and any third-party consumer of /api/jobs/{id}.
//
// Caption is a short Chinese sentence describing what the frame shows.
// OCRText, when non-empty, lists the on-screen text the model spotted
// in the frame — used by the summary stage to surface slide content
// the audio transcript may have missed.
type Insight struct {
	Index        int     `json:"index"`
	TimestampSec float64 `json:"timestamp_sec"`
	ImagePath    string  `json:"image_path,omitempty"` // absolute path on disk
	Caption      string  `json:"caption"`
	OCRText      string  `json:"ocr_text,omitempty"`
	TokensUsed   int     `json:"tokens_used,omitempty"`
	DurationMs   int64   `json:"duration_ms,omitempty"`
	Error        string  `json:"error,omitempty"` // populated when this frame's VLM call failed
}
