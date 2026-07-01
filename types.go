package main

import "time"

// ChatCompletionRequest represents an OpenAI-compatible chat completion request.
type ChatCompletionRequest struct {
	Model        string              `json:"model"`
	Messages     []Message           `json:"messages"`
	Stream       bool                `json:"stream,omitempty"`
	CodeResearch *CodeResearchConfig `json:"code_research,omitempty"`
}

// CodeResearchConfig enables a controlled local-code research loop for panel models.
type CodeResearchConfig struct {
	Enabled        bool   `json:"enabled"`
	Workdir        string `json:"workdir"`
	MaxRounds      int    `json:"max_rounds,omitempty"`
	MaxFileBytes   int    `json:"max_file_bytes,omitempty"`
	MaxTotalBytes  int    `json:"max_total_bytes,omitempty"`
	IncludeGitDiff bool   `json:"include_git_diff,omitempty"`
}

// Message represents a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCompletionResponse represents an OpenAI-compatible chat completion response.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice represents a single completion choice.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage contains token usage information.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// upstreamResponse is the minimal response we read from upstream providers.
type upstreamResponse struct {
	Choices []upstreamChoice `json:"choices"`
}

type upstreamChoice struct {
	Message Message `json:"message"`
}

// ErrorResponse is the OpenAI-compatible error format.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains error information.
type ErrorDetail struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`
}

// panelResult holds the result from a single panel model query.
type panelResult struct {
	Provider     string
	Model        string
	StatusCode   int
	Content      string
	Err          error
	CodeResearch *DebugCodeResearchPanel
}

// estimateTokens provides a rough token count estimate (4 chars ≈ 1 token for English).
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return max(1, len(text)/4)
}

// nowUnix returns the current Unix timestamp in seconds.
func nowUnix() int64 {
	return time.Now().Unix()
}
