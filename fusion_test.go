package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newFakeUpstream creates an httptest server that mimics an OpenAI-compatible
// chat completions endpoint, returning the given responses in order.
func newFakeUpstream(responses []fakeResponse) *httptest.Server {
	var callCount atomic.Int32
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

		idx := int(callCount.Add(1) - 1)
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

// newFakeUpstreamByModel creates an httptest server that returns a response
// based on the model field in the request, avoiding ordering assumptions when
// multiple panels are called concurrently.
func newFakeUpstreamByModel(responses map[string]fakeResponse) *httptest.Server {
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

		resp, ok := responses[req.Model]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "unknown model " + req.Model})
			return
		}

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

func TestFusionUsesRequestedPreset(t *testing.T) {
	upstream := newFakeUpstreamByModel(map[string]fakeResponse{
		"base-panel":   {Content: "base panel should not be used"},
		"base-synth":   {Content: "base synth should not be used"},
		"review-panel": {Content: "Review panel says use missing perspective context."},
		"review-synth": {Content: "Review preset synthesized answer."},
	})
	defer upstream.Close()

	cfg := &Config{
		VirtualModel:   "local/fusion",
		TimeoutSeconds: 10,
		Providers:      []Provider{{Name: "fake", BaseURL: upstream.URL}},
		Panel:          []PanelEntry{{Provider: "fake", Model: "base-panel"}},
		Synthesizer:    Synthesizer{Provider: "fake", Model: "base-synth"},
		Presets: map[string]FusionPreset{
			"review": {
				Panel:       []PanelEntry{{Provider: "fake", Model: "review-panel"}},
				Synthesizer: Synthesizer{Provider: "fake", Model: "review-synth"},
			},
		},
	}
	req := &ChatCompletionRequest{
		Model:    "local/fusion/review",
		Messages: []Message{{Role: "user", Content: "Run review preset"}},
	}

	resp, err := NewFusionService(cfg).Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "Review preset synthesized answer." {
		t.Fatalf("expected review preset synth response, got %q", got)
	}
	if resp.Model != "local/fusion/review" {
		t.Fatalf("expected response model local/fusion/review, got %q", resp.Model)
	}
}

func TestFusionUsesDefaultPresetForVirtualModel(t *testing.T) {
	upstream := newFakeUpstreamByModel(map[string]fakeResponse{
		"base-panel":   {Content: "base panel should not be used"},
		"base-synth":   {Content: "base synth should not be used"},
		"review-panel": {Content: "Review panel response."},
		"review-synth": {Content: "Default preset synthesized answer."},
	})
	defer upstream.Close()

	cfg := &Config{
		VirtualModel:   "local/fusion",
		TimeoutSeconds: 10,
		DefaultPreset:  "review",
		Providers:      []Provider{{Name: "fake", BaseURL: upstream.URL}},
		Panel:          []PanelEntry{{Provider: "fake", Model: "base-panel"}},
		Synthesizer:    Synthesizer{Provider: "fake", Model: "base-synth"},
		Presets: map[string]FusionPreset{
			"review": {
				Panel:       []PanelEntry{{Provider: "fake", Model: "review-panel"}},
				Synthesizer: Synthesizer{Provider: "fake", Model: "review-synth"},
			},
		},
	}
	req := &ChatCompletionRequest{
		Model:    "local/fusion",
		Messages: []Message{{Role: "user", Content: "Use default preset"}},
	}

	resp, err := NewFusionService(cfg).Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "Default preset synthesized answer." {
		t.Fatalf("expected default preset synth response, got %q", got)
	}
	if resp.Model != "local/fusion" {
		t.Fatalf("expected response model local/fusion, got %q", resp.Model)
	}
}

func TestFusionAllPanelsSucceed(t *testing.T) {
	// Use a model-based fake upstream so concurrent panel order does not matter.
	upstream := newFakeUpstreamByModel(map[string]fakeResponse{
		"model-a":     {Content: "Panel model A says: Paris is the capital of France."},
		"model-b":     {Content: "Panel model B says: The capital of France is Paris, known as the City of Light."},
		"model-synth": {Content: "Synthesized: Paris is indeed the capital of France, and it is known as the City of Light."},
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
	upstream := newFakeUpstreamByModel(map[string]fakeResponse{
		"model-broken": {StatusCode: http.StatusInternalServerError, Body: "crash"},
		"model-ok":     {Content: "The answer is 42."},
		"model-synth":  {Content: "Synthesized: The answer is 42."},
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

func TestFusionPartialSuccessSendsFailuresToSynthesizer(t *testing.T) {
	var capturedSynthPrompt string
	var mu sync.Mutex

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		switch req.Model {
		case "model-broken":
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("temporarily unavailable"))
		case "model-ok":
			json.NewEncoder(w).Encode(upstreamResponse{Choices: []upstreamChoice{{Message: Message{Role: "assistant", Content: "The answer is 42."}}}})
		case "model-synth":
			mu.Lock()
			if len(req.Messages) > 0 {
				capturedSynthPrompt = req.Messages[0].Content
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(upstreamResponse{Choices: []upstreamChoice{{Message: Message{Role: "assistant", Content: "Synthesized: The answer is 42."}}}})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	cfg := &Config{
		VirtualModel:   "local/fusion",
		TimeoutSeconds: 10,
		Providers:      []Provider{{Name: "fake", BaseURL: upstream.URL}},
		Panel: []PanelEntry{
			{Provider: "fake", Model: "model-broken"},
			{Provider: "fake", Model: "model-ok"},
		},
		Synthesizer: Synthesizer{Provider: "fake", Model: "model-synth"},
	}
	req := &ChatCompletionRequest{
		Model:    "local/fusion",
		Messages: []Message{{Role: "user", Content: "What is the answer to everything?"}},
	}

	_, err := NewFusionService(cfg).Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process should succeed with partial panel success: %v", err)
	}

	mu.Lock()
	prompt := capturedSynthPrompt
	mu.Unlock()
	required := []string{"model-broken", "FAILED", "Status: 503", "missing perspective", "The answer is 42."}
	for _, keyword := range required {
		if !strings.Contains(prompt, keyword) {
			t.Errorf("synthesizer prompt missing %q\nPrompt:\n%s", keyword, prompt)
		}
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

func TestBuildSynthesizerPromptIncludesPanelFailures(t *testing.T) {
	panelResults := []panelResult{
		{Provider: "p1", Model: "m1", Content: "The sky is blue."},
		{Provider: "p2", Model: "m2", StatusCode: http.StatusUnauthorized, Err: errors.New("upstream p2/m2 returned status 401")},
	}
	userMessages := []Message{
		{Role: "user", Content: "Why is the sky blue?"},
	}

	prompt := buildSynthesizerPrompt(userMessages, panelResults)

	required := []string{
		"p1/m1",
		"The sky is blue.",
		"p2/m2",
		"FAILED",
		"status 401",
		"missing perspective",
	}
	for _, keyword := range required {
		if !strings.Contains(prompt, keyword) {
			t.Errorf("synthesizer prompt missing expected failure keyword: %q\nPrompt:\n%s", keyword, prompt)
		}
	}
}

func TestBuildSynthesizerPromptSanitizesPanelFailureErrors(t *testing.T) {
	panelResults := []panelResult{
		{Provider: "p1", Model: "m1", Err: errors.New("upstream request failed for p1/m1: dial tcp api.internal.example.com:443: connection refused")},
	}

	prompt := buildSynthesizerPrompt([]Message{{Role: "user", Content: "hello"}}, panelResults)

	for _, forbidden := range []string{"api.internal.example.com", "connection refused", "dial tcp"} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("expected panel failure prompt to redact %q; prompt:\n%s", forbidden, prompt)
		}
	}
	if !strings.Contains(prompt, "upstream request failed") {
		t.Errorf("expected generic upstream request failure; prompt:\n%s", prompt)
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

func TestBuildSynthesizerPromptIncludesContentRaw(t *testing.T) {
	panelResults := []panelResult{
		{Provider: "p1", Model: "m1", Content: "The sky is blue."},
	}
	userMessages := []Message{
		{Role: "user", ContentRaw: json.RawMessage(`[{"type":"text","text":"Why is the sky blue?"}]`)},
	}
	prompt := buildSynthesizerPrompt(userMessages, panelResults)
	if !strings.Contains(prompt, `[{"type":"text","text":"Why is the sky blue?"}]`) {
		t.Errorf("synthesizer prompt missing ContentRaw value; prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Why is the sky blue?") {
		t.Errorf("synthesizer prompt missing user message; prompt:\n%s", prompt)
	}
}

func TestBuildCodeResearchMessagesIncludesContentRaw(t *testing.T) {
	msgs := []Message{
		{Role: "user", ContentRaw: json.RawMessage(`[{"type":"text","text":"hello"}]`)},
	}
	messages := buildCodeResearchMessages(msgs, CodeResearchConfig{Workdir: "/tmp"})
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if !strings.Contains(messages[0].Content, `[{"type":"text","text":"hello"}]`) {
		t.Errorf("code research prompt missing ContentRaw value; prompt:\n%s", messages[0].Content)
	}
}

func TestFusionPreservesExtraParameters(t *testing.T) {
	var capturedExtras []map[string]json.RawMessage
	var mu sync.Mutex

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		capturedExtras = append(capturedExtras, req.Extra)
		mu.Unlock()

		upResp := upstreamResponse{
			Choices: []upstreamChoice{
				{Message: Message{Role: "assistant", Content: "ok"}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(upResp)
	}))
	defer upstream.Close()

	cfg := &Config{
		VirtualModel:   "local/fusion",
		TimeoutSeconds: 10,
		Providers:      []Provider{{Name: "fake", BaseURL: upstream.URL}},
		Panel:          []PanelEntry{{Provider: "fake", Model: "panel"}},
		Synthesizer:    Synthesizer{Provider: "fake", Model: "synth"},
	}
	req := &ChatCompletionRequest{
		Model: "local/fusion",
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
		Extra: map[string]json.RawMessage{
			"temperature": json.RawMessage(`0.7`),
		},
	}

	_, err := NewFusionService(cfg).Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(capturedExtras) != 2 {
		t.Fatalf("expected 2 upstream calls, got %d", len(capturedExtras))
	}
	for i, extra := range capturedExtras {
		if extra == nil || string(extra["temperature"]) != "0.7" {
			t.Errorf("call %d missing temperature extra, got %v", i, extra)
		}
	}
}
