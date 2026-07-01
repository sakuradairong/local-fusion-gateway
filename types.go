package main

import (
	"encoding/json"
	"time"
)

// ChatCompletionRequest represents an OpenAI-compatible chat completion request.
type ChatCompletionRequest struct {
	Model        string                     `json:"model"`
	Messages     []Message                  `json:"messages"`
	Stream       bool                       `json:"stream,omitempty"`
	Tools        json.RawMessage            `json:"tools,omitempty"`
	ToolChoice   json.RawMessage            `json:"tool_choice,omitempty"`
	CodeResearch *CodeResearchConfig        `json:"code_research,omitempty"`
	Extra        map[string]json.RawMessage `json:"-"`
}

func (r *ChatCompletionRequest) UnmarshalJSON(data []byte) error {
	type alias ChatCompletionRequest
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	delete(raw, "model")
	delete(raw, "messages")
	delete(raw, "stream")
	delete(raw, "tools")
	delete(raw, "tool_choice")
	delete(raw, "code_research")
	*r = ChatCompletionRequest(a)
	if len(raw) > 0 {
		r.Extra = raw
	}
	return nil
}

func (r ChatCompletionRequest) MarshalJSON() ([]byte, error) {
	m := make(map[string]json.RawMessage, len(r.Extra)+6)
	for k, v := range r.Extra {
		m[k] = v
	}
	putJSON(m, "model", r.Model)
	putJSON(m, "messages", r.Messages)
	if r.Stream {
		putJSON(m, "stream", r.Stream)
	}
	if len(r.Tools) > 0 {
		m["tools"] = r.Tools
	}
	if len(r.ToolChoice) > 0 {
		m["tool_choice"] = r.ToolChoice
	}
	if r.CodeResearch != nil {
		putJSON(m, "code_research", r.CodeResearch)
	}
	return json.Marshal(m)
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
	Role       string                     `json:"role"`
	Content    string                     `json:"content,omitempty"`
	ContentRaw json.RawMessage            `json:"-"`
	Name       string                     `json:"name,omitempty"`
	ToolCallID string                     `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage            `json:"tool_calls,omitempty"`
	Extra      map[string]json.RawMessage `json:"-"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v := raw["role"]; len(v) > 0 {
		_ = json.Unmarshal(v, &m.Role)
	}
	if v := raw["content"]; len(v) > 0 {
		var text string
		if err := json.Unmarshal(v, &text); err == nil {
			m.Content = text
		} else {
			m.ContentRaw = append(json.RawMessage(nil), v...)
		}
	}
	if v := raw["name"]; len(v) > 0 {
		_ = json.Unmarshal(v, &m.Name)
	}
	if v := raw["tool_call_id"]; len(v) > 0 {
		_ = json.Unmarshal(v, &m.ToolCallID)
	}
	if v := raw["tool_calls"]; len(v) > 0 {
		m.ToolCalls = append(json.RawMessage(nil), v...)
	}
	delete(raw, "role")
	delete(raw, "content")
	delete(raw, "name")
	delete(raw, "tool_call_id")
	delete(raw, "tool_calls")
	if len(raw) > 0 {
		m.Extra = raw
	}
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	raw := make(map[string]json.RawMessage, len(m.Extra)+5)
	for k, v := range m.Extra {
		raw[k] = v
	}
	putJSON(raw, "role", m.Role)
	if len(m.ContentRaw) > 0 {
		raw["content"] = m.ContentRaw
	} else if m.Content != "" {
		putJSON(raw, "content", m.Content)
	}
	if m.Name != "" {
		putJSON(raw, "name", m.Name)
	}
	if m.ToolCallID != "" {
		putJSON(raw, "tool_call_id", m.ToolCallID)
	}
	if len(m.ToolCalls) > 0 {
		raw["tool_calls"] = m.ToolCalls
	}
	return json.Marshal(raw)
}

func putJSON(m map[string]json.RawMessage, key string, value any) {
	data, _ := json.Marshal(value)
	m[key] = data
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
