// Package llm — chat.go defines the OpenAI Chat Completions wire types.
// Field names + json tags follow the OpenAI spec verbatim so the same
// payload works for any compatible provider (火山方舟 / DeepSeek / OpenAI
// / vLLM / Ollama-OpenAI-compat / ...).
package llm

// ChatMessage is one turn in a conversation.
type ChatMessage struct {
	Role    string `json:"role"`              // system | user | assistant
	Content string `json:"content"`           // plain text only for our use
	Name    string `json:"name,omitempty"`    // unused; kept for completeness
}

// ChatRequest is the body sent to POST {base_url}/chat/completions.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	// Stream is intentionally absent — we use synchronous responses for
	// the MVP. Adding it later only requires a new code path; the field
	// itself is a simple bool when needed.
}

// ChatResponse is the bits of the upstream JSON we actually consume.
// Anything else is ignored on purpose (forward compatibility).
type ChatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Created int64        `json:"created"`
	Choices []ChatChoice `json:"choices"`
	Usage   ChatUsage    `json:"usage"`
}

// ChatChoice is one candidate completion. We only ever look at index 0.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatUsage mirrors OpenAI's token accounting. Doubao / DeepSeek return
// the same fields; if a provider omits them the zero value is fine.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
