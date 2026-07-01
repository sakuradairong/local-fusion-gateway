package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodeResearchRejectsRelativeWorkdir(t *testing.T) {
	_, err := newCodeResearchSession(CodeResearchConfig{
		Enabled:       true,
		Workdir:       "relative/path",
		MaxRounds:     1,
		MaxFileBytes:  1024,
		MaxTotalBytes: 4096,
	})
	if err == nil {
		t.Fatal("expected relative workdir to be rejected")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected absolute-path error, got %v", err)
	}
}

func TestCodeResearchReadFileRejectsPathTraversal(t *testing.T) {
	workdir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	session := mustCodeResearchSession(t, CodeResearchConfig{
		Enabled:       true,
		Workdir:       workdir,
		MaxRounds:     1,
		MaxFileBytes:  1024,
		MaxTotalBytes: 4096,
	})

	result := session.readFile(filepath.Join("..", filepath.Base(filepath.Dir(outside)), "outside.go"), 0, 20)
	if !strings.Contains(result, "ERROR") || !strings.Contains(result, "outside workdir") {
		t.Fatalf("expected path traversal rejection, got %q", result)
	}
}

func TestCodeResearchReadFileRejectsSensitiveFiles(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, ".env"), []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "private.pem"), []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write pem: %v", err)
	}

	session := mustCodeResearchSession(t, CodeResearchConfig{
		Enabled:       true,
		Workdir:       workdir,
		MaxRounds:     1,
		MaxFileBytes:  1024,
		MaxTotalBytes: 4096,
	})

	for _, path := range []string{".env", "private.pem"} {
		result := session.readFile(path, 0, 20)
		if !strings.Contains(result, "ERROR") || !strings.Contains(result, "sensitive") {
			t.Fatalf("expected sensitive file rejection for %s, got %q", path, result)
		}
		if strings.Contains(result, "secret") {
			t.Fatalf("sensitive file content leaked for %s: %q", path, result)
		}
	}
}

func TestCodeResearchSearchFilesFindsGoKeyword(t *testing.T) {
	workdir := t.TempDir()
	content := "package sample\n\nfunc TargetFunction() string {\n\treturn \"ok\"\n}\n"
	if err := os.WriteFile(filepath.Join(workdir, "sample.go"), []byte(content), 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	session := mustCodeResearchSession(t, CodeResearchConfig{
		Enabled:       true,
		Workdir:       workdir,
		MaxRounds:     2,
		MaxFileBytes:  1024,
		MaxTotalBytes: 4096,
	})

	result := session.searchFiles("TargetFunction", "*.go", 10)
	if !strings.Contains(result, "sample.go:3") || !strings.Contains(result, "TargetFunction") {
		t.Fatalf("expected Go keyword match, got %q", result)
	}
}

func TestFusionCodeResearchPanelCanReadFileThenFinal(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "target.go"), []byte("package target\n\nfunc EvidenceFunction() {}\n"), 0o644); err != nil {
		t.Fatalf("write target file: %v", err)
	}

	upstream := newFakeUpstream([]fakeResponse{
		{Content: `{"tool":"read_file","args":{"path":"target.go","offset":1,"limit":20}}`},
		{Content: `{"final":"The code defines EvidenceFunction.\n\n证据文件:\n- target.go:3"}`},
		{Content: "Synthesized answer includes EvidenceFunction.\n\n证据文件:\n- target.go:3"},
	})
	defer upstream.Close()

	cfg := &Config{
		VirtualModel:   "local/fusion",
		TimeoutSeconds: 10,
		Debug:          DebugConfig{CaptureContent: true},
		Providers:      []Provider{{Name: "fake", BaseURL: upstream.URL}},
		Panel:          []PanelEntry{{Provider: "fake", Model: "panel"}},
		Synthesizer:    Synthesizer{Provider: "fake", Model: "synth"},
	}
	svc := NewFusionService(cfg)
	req := &ChatCompletionRequest{
		Model: "local/fusion",
		Messages: []Message{
			{Role: "user", Content: "Where is EvidenceFunction defined?"},
		},
		CodeResearch: &CodeResearchConfig{
			Enabled:       true,
			Workdir:       workdir,
			MaxRounds:     3,
			MaxFileBytes:  2000,
			MaxTotalBytes: 8000,
		},
	}

	resp, run, err := svc.ProcessWithRun(context.Background(), req, "test-run", time.Now())
	if err != nil {
		t.Fatalf("ProcessWithRun failed: %v", err)
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "证据文件") {
		t.Fatalf("expected final response to include evidence section, got %q", resp.Choices[0].Message.Content)
	}
	if len(run.Panel) != 1 || !strings.Contains(run.Panel[0].Content.Text, "证据文件") {
		t.Fatalf("expected debug panel answer to include evidence section, got %#v", run.Panel)
	}
	if !run.CodeResearch.Enabled || len(run.CodeResearch.Panels) != 1 {
		t.Fatalf("expected debug artifact to record code research panel metadata, got %#v", run.CodeResearch)
	}
	if len(run.CodeResearch.Panels[0].ToolCalls) != 1 || run.CodeResearch.Panels[0].ToolCalls[0].Tool != "read_file" {
		t.Fatalf("expected read_file tool call metadata, got %#v", run.CodeResearch.Panels[0].ToolCalls)
	}
	if len(run.CodeResearch.Panels[0].FilesRead) != 1 || run.CodeResearch.Panels[0].FilesRead[0] != "target.go" {
		t.Fatalf("expected target.go in files_read, got %#v", run.CodeResearch.Panels[0].FilesRead)
	}
}

func TestCodeResearchMaxTotalBytesApplies(t *testing.T) {
	workdir := t.TempDir()
	large := strings.Repeat("0123456789", 100)
	if err := os.WriteFile(filepath.Join(workdir, "large.go"), []byte("package large\n// "+large+"\n"), 0o644); err != nil {
		t.Fatalf("write large file: %v", err)
	}

	session := mustCodeResearchSession(t, CodeResearchConfig{
		Enabled:       true,
		Workdir:       workdir,
		MaxRounds:     1,
		MaxFileBytes:  2000,
		MaxTotalBytes: 120,
	})

	result := session.readFile("large.go", 0, 100)
	if len(result) > 180 {
		t.Fatalf("expected bounded output, got length %d: %q", len(result), result)
	}
	if !strings.Contains(result, "truncated") {
		t.Fatalf("expected truncation marker, got %q", result)
	}
	if session.totalBytes > session.config.MaxTotalBytes {
		t.Fatalf("expected tracked bytes <= max_total_bytes, got %d > %d", session.totalBytes, session.config.MaxTotalBytes)
	}
}

func mustCodeResearchSession(t *testing.T, cfg CodeResearchConfig) *codeResearchSession {
	t.Helper()
	session, err := newCodeResearchSession(cfg)
	if err != nil {
		t.Fatalf("newCodeResearchSession failed: %v", err)
	}
	return session
}

// Compile-time guard that fake upstream still accepts chat completion request JSON.
func TestCodeResearchRequestJSONShape(t *testing.T) {
	body, err := json.Marshal(ChatCompletionRequest{
		Model:        "local/fusion",
		Messages:     []Message{{Role: "user", Content: "hello"}},
		CodeResearch: &CodeResearchConfig{Enabled: true, Workdir: "/tmp"},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if !strings.Contains(string(body), "code_research") {
		t.Fatalf("expected code_research field in request JSON: %s", string(body))
	}
}

func newRecordingFakeUpstream(handler func(ChatCompletionRequest) fakeResponse) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp := handler(req)
		if resp.StatusCode != 0 && resp.StatusCode != http.StatusOK {
			w.WriteHeader(resp.StatusCode)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(upstreamResponse{Choices: []upstreamChoice{{Message: Message{Role: "assistant", Content: resp.Content}}}})
	}))
}

func TestCodeResearchReadFileRejectsSymlink(t *testing.T) {
	workdir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.go")
	if err := os.WriteFile(outside, []byte("package secret\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	link := filepath.Join(workdir, "link.go")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	session := mustCodeResearchSession(t, CodeResearchConfig{
		Enabled:       true,
		Workdir:       workdir,
		MaxRounds:     1,
		MaxFileBytes:  1024,
		MaxTotalBytes: 4096,
	})

	result := session.readFile("link.go", 0, 20)
	if !strings.Contains(result, "ERROR") || !strings.Contains(result, "symlink") {
		t.Fatalf("expected symlink rejection, got %q", result)
	}
	if strings.Contains(result, "package secret") || strings.Contains(result, "secret") {
		t.Fatalf("symlink target content leaked: %q", result)
	}
}

func TestRunFixedGitDiffTruncatesLargeOutput(t *testing.T) {
	workdir := t.TempDir()
	if err := exec.Command("git", "init", "-q", workdir).Run(); err != nil {
		t.Fatalf("git init failed: %v", err)
	}
	if err := exec.Command("git", "-C", workdir, "config", "user.email", "test@example.com").Run(); err != nil {
		t.Fatalf("git config email: %v", err)
	}
	if err := exec.Command("git", "-C", workdir, "config", "user.name", "Test").Run(); err != nil {
		t.Fatalf("git config name: %v", err)
	}

	content := strings.Repeat("a", 2000)
	if err := os.WriteFile(filepath.Join(workdir, "large.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("write large file: %v", err)
	}
	if err := exec.Command("git", "-C", workdir, "add", "large.txt").Run(); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := exec.Command("git", "-C", workdir, "commit", "-q", "-m", "initial").Run(); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "large.txt"), []byte(strings.Repeat("b", 2000)), 0o644); err != nil {
		t.Fatalf("modify large file: %v", err)
	}

	ctx := context.Background()
	result := runFixedGitDiff(ctx, workdir, 500)
	if len(result) > 700 {
		t.Fatalf("expected truncated output, got length %d: %q", len(result), result)
	}
	if !strings.Contains(result, "[truncated") {
		t.Fatalf("expected truncation marker, got %q", result)
	}
}
