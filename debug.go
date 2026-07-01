package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DebugRun is the optional sanitized debug artifact written per chat request.
type DebugRun struct {
	RunID        string                 `json:"run_id"`
	StartedAt    string                 `json:"started_at"`
	Model        string                 `json:"model,omitempty"`
	Request      DebugContentSummary    `json:"request"`
	Panel        []DebugUpstreamSummary `json:"panel"`
	CodeResearch DebugCodeResearchRun   `json:"code_research"`
	Synthesizer  DebugUpstreamSummary   `json:"synthesizer"`
	DurationMS   int64                  `json:"duration_ms"`
	HTTPStatus   int                    `json:"http_status"`
	FinalStatus  string                 `json:"final_status"`
	Response     DebugContentSummary    `json:"response"`
}

// DebugUpstreamSummary contains provider/model/status metadata without credentials.
type DebugUpstreamSummary struct {
	Provider string              `json:"provider,omitempty"`
	Model    string              `json:"model,omitempty"`
	Status   int                 `json:"status,omitempty"`
	Success  bool                `json:"success"`
	Error    string              `json:"error,omitempty"`
	Prompt   DebugContentSummary `json:"prompt,omitempty"`
	Content  DebugContentSummary `json:"content,omitempty"`
}

// DebugCodeResearchRun contains sanitized metadata for code research mode.
type DebugCodeResearchRun struct {
	Enabled        bool                     `json:"enabled"`
	Workdir        string                   `json:"workdir,omitempty"`
	MaxRounds      int                      `json:"max_rounds,omitempty"`
	MaxFileBytes   int                      `json:"max_file_bytes,omitempty"`
	MaxTotalBytes  int                      `json:"max_total_bytes,omitempty"`
	IncludeGitDiff bool                     `json:"include_git_diff,omitempty"`
	Panels         []DebugCodeResearchPanel `json:"panels,omitempty"`
}

// DebugCodeResearchPanel contains per-panel code research metadata.
type DebugCodeResearchPanel struct {
	Provider  string                    `json:"provider,omitempty"`
	Model     string                    `json:"model,omitempty"`
	ToolCalls []DebugCodeResearchTool   `json:"tool_calls,omitempty"`
	FilesRead []string                  `json:"files_read,omitempty"`
	Searches  []DebugCodeResearchSearch `json:"searches,omitempty"`
}

// DebugCodeResearchSearch records a search query without file contents.
type DebugCodeResearchSearch struct {
	Query    string `json:"query"`
	FileGlob string `json:"file_glob,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// DebugCodeResearchTool records a single tool invocation.
type DebugCodeResearchTool struct {
	Tool   string              `json:"tool"`
	Args   map[string]any      `json:"args,omitempty"`
	Result DebugContentSummary `json:"result,omitempty"`
	Error  string              `json:"error,omitempty"`
}

// DebugContentSummary stores size/hash by default, and content only when explicitly enabled.
type DebugContentSummary struct {
	Length int    `json:"length"`
	SHA256 string `json:"sha256,omitempty"`
	Text   string `json:"text,omitempty"`
}

func newDebugRun(runID string, started time.Time, captureContent bool) *DebugRun {
	return &DebugRun{
		RunID:        runID,
		StartedAt:    started.UTC().Format(time.RFC3339Nano),
		Request:      summarizeText("", captureContent),
		Panel:        []DebugUpstreamSummary{},
		CodeResearch: DebugCodeResearchRun{Enabled: false},
		Synthesizer:  DebugUpstreamSummary{},
		Response:     summarizeText("", captureContent),
	}
}

func summarizeMessages(messages []Message, captureContent bool) DebugContentSummary {
	data, err := json.Marshal(messages)
	if err != nil {
		return summarizeText("", false)
	}
	return summarizeText(string(data), captureContent)
}

func summarizeText(text string, captureContent bool) DebugContentSummary {
	sum := sha256.Sum256([]byte(text))
	summary := DebugContentSummary{
		Length: len(text),
		SHA256: hex.EncodeToString(sum[:]),
	}
	if captureContent {
		summary.Text = text
	}
	return summary
}

func summarizePanelResults(results []panelResult, captureContent bool) []DebugUpstreamSummary {
	summaries := make([]DebugUpstreamSummary, 0, len(results))
	for _, result := range results {
		summaries = append(summaries, summarizePanelResult(result, captureContent))
	}
	return summaries
}

func summarizePanelResult(result panelResult, captureContent bool) DebugUpstreamSummary {
	summary := DebugUpstreamSummary{
		Provider: result.Provider,
		Model:    result.Model,
		Status:   result.StatusCode,
		Success:  result.Err == nil,
		Content:  summarizeText(result.Content, captureContent),
	}
	if result.Err != nil {
		summary.Error = result.Err.Error()
	}
	return summary
}

func (f *FusionService) writeDebugRun(run *DebugRun) error {
	if !f.config.Debug.Enabled || run == nil {
		return nil
	}

	dir := f.config.Debug.Dir
	if dir == "" {
		dir = "debug"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create debug directory: %w", err)
	}

	fileName := safeDebugFileName(run.RunID) + ".json"
	path := filepath.Join(dir, fileName)
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal debug artifact: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write debug artifact: %w", err)
	}
	return nil
}

func safeDebugFileName(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			builder.WriteRune(r)
		}
	}
	if builder.Len() == 0 {
		return "fusion-debug"
	}
	return builder.String()
}
