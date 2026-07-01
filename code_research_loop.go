package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (f *FusionService) queryPanelWithCodeResearch(ctx context.Context, entry PanelEntry, originalMessages []Message, extra map[string]json.RawMessage, config CodeResearchConfig) panelResult {
	debugPanel := DebugCodeResearchPanel{Provider: entry.Provider, Model: entry.Model}
	session, err := newCodeResearchSession(config)
	if err != nil {
		return panelResult{Provider: entry.Provider, Model: entry.Model, Err: err, CodeResearch: &debugPanel}
	}

	researchMessages := buildCodeResearchMessages(originalMessages, session.config)
	if session.config.IncludeGitDiff {
		result := session.gitDiff()
		debugPanel.ToolCalls = append(debugPanel.ToolCalls, DebugCodeResearchTool{
			Tool:   "git_diff",
			Result: summarizeText(result, f.config.Debug.CaptureContent),
		})
		researchMessages = append(researchMessages, Message{Role: "user", Content: formatToolResult("git_diff", result)})
	}

	var lastStatus int
	for round := 1; round <= session.config.MaxRounds; round++ {
		callResult := f.callUpstream(ctx, entry.Provider, entry.Model, researchMessages, extra)
		lastStatus = callResult.StatusCode
		if callResult.Err != nil {
			return panelResult{
				Provider:     callResult.Provider,
				Model:        callResult.Model,
				StatusCode:   callResult.StatusCode,
				Content:      callResult.Content,
				Err:          callResult.Err,
				CodeResearch: &debugPanel,
			}
		}

		parsed, ok := parseCodeResearchResponse(callResult.Content)
		if !ok {
			return panelResult{Provider: callResult.Provider, Model: callResult.Model, StatusCode: callResult.StatusCode, Content: callResult.Content, CodeResearch: &debugPanel}
		}
		if strings.TrimSpace(parsed.Final) != "" {
			return panelResult{Provider: callResult.Provider, Model: callResult.Model, StatusCode: callResult.StatusCode, Content: parsed.Final, CodeResearch: &debugPanel}
		}
		if strings.TrimSpace(parsed.Tool) == "" {
			return panelResult{Provider: callResult.Provider, Model: callResult.Model, StatusCode: callResult.StatusCode, Content: callResult.Content, CodeResearch: &debugPanel}
		}

		toolResult, toolDebug := executeCodeResearchTool(session, parsed.Tool, parsed.Args, f.config.Debug.CaptureContent)
		debugPanel.ToolCalls = append(debugPanel.ToolCalls, toolDebug)
		updateCodeResearchDebugPanel(&debugPanel, parsed.Tool, parsed.Args)
		researchMessages = append(researchMessages,
			Message{Role: "assistant", Content: callResult.Content},
			Message{Role: "user", Content: formatToolResult(parsed.Tool, toolResult)},
		)
	}

	finalMessages := append([]Message{}, researchMessages...)
	finalMessages = append(finalMessages, Message{
		Role:    "user",
		Content: "Maximum research rounds reached. Do not call more tools. Based only on the tool evidence already provided and the user's question, return JSON exactly in this shape: {\"final\":\"your answer with a 证据文件 section listing cited file paths and line numbers\"}.",
	})
	callResult := f.callUpstream(ctx, entry.Provider, entry.Model, finalMessages, extra)
	if callResult.StatusCode != 0 {
		lastStatus = callResult.StatusCode
	}
	if callResult.Err != nil {
		return panelResult{Provider: entry.Provider, Model: entry.Model, StatusCode: lastStatus, Content: callResult.Content, Err: callResult.Err, CodeResearch: &debugPanel}
	}
	if parsed, ok := parseCodeResearchResponse(callResult.Content); ok && strings.TrimSpace(parsed.Final) != "" {
		return panelResult{Provider: callResult.Provider, Model: callResult.Model, StatusCode: callResult.StatusCode, Content: parsed.Final, CodeResearch: &debugPanel}
	}
	return panelResult{Provider: callResult.Provider, Model: callResult.Model, StatusCode: callResult.StatusCode, Content: callResult.Content, CodeResearch: &debugPanel}
}

func buildCodeResearchMessages(originalMessages []Message, config CodeResearchConfig) []Message {
	var prompt strings.Builder
	prompt.WriteString("You are answering a user question about a local codebase. You cannot access files directly. The gateway can run only these controlled read-only tools inside the configured workdir.\n\n")
	prompt.WriteString("STRICT RULES:\n")
	prompt.WriteString("- Answer only from the user's question and evidence returned by tools.\n")
	prompt.WriteString("- Do not claim you read files that were not returned by read_file/search_files/list_files/git_diff.\n")
	prompt.WriteString("- Do not request shell, write files, network access, or tools outside the list below.\n")
	prompt.WriteString("- Final answers must include a section named \"证据文件\" listing cited file paths and line numbers.\n\n")
	prompt.WriteString("TOOLS (return one JSON object only, no markdown):\n")
	prompt.WriteString("- {\"tool\":\"list_files\",\"args\":{\"pattern\":\"*.go\",\"limit\":50}}\n")
	prompt.WriteString("- {\"tool\":\"search_files\",\"args\":{\"query\":\"keyword\",\"file_glob\":\"*.go\",\"limit\":20}}\n")
	prompt.WriteString("- {\"tool\":\"read_file\",\"args\":{\"path\":\"relative/path.go\",\"offset\":1,\"limit\":120}}\n")
	if config.IncludeGitDiff {
		prompt.WriteString("- {\"tool\":\"git_diff\",\"args\":{}}\n")
	}
	prompt.WriteString("- When ready: {\"final\":\"answer text including 证据文件\"}\n\n")
	prompt.WriteString(fmt.Sprintf("WORKDIR: %s\n", config.Workdir))
	prompt.WriteString(fmt.Sprintf("LIMITS: max_rounds=%d max_file_bytes=%d max_total_bytes=%d\n\n", config.MaxRounds, config.MaxFileBytes, config.MaxTotalBytes))
	prompt.WriteString("USER CONVERSATION:\n")
	for _, msg := range originalMessages {
		prompt.WriteString(fmt.Sprintf("[%s]: %s\n", msg.Role, msg.ContentString()))
	}

	return []Message{{Role: "user", Content: prompt.String()}}
}

func executeCodeResearchTool(session *codeResearchSession, tool string, args map[string]json.RawMessage, captureContent bool) (string, DebugCodeResearchTool) {
	tool = strings.TrimSpace(tool)
	debug := DebugCodeResearchTool{Tool: tool, Args: sanitizeToolArgs(tool, args)}
	var result string
	switch tool {
	case "list_files":
		result = session.listFiles(toolStringArg(args, "pattern"), toolIntArg(args, "limit"))
	case "search_files":
		result = session.searchFiles(toolStringArg(args, "query"), toolStringArg(args, "file_glob"), toolIntArg(args, "limit"))
	case "read_file":
		result = session.readFile(toolStringArg(args, "path"), toolIntArg(args, "offset"), toolIntArg(args, "limit"))
	case "git_diff":
		result = session.gitDiff()
	default:
		result = session.applyOutputBudget("ERROR: unsupported code research tool: " + tool + "\n")
	}
	debug.Result = summarizeText(result, captureContent)
	if strings.HasPrefix(result, "ERROR:") {
		debug.Error = strings.TrimSpace(result)
	}
	return result, debug
}

func sanitizeToolArgs(tool string, args map[string]json.RawMessage) map[string]any {
	sanitized := make(map[string]any)
	switch tool {
	case "list_files":
		sanitized["pattern"] = toolStringArg(args, "pattern")
		sanitized["limit"] = toolIntArg(args, "limit")
	case "search_files":
		sanitized["query"] = toolStringArg(args, "query")
		sanitized["file_glob"] = toolStringArg(args, "file_glob")
		sanitized["limit"] = toolIntArg(args, "limit")
	case "read_file":
		sanitized["path"] = toolStringArg(args, "path")
		sanitized["offset"] = toolIntArg(args, "offset")
		sanitized["limit"] = toolIntArg(args, "limit")
	case "git_diff":
		return sanitized
	}
	return sanitized
}

func updateCodeResearchDebugPanel(panel *DebugCodeResearchPanel, tool string, args map[string]json.RawMessage) {
	switch tool {
	case "read_file":
		path := filepathCleanSlash(toolStringArg(args, "path"))
		if path != "" {
			panel.FilesRead = appendUniqueString(panel.FilesRead, path)
		}
	case "search_files":
		panel.Searches = append(panel.Searches, DebugCodeResearchSearch{
			Query:    toolStringArg(args, "query"),
			FileGlob: toolStringArg(args, "file_glob"),
			Limit:    toolIntArg(args, "limit"),
		})
	}
}

func summarizeCodeResearchPanels(results []panelResult) []DebugCodeResearchPanel {
	panels := make([]DebugCodeResearchPanel, 0, len(results))
	for _, result := range results {
		if result.CodeResearch != nil {
			panels = append(panels, *result.CodeResearch)
		}
	}
	return panels
}

func formatToolResult(tool string, result string) string {
	return fmt.Sprintf("TOOL RESULT for %s:\n%s\nNext, return another tool-call JSON if more evidence is needed, or {\"final\":\"...\"}. Remember: final must include 证据文件 with cited file paths and line numbers.", tool, result)
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func filepathCleanSlash(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return strings.ReplaceAll(strings.TrimPrefix(strings.TrimPrefix(path, "./"), "/"), "\\", "/")
}
