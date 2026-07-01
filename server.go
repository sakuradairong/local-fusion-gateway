package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Server holds the HTTP server state.
type Server struct {
	config    *Config
	fusionSvc *FusionService
}

// NewServer creates a new Server instance.
func NewServer(cfg *Config) *Server {
	normalized := cfg.WithDefaults()
	return &Server{
		config:    &normalized,
		fusionSvc: NewFusionService(&normalized),
	}
}

// Handler returns an http.Handler configured with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	return mux
}

// handleHealth responds with a simple health check.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// modelEntry is a single model in the model list response.
type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// modelList is the response for GET /v1/models.
type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// handleModels returns the list of available models.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	requestID := newRunID()
	w.Header().Set("X-Request-ID", requestID)
	if !s.authorize(w, r, requestID) {
		return
	}

	models := []modelEntry{
		{ID: s.config.VirtualModel, Object: "model", OwnedBy: "local-fusion"},
	}

	// Add panel models.
	seen := make(map[string]bool)
	for _, pe := range s.config.Panel {
		id := fmt.Sprintf("%s/%s", pe.Provider, pe.Model)
		if !seen[id] {
			seen[id] = true
			models = append(models, modelEntry{ID: id, Object: "model", OwnedBy: pe.Provider})
		}
	}

	// Add synthesizer model.
	synthID := fmt.Sprintf("%s/%s", s.config.Synthesizer.Provider, s.config.Synthesizer.Model)
	if !seen[synthID] {
		models = append(models, modelEntry{ID: synthID, Object: "model", OwnedBy: s.config.Synthesizer.Provider})
	}

	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: models})
}

// handleChatCompletions processes a chat completion request.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	runID := newRunID()
	started := time.Now()
	status := http.StatusInternalServerError
	debugRun := newDebugRun(runID, started, s.config.Debug.CaptureContent)

	w.Header().Set("X-Request-ID", runID)
	defer func() {
		debugRun.DurationMS = time.Since(started).Milliseconds()
		debugRun.HTTPStatus = status
		debugRun.FinalStatus = finalStatus(status)
		if err := s.fusionSvc.writeDebugRun(debugRun); err != nil {
			log.Printf("debug artifact write failed for request %s: %v", runID, err)
		}
	}()

	if !s.authorize(w, r, runID) {
		status = http.StatusUnauthorized
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.config.MaxBodyBytes)
	defer r.Body.Close()

	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			status = http.StatusRequestEntityTooLarge
			writeError(w, status, "request body too large", runID)
			return
		}
		status = http.StatusBadRequest
		writeError(w, status, "invalid request body: "+err.Error(), runID)
		return
	}
	debugRun.Model = req.Model
	debugRun.Request = summarizeMessages(req.Messages, s.config.Debug.CaptureContent)

	if shouldPassthroughForAgent(&req) {
		statusCode, err := s.fusionSvc.proxyChatCompletion(r.Context(), &req, w)
		status = statusCode
		if err != nil {
			log.Printf("passthrough error for request %s: %v", runID, err)
			writeError(w, status, err.Error(), runID)
		}
		return
	}

	resp, run, err := s.fusionSvc.ProcessWithRun(r.Context(), &req, runID, started)
	if run != nil {
		debugRun = run
	}
	if err != nil {
		log.Printf("fusion error for request %s: %v", runID, err)
		status = http.StatusInternalServerError
		writeError(w, status, err.Error(), runID)
		return
	}

	status = http.StatusOK
	writeJSON(w, status, resp)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, requestID string) bool {
	if s.config.AuthTokenEnv == "" {
		return true
	}

	expected := s.config.GetAuthToken()
	if expected == "" {
		writeErrorWithType(w, http.StatusUnauthorized, "authentication token is not configured", "authentication_error", requestID)
		return false
	}

	provided, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		writeErrorWithType(w, http.StatusUnauthorized, "invalid authentication token", "authentication_error", requestID)
		return false
	}
	return true
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return token, token != ""
}

func finalStatus(status int) string {
	if status >= 200 && status < 400 {
		return "ok"
	}
	return "error"
}

func newRunID() string {
	var randomBytes [6]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return fmt.Sprintf("fusion-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("fusion-%d-%s", time.Now().UnixNano(), hex.EncodeToString(randomBytes[:]))
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON encode error: %v", err)
	}
}

// writeError writes an OpenAI-compatible error response.
func writeError(w http.ResponseWriter, status int, message string, requestID string) {
	writeErrorWithType(w, status, message, "invalid_request_error", requestID)
}

func writeErrorWithType(w http.ResponseWriter, status int, message string, errorType string, requestID string) {
	writeJSON(w, status, ErrorResponse{
		Error: ErrorDetail{
			Message:   message,
			Type:      errorType,
			RequestID: requestID,
		},
	})
}
