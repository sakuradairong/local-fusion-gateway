package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newFakeUpstream creates an httptest server that mimics an OpenAI-compatible
// chat completions endpoint, returning the given responses in order.
func newFakeUpstream(responses []fakeResponse) *httptest.Server {
	callCount := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		idx := callCount
		callCount++
		if idx >= len(responses) {
			// Return a default error if we run out of responses.
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "no more fake responses"})
			return
		}

		resp := responses[idx]
		if resp.StatusCode != 0 && resp.StatusCode != http.StatusOK {
			w.WriteHeader(resp.StatusCode)
			if resp.Body != "" {
				w.Write([]byte(resp.Body))
			}
			return
		}

		upResp := upstreamResponse{
			Choices: []upstreamChoice{
				{
					Message: Message{
						Role:    "assistant",
						Content: resp.Content,
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(upResp)
	}))
}

type fakeResponse struct {
	Content    string
	StatusCode int
	Body       string
}

func TestFusionAllPanelsSucceed(t *testing.T) {
	// Create a fake upstream that serves both panel and synthesizer.
	upstream := newFakeUpstream([]fakeResponse{
		{Content: "Panel model A says: Paris is the capital of France."},
		{Content: "Panel model B says: The capital of France is Paris, known as the City of Light."},
		{Content: "Synthesized: Paris is indeed the capital of France, and it is known as the City of Light."},
	})
	defer upstream.Close()

	cfg := &Config{
		VirtualModel:   "local/fusion",
		TimeoutSeconds: 10,
		Providers: []Provider{
			{Name: "fake", BaseURL: upstream.URL, APIKeyEnv: ""},
		},
		Panel: []PanelEntry{
			{Provider: "fake", Model: "model-a"},
			{Provider: "fake", Model: "model-b"},
		},
		Synthesizer: Synthesizer{Provider: "fake", Model: "model-synth"},
	}

	svc := NewFusionService(cfg)
	req := &ChatCompletionRequest{
		Model: "local/fusion",
		Messages: []Message{
			{Role: "user", Content: "What is the capital of France?"},
		},
	}

	resp, err := svc.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if resp.Object != "chat.completion" {
		t.Errorf("expected chat.completion, got %q", resp.Object)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "Paris") {
		t.Errorf("expected response to contain Paris, got %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("expected finish_reason stop, got %q", resp.Choices[0].FinishReason)
	}
}

func TestFusionNoPanelSuccess(t *testing.T) {
	// All panel calls return 500.
	upstream := newFakeUpstream([]fakeResponse{
		{StatusCode: http.StatusInternalServerError, Body: "internal error"},
		{StatusCode: http.StatusInternalServerError, Body: "internal error"},
	})
	defer upstream.Close()

	cfg := &Config{
		VirtualModel:   "local/fusion",
		TimeoutSeconds: 10,
		Providers: []Provider{
			{Name: "fake", BaseURL: upstream.URL, APIKeyEnv: ""},
		},
		Panel: []PanelEntry{
			{Provider: "fake", Model: "model-a"},
			{Provider: "fake", Model: "model-b"},
		},
		Synthesizer: Synthesizer{Provider: "fake", Model: "model-synth"},
	}

	svc := NewFusionService(cfg)
	req := &ChatCompletionRequest{
		Model: "local/fusion",
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}

	_, err := svc.Process(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when no panel succeeds")
	}
	if !strings.Contains(err.Error(), "all panel models failed") {
		t.Errorf("expected 'all panel models failed' in error, got: %v", err)
	}
}

func TestFusionPartialSuccess(t *testing.T) {
	// First panel fails, second panel succeeds, synthesizer succeeds.
	upstream := newFakeUpstream([]fakeResponse{
		{StatusCode: http.StatusInternalServerError, Body: "crash"},
		{Content: "The answer is 42."},
		{Content: "Synthesized: The answer is 42."},
	})
	defer upstream.Close()

	cfg := &Config{
		VirtualModel:   "local/fusion",
		TimeoutSeconds: 10,
		Providers: []Provider{
			{Name: "fake", BaseURL: upstream.URL, APIKeyEnv: ""},
		},
		Panel: []PanelEntry{
			{Provider: "fake", Model: "model-broken"},
			{Provider: "fake", Model: "model-ok"},
		},
		Synthesizer: Synthesizer{Provider: "fake", Model: "model-synth"},
	}

	svc := NewFusionService(cfg)
	req := &ChatCompletionRequest{
		Model: "local/fusion",
		Messages: []Message{
			{Role: "user", Content: "What is the answer to everything?"},
		},
	}

	resp, err := svc.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process should succeed with partial panel success: %v", err)
	}

	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "42") {
		t.Errorf("expected response to contain 42, got %q", resp.Choices[0].Message.Content)
	}
}

func TestFusionEmptyMessages(t *testing.T) {
	cfg := &Config{
		VirtualModel:   "local/fusion",
		TimeoutSeconds: 10,
		Providers:      []Provider{},
		Panel:          []PanelEntry{},
		Synthesizer:    Synthesizer{Provider: "test", Model: "test"},
	}

	svc := NewFusionService(cfg)
	req := &ChatCompletionRequest{
		Model:    "local/fusion",
		Messages: []Message{},
	}

	_, err := svc.Process(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty messages")
	}
}

func TestFusionSynthesizerPrompt(t *testing.T) {
	panelResults := []panelResult{
		{Provider: "p1", Model: "m1", Content: "The sky is blue."},
		{Provider: "p2", Model: "m2", Content: "The sky appears blue due to Rayleigh scattering."},
	}

	userMessages := []Message{
		{Role: "user", Content: "Why is the sky blue?"},
	}

	prompt := buildSynthesizerPrompt(userMessages, panelResults)

	// Verify the prompt contains all required sections.
	required := []string{
		"Consensus",
		"共识",
		"Conflicts",
		"冲突",
		"Omissions",
		"遗漏",
		"Final Answer",
		"最终答案",
		"Why is the sky blue?",
		"The sky is blue.",
		"Rayleigh scattering",
		"p1/m1",
		"p2/m2",
	}

	for _, keyword := range required {
		if !strings.Contains(prompt, keyword) {
			t.Errorf("synthesizer prompt missing expected keyword: %q\nPrompt:\n%s", keyword, prompt)
		}
	}
}

func TestFusionTimeout(t *testing.T) {
	// Create an upstream that sleeps for a long time.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(upstreamResponse{
			Choices: []upstreamChoice{
				{Message: Message{Role: "assistant", Content: "slow response"}},
			},
		})
	}))
	defer upstream.Close()

	cfg := &Config{
		VirtualModel:   "local/fusion",
		TimeoutSeconds: 1, // 1 second timeout
		Providers: []Provider{
			{Name: "slow", BaseURL: upstream.URL, APIKeyEnv: ""},
		},
		Panel: []PanelEntry{
			{Provider: "slow", Model: "slow-model"},
		},
		Synthesizer: Synthesizer{Provider: "slow", Model: "synth-model"},
	}

	svc := NewFusionService(cfg)
	req := &ChatCompletionRequest{
		Model: "local/fusion",
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// First call is panel (timeout), second would be synthesizer but we won't
	// reach it since all panels fail.
	_, err := svc.Process(ctx, req)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "all panel models failed") {
		t.Errorf("expected all panel models failed, got: %v", err)
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		text     string
		expected int
	}{
		{"", 0},
		{"abcd", 1},
		{"hello world, how are you?", 6}, // 25 chars / 4 = 6
	}
	for _, tt := range tests {
		got := estimateTokens(tt.text)
		if got != tt.expected {
			t.Errorf("estimateTokens(%q) = %d, want %d", tt.text, got, tt.expected)
		}
	}
}
