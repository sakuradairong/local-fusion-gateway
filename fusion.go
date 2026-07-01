package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// FusionService orchestrates panel queries and synthesis.
type FusionService struct {
	config     *Config
	httpClient *http.Client
}

type upstreamCallResult struct {
	Provider   string
	Model      string
	StatusCode int
	Content    string
	Err        error
}

// NewFusionService creates a FusionService from a config.
func NewFusionService(cfg *Config) *FusionService {
	return &FusionService{
		config: cfg,
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
	}
}

// Process handles a chat completion request through the fusion pipeline.
func (f *FusionService) Process(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	resp, _, err := f.ProcessWithRun(ctx, req, newRunID(), time.Now())
	return resp, err
}

// ProcessWithRun handles a chat completion request and records sanitized debug metadata.
func (f *FusionService) ProcessWithRun(ctx context.Context, req *ChatCompletionRequest, runID string, started time.Time) (*ChatCompletionResponse, *DebugRun, error) {
	debugRun := newDebugRun(runID, started, f.config.Debug.CaptureContent)
	debugRun.Model = req.Model
	debugRun.Request = summarizeMessages(req.Messages, f.config.Debug.CaptureContent)

	if len(req.Messages) == 0 {
		return nil, debugRun, errors.New("messages array is empty")
	}

	var researchConfig *CodeResearchConfig
	if req.CodeResearch != nil && req.CodeResearch.Enabled {
		debugRun.CodeResearch = DebugCodeResearchRun{
			Enabled:        true,
			Workdir:        req.CodeResearch.Workdir,
			MaxRounds:      req.CodeResearch.MaxRounds,
			MaxFileBytes:   req.CodeResearch.MaxFileBytes,
			MaxTotalBytes:  req.CodeResearch.MaxTotalBytes,
			IncludeGitDiff: req.CodeResearch.IncludeGitDiff,
		}
		normalized, err := normalizeCodeResearchConfig(*req.CodeResearch)
		if err != nil {
			return nil, debugRun, err
		}
		researchConfig = &normalized
		debugRun.CodeResearch = DebugCodeResearchRun{
			Enabled:        true,
			Workdir:        normalized.Workdir,
			MaxRounds:      normalized.MaxRounds,
			MaxFileBytes:   normalized.MaxFileBytes,
			MaxTotalBytes:  normalized.MaxTotalBytes,
			IncludeGitDiff: normalized.IncludeGitDiff,
		}
	}

	// Phase 1: Query all panel models in parallel.
	results := f.queryPanel(ctx, req, researchConfig)
	debugRun.Panel = summarizePanelResults(results, f.config.Debug.CaptureContent)
	if researchConfig != nil {
		debugRun.CodeResearch.Panels = summarizeCodeResearchPanels(results)
	}

	// Collect successful results.
	var successes []panelResult
	var failures []panelResult
	for _, r := range results {
		if r.Err != nil {
			failures = append(failures, r)
		} else {
			successes = append(successes, r)
		}
	}

	// At least one panel must succeed.
	if len(successes) == 0 {
		errMsg := "all panel models failed"
		if len(failures) > 0 {
			parts := make([]string, len(failures))
			for i, failure := range failures {
				parts[i] = fmt.Sprintf("%s/%s: %v", failure.Provider, failure.Model, failure.Err)
			}
			errMsg = "all panel models failed: " + strings.Join(parts, "; ")
		}
		return nil, debugRun, errors.New(errMsg)
	}

	log.Printf("panel results: %d succeeded, %d failed", len(successes), len(failures))

	// Phase 2: Build synthesizer prompt and call synthesizer.
	synthResp, synthSummary, err := f.synthesize(ctx, req, successes)
	debugRun.Synthesizer = synthSummary
	if err != nil {
		return nil, debugRun, fmt.Errorf("synthesis failed: %w", err)
	}

	synthResp.ID = runID
	synthResp.Model = req.Model
	if len(synthResp.Choices) > 0 {
		debugRun.Response = summarizeText(synthResp.Choices[0].Message.Content, f.config.Debug.CaptureContent)
	}

	return synthResp, debugRun, nil
}

// queryPanel fans out the request to all panel models in parallel.
func (f *FusionService) queryPanel(ctx context.Context, req *ChatCompletionRequest, researchConfig *CodeResearchConfig) []panelResult {
	results := make([]panelResult, len(f.config.Panel))
	var wg sync.WaitGroup

	for i, pe := range f.config.Panel {
		wg.Add(1)
		go func(idx int, entry PanelEntry) {
			defer wg.Done()
			var result panelResult
			if researchConfig != nil {
				result = f.queryPanelWithCodeResearch(ctx, entry, req.Messages, req.Extra, *researchConfig)
			} else {
				callResult := f.callUpstream(ctx, entry.Provider, entry.Model, req.Messages, req.Extra)
				result = panelResult{
					Provider:   callResult.Provider,
					Model:      callResult.Model,
					StatusCode: callResult.StatusCode,
					Content:    callResult.Content,
					Err:        callResult.Err,
				}
			}
			results[idx] = result
		}(i, pe)
	}

	wg.Wait()
	return results
}

// callUpstream sends a chat completion request to a single upstream provider.
func (f *FusionService) callUpstream(ctx context.Context, providerName, model string, messages []Message, extra map[string]json.RawMessage) upstreamCallResult {
	result := upstreamCallResult{Provider: providerName, Model: model}

	prov, ok := f.config.GetProvider(providerName)
	if !ok {
		result.Err = fmt.Errorf("unknown provider %q", providerName)
		return result
	}

	payload := ChatCompletionRequest{
		Extra:    extra,
		Model:    model,
		Messages: messages,
		Stream:   false,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		result.Err = fmt.Errorf("marshal request: %w", err)
		return result
	}

	url := strings.TrimRight(prov.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		result.Err = fmt.Errorf("create request: %w", err)
		return result
	}
	httpReq.Header.Set("Content-Type", "application/json")

	apiKey := f.config.GetAPIKey(providerName)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := f.httpClient.Do(httpReq)
	if err != nil {
		result.Err = fmt.Errorf("upstream request failed for %s/%s: %w", providerName, model, err)
		return result
	}
	defer resp.Body.Close()
	result.StatusCode = resp.StatusCode

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
		result.Err = fmt.Errorf("upstream %s/%s returned status %d", providerName, model, resp.StatusCode)
		return result
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		result.Err = fmt.Errorf("read upstream response for %s/%s: %w", providerName, model, err)
		return result
	}

	var upstream upstreamResponse
	if err := json.Unmarshal(respBody, &upstream); err != nil {
		result.Err = fmt.Errorf("parse upstream response for %s/%s: %w", providerName, model, err)
		return result
	}

	if len(upstream.Choices) == 0 {
		result.Err = fmt.Errorf("upstream %s/%s returned no choices", providerName, model)
		return result
	}

	result.Content = upstream.Choices[0].Message.Content
	return result
}

// synthesize builds the synthesizer prompt and calls the synthesizer model.
func (f *FusionService) synthesize(ctx context.Context, originalReq *ChatCompletionRequest, panelResults []panelResult) (*ChatCompletionResponse, DebugUpstreamSummary, error) {
	synthPrompt := buildSynthesizerPrompt(originalReq.Messages, panelResults)

	synthMessages := []Message{
		{
			Role:    "user",
			Content: synthPrompt,
		},
	}

	callResult := f.callUpstream(ctx, f.config.Synthesizer.Provider, f.config.Synthesizer.Model, synthMessages, originalReq.Extra)
	summary := DebugUpstreamSummary{
		Provider: callResult.Provider,
		Model:    callResult.Model,
		Status:   callResult.StatusCode,
		Success:  callResult.Err == nil,
		Prompt:   summarizeText(synthPrompt, f.config.Debug.CaptureContent),
		Content:  summarizeText(callResult.Content, f.config.Debug.CaptureContent),
	}
	if callResult.Err != nil {
		summary.Error = callResult.Err.Error()
		return nil, summary, fmt.Errorf("synthesizer call failed: %w", callResult.Err)
	}

	return &ChatCompletionResponse{
		ID:      "",
		Object:  "chat.completion",
		Created: nowUnix(),
		Model:   f.config.VirtualModel,
		Choices: []Choice{
			{
				Index:        0,
				Message:      Message{Role: "assistant", Content: callResult.Content},
				FinishReason: "stop",
			},
		},
		Usage: &Usage{
			PromptTokens:     estimateTokens(synthPrompt),
			CompletionTokens: estimateTokens(callResult.Content),
			TotalTokens:      estimateTokens(synthPrompt) + estimateTokens(callResult.Content),
		},
	}, summary, nil
}

// buildSynthesizerPrompt creates the prompt that instructs the synthesizer model.
func buildSynthesizerPrompt(userMessages []Message, panelResults []panelResult) string {
	var sb strings.Builder

	sb.WriteString("You are a synthesis engine. Analyze the following responses from multiple AI models to the user's question, then provide a synthesized answer.\n\n")

	// Include original user messages.
	sb.WriteString("USER'S QUESTION:\n")
	for _, msg := range userMessages {
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", msg.Role, msg.ContentString()))
	}

	sb.WriteString("\nPANEL MODEL RESPONSES:\n")
	for i, r := range panelResults {
		sb.WriteString(fmt.Sprintf("--- Model %d: %s/%s ---\n", i+1, r.Provider, r.Model))
		sb.WriteString(r.Content)
		sb.WriteString("\n\n")
	}

	sb.WriteString("INSTRUCTIONS:\n")
	sb.WriteString("Provide a synthesized answer that covers the following four sections:\n\n")
	sb.WriteString("1. **Consensus (共识)**: What points do all models agree on?\n")
	sb.WriteString("2. **Conflicts (冲突)**: Where do models disagree or contradict each other?\n")
	sb.WriteString("3. **Omissions (遗漏)**: What important points or perspectives were missed by some models?\n")
	sb.WriteString("4. **Final Answer (最终答案)**: The best synthesized response that incorporates consensus points, resolves conflicts, and fills in omissions.\n\n")
	sb.WriteString("Please structure your response with these four sections clearly labeled.")

	return sb.String()
}
