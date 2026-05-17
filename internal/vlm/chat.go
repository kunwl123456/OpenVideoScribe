// Package vlm — chat.go defines the OpenAI Chat Completions wire types
// extended for multi-modal (image + text) messages. Field names + json
// tags follow the OpenAI vision spec verbatim so the same payload works
// for 火山方舟 / Doubao Vision, OpenAI gpt-4o, and any compatible fork.
//
// We keep this package side-by-side with internal/llm rather than
// merging them: llm.ChatMessage.Content is a plain string, vision wants
// a []ContentPart array — collapsing both shapes into one struct would
// force an interface{} or a custom MarshalJSON on every code path. Two
// thin packages stay both type-safe and short.
package vlm

// ContentPart is one element of a multi-modal message body. Exactly
// one of Text / ImageURL is populated, depending on Type.
type ContentPart struct {
	Type     string    `json:"type"`               // "text" | "image_url"
	Text     string    `json:"text,omitempty"`     // when Type == "text"
	ImageURL *ImageURL `json:"image_url,omitempty"` // when Type == "image_url"
}

// ImageURL carries either an http(s):// URL or a data: URI. For local
// frame files we always embed as base64 data URI (see EncodeImage)
// because the upstream provider must be able to fetch the bytes and
// our keyframes only exist on the server's disk.
type ImageURL struct {
	URL string `json:"url"`
}

// ChatMessage is one turn in a vision conversation. Content is always
// an array — even for plain text we wrap a single ContentPart{Type:"text"}.
// Some providers tolerate a bare string here, but the array form is the
// official OpenAI spec and is universally accepted.
type ChatMessage struct {
	Role    string        `json:"role"`              // "system" | "user" | "assistant"
	Content []ContentPart `json:"content"`
	Name    string        `json:"name,omitempty"`
}

// ChatRequest is the body POSTed to {base_url}/chat/completions.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

// ChatResponse is the subset of the response we actually consume.
// Vision providers return a plain string completion (the model answers
// in text), so Choices[].Message.Content is a string — NOT an array,
// even though the request side is. We use a small wrapper type below
// to handle both possible shapes defensively.
type ChatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Created int64        `json:"created"`
	Choices []ChatChoice `json:"choices"`
	Usage   ChatUsage    `json:"usage"`
}

// ChatChoice is one candidate completion. We only ever look at index 0.
type ChatChoice struct {
	Index        int             `json:"index"`
	Message      AssistantOutput `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

// AssistantOutput is the response-side message. Content is plain text
// (the model's natural-language reply about the image).
type AssistantOutput struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatUsage mirrors OpenAI's token accounting.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
