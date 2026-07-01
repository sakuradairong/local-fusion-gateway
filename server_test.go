package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer() *httptest.Server {
	cfg := &Config{
		Listen:       ":0",
		VirtualModel: "local/fusion",
		Providers:    []Provider{},
		Panel:        []PanelEntry{},
		Synthesizer:  Synthesizer{Provider: "test", Model: "test-model"},
	}
	srv := NewServer(cfg)
	return httptest.NewServer(srv.Handler())
}

func TestHealthEndpointDoesNotRequireAuth(t *testing.T) {
	t.Setenv("LOCAL_FUSION_TEST_API_KEY", "local-secret")
	cfg := &Config{
		Listen:       ":0",
		VirtualModel: "local/fusion",
		AuthTokenEnv: "LOCAL_FUSION_TEST_API_KEY",
		Providers:    []Provider{},
		Panel:        []PanelEntry{},
		Synthesizer:  Synthesizer{Provider: "test", Model: "test-model"},
	}
	srv := NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var body map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}

	if !body["ok"] {
		t.Error("expected ok:true")
	}
}

func TestModelsEndpoint(t *testing.T) {
	cfg := &Config{
		Listen:       ":0",
		VirtualModel: "my-fusion/v2",
		Panel: []PanelEntry{
			{Provider: "openrouter", Model: "gpt-4o"},
		},
		Synthesizer: Synthesizer{Provider: "ollama", Model: "llama3"},
	}
	srv := NewServer(cfg)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var body modelList
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode models response: %v", err)
	}

	if body.Object != "list" {
		t.Errorf("expected list, got %q", body.Object)
	}

	// Should include virtual model, panel model, and synthesizer model.
	if len(body.Data) < 1 {
		t.Error("expected at least 1 model")
	}

	foundVirtual := false
	foundPanel := false
	foundSynth := false
	for _, m := range body.Data {
		switch m.ID {
		case "my-fusion/v2":
			foundVirtual = true
		case "openrouter/gpt-4o":
			foundPanel = true
		case "ollama/llama3":
			foundSynth = true
		}
	}

	if !foundVirtual {
		t.Error("virtual model not found in model list")
	}
	if !foundPanel {
		t.Error("panel model not found in model list")
	}
	if !foundSynth {
		t.Error("synthesizer model not found in model list")
	}
}

func TestModelsEndpointUnauthorized(t *testing.T) {
	t.Setenv("LOCAL_FUSION_TEST_API_KEY", "local-secret")
	cfg := &Config{
		AuthTokenEnv: "LOCAL_FUSION_TEST_API_KEY",
		Panel:        []PanelEntry{{Provider: "test", Model: "panel"}},
		Synthesizer:  Synthesizer{Provider: "test", Model: "synth"},
	}
	ts := httptest.NewServer(NewServer(cfg).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error.Type != "authentication_error" {
		t.Errorf("expected authentication_error, got %q", errResp.Error.Type)
	}
	if errResp.Error.RequestID == "" {
		t.Error("expected request_id in error response")
	}
}

func TestModelsEndpointAuthorized(t *testing.T) {
	t.Setenv("LOCAL_FUSION_TEST_API_KEY", "local-secret")
	cfg := &Config{
		AuthTokenEnv: "LOCAL_FUSION_TEST_API_KEY",
		Panel:        []PanelEntry{{Provider: "test", Model: "panel"}},
		Synthesizer:  Synthesizer{Provider: "test", Model: "synth"},
	}
	ts := httptest.NewServer(NewServer(cfg).Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer local-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/models failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestStreamUnsupported(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	reqBody := ChatCompletionRequest{
		Model: "local/fusion",
		Messages: []Message{
			{Role: "user", Content: "hello"},
		},
		Stream: true,
	}

	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for stream=true, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID header")
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}

	if errResp.Error.Message != "streaming is not supported" {
		t.Errorf("expected 'streaming is not supported' message, got %q", errResp.Error.Message)
	}
	if errResp.Error.RequestID == "" {
		t.Error("expected request_id in error response")
	}
}

func TestChatCompletionBodyTooLarge(t *testing.T) {
	cfg := &Config{
		MaxBodyBytes: 32,
		Panel:        []PanelEntry{{Provider: "test", Model: "panel"}},
		Synthesizer:  Synthesizer{Provider: "test", Model: "synth"},
	}
	ts := httptest.NewServer(NewServer(cfg).Handler())
	defer ts.Close()

	largeBody := `{"model":"local/fusion","messages":[{"role":"user","content":"this body is definitely too large"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(largeBody))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(data), "this body is definitely too large") {
		t.Fatalf("413 response leaked request content: %s", string(data))
	}
}

func TestUpstreamErrorDoesNotLeakBody(t *testing.T) {
	secret := "secret-from-upstream-body"
	upstream := newFakeUpstream([]fakeResponse{
		{StatusCode: http.StatusInternalServerError, Body: "upstream failed with " + secret},
	})
	defer upstream.Close()

	cfg := &Config{
		Providers: []Provider{{Name: "fake", BaseURL: upstream.URL}},
		Panel: []PanelEntry{
			{Provider: "fake", Model: "panel"},
		},
		Synthesizer: Synthesizer{Provider: "fake", Model: "synth"},
	}
	ts := httptest.NewServer(NewServer(cfg).Handler())
	defer ts.Close()

	body, err := json.Marshal(ChatCompletionRequest{
		Model:    "local/fusion",
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	responseBody := string(data)
	if strings.Contains(responseBody, secret) {
		t.Fatalf("upstream secret leaked to client: %s", responseBody)
	}
	if !strings.Contains(responseBody, "fake/panel") || !strings.Contains(responseBody, "status 500") {
		t.Fatalf("expected sanitized provider/model/status summary, got: %s", responseBody)
	}
}

func TestDebugEnabledWritesSanitizedFile(t *testing.T) {
	secretToken := "s" + "k-test-provider-secret"
	t.Setenv("FAKE_PROVIDER_API_KEY", secretToken)
	debugDir := t.TempDir()
	upstream := newFakeUpstream([]fakeResponse{
		{Content: "panel response"},
		{Content: "synth response"},
	})
	defer upstream.Close()

	cfg := &Config{
		Debug: DebugConfig{Enabled: true, Dir: debugDir},
		Providers: []Provider{
			{Name: "fake", BaseURL: upstream.URL, APIKeyEnv: "FAKE_PROVIDER_API_KEY"},
		},
		Panel: []PanelEntry{
			{Provider: "fake", Model: "panel"},
		},
		Synthesizer: Synthesizer{Provider: "fake", Model: "synth"},
	}
	ts := httptest.NewServer(NewServer(cfg).Handler())
	defer ts.Close()

	body, err := json.Marshal(ChatCompletionRequest{
		Model:    "local/fusion",
		Messages: []Message{{Role: "user", Content: "hello debug"}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(data))
	}

	entries, err := os.ReadDir(debugDir)
	if err != nil {
		t.Fatalf("read debug dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one debug file, got %d", len(entries))
	}

	data, err := os.ReadFile(filepath.Join(debugDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read debug file: %v", err)
	}
	artifact := string(data)
	for _, forbidden := range []string{"Authorization", secretToken, "s" + "k-"} {
		if strings.Contains(artifact, forbidden) {
			t.Fatalf("debug artifact contains forbidden %q: %s", forbidden, artifact)
		}
	}
	for _, expected := range []string{"\"run_id\"", "\"panel\"", "\"synthesizer\"", "\"duration_ms\"", "\"final_status\": \"ok\""} {
		if !strings.Contains(artifact, expected) {
			t.Fatalf("debug artifact missing %q: %s", expected, artifact)
		}
	}
}

func TestInvalidRequestBody(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", resp.StatusCode)
	}
}
