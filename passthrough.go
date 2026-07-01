package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func shouldPassthroughForAgent(req *ChatCompletionRequest) bool {
	if req.Stream || len(req.Tools) > 0 || len(req.ToolChoice) > 0 {
		return true
	}
	for _, msg := range req.Messages {
		if msg.Role == "tool" || msg.ToolCallID != "" || len(msg.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

type passthroughTarget struct {
	Provider string
	Model    string
}

func (f *FusionService) defaultPassthroughTarget() passthroughTarget {
	return passthroughTarget{Provider: f.config.Synthesizer.Provider, Model: f.config.Synthesizer.Model}
}

func (f *FusionService) proxyChatCompletion(ctx context.Context, req *ChatCompletionRequest, w http.ResponseWriter) (int, error) {
	target := f.defaultPassthroughTarget()
	prov, ok := f.config.GetProvider(target.Provider)
	if !ok {
		return http.StatusInternalServerError, fmt.Errorf("passthrough provider %q not found", target.Provider)
	}

	body, err := buildPassthroughBody(req, target.Model)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	url := strings.TrimRight(prov.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, fmt.Errorf("create passthrough request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	apiKey := f.config.GetAPIKey(target.Provider)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := f.httpClient.Do(httpReq)
	if err != nil {
		return http.StatusBadGateway, fmt.Errorf("passthrough upstream request failed for %s/%s: %w", target.Provider, target.Model, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
		return http.StatusBadGateway, fmt.Errorf("passthrough upstream %s/%s returned status %d", target.Provider, target.Model, resp.StatusCode)
	}

	copyPassthroughHeaders(w.Header(), resp.Header, req.Stream)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		return resp.StatusCode, fmt.Errorf("copy passthrough response: %w", err)
	}
	return resp.StatusCode, nil
}

func buildPassthroughBody(req *ChatCompletionRequest, upstreamModel string) ([]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal passthrough request: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("remarshal passthrough request: %w", err)
	}
	putJSON(raw, "model", upstreamModel)
	delete(raw, "code_research")
	return json.Marshal(raw)
}

func copyPassthroughHeaders(dst http.Header, src http.Header, stream bool) {
	for key, values := range src {
		lower := strings.ToLower(key)
		switch lower {
		case "content-length", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	if stream {
		dst.Set("Content-Type", "text/event-stream")
		dst.Set("Cache-Control", "no-cache")
		dst.Set("X-Accel-Buffering", "no")
	} else if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
}
