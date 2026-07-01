package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultCodeResearchMaxRounds     = 4
	defaultCodeResearchMaxFileBytes  = 20_000
	defaultCodeResearchMaxTotalBytes = 80_000

	gitDiffTimeout  = 30 * time.Second
	maxGitDiffBytes = 2 * 1024 * 1024
)

type codeResearchSession struct {
	config     CodeResearchConfig
	totalBytes int
}

type codeResearchModelResponse struct {
	Tool  string                     `json:"tool,omitempty"`
	Args  map[string]json.RawMessage `json:"args,omitempty"`
	Final string                     `json:"final,omitempty"`
}

func newCodeResearchSession(config CodeResearchConfig) (*codeResearchSession, error) {
	normalized, err := normalizeCodeResearchConfig(config)
	if err != nil {
		return nil, err
	}
	return &codeResearchSession{config: normalized}, nil
}

func normalizeCodeResearchConfig(config CodeResearchConfig) (CodeResearchConfig, error) {
	if !config.Enabled {
		return config, nil
	}
	if config.Workdir == "" {
		return config, errors.New("code_research.workdir is required")
	}
	if !filepath.IsAbs(config.Workdir) {
		return config, fmt.Errorf("code_research.workdir must be an absolute path: %q", config.Workdir)
	}
	cleanWorkdir := filepath.Clean(config.Workdir)
	info, err := os.Stat(cleanWorkdir)
	if err != nil {
		return config, fmt.Errorf("code_research.workdir does not exist: %w", err)
	}
	if !info.IsDir() {
		return config, fmt.Errorf("code_research.workdir is not a directory: %q", cleanWorkdir)
	}

	config.Workdir = cleanWorkdir
	if config.MaxRounds <= 0 {
		config.MaxRounds = defaultCodeResearchMaxRounds
	}
	if config.MaxFileBytes <= 0 {
		config.MaxFileBytes = defaultCodeResearchMaxFileBytes
	}
	if config.MaxTotalBytes <= 0 {
		config.MaxTotalBytes = defaultCodeResearchMaxTotalBytes
	}
	return config, nil
}

func (s *codeResearchSession) listFiles(pattern string, limit int) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		pattern = "*"
	}
	limit = normalizeToolLimit(limit, 100, 500)

	matches := make([]string, 0, limit)
	_ = filepath.WalkDir(s.config.Workdir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == s.config.Workdir {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if shouldSkipDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= limit {
			return filepath.SkipAll
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(s.config.Workdir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if shouldSkipFile(rel) || !matchSimpleGlob(pattern, rel) {
			return nil
		}
		matches = append(matches, rel)
		return nil
	})

	sort.Strings(matches)
	if len(matches) == 0 {
		return s.applyOutputBudget("No files matched.\n")
	}
	return s.applyOutputBudget(strings.Join(matches, "\n") + "\n")
}

func (s *codeResearchSession) searchFiles(query string, fileGlob string, limit int) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return s.applyOutputBudget("ERROR: search_files query is required.\n")
	}
	fileGlob = strings.TrimSpace(fileGlob)
	if fileGlob == "" {
		fileGlob = "*"
	}
	limit = normalizeToolLimit(limit, 20, 200)

	results := make([]string, 0, limit)
	_ = filepath.WalkDir(s.config.Workdir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if path == s.config.Workdir {
			return nil
		}
		if entry.IsDir() {
			if shouldSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(results) >= limit {
			return filepath.SkipAll
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(s.config.Workdir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if shouldSkipFile(rel) || !matchSimpleGlob(fileGlob, rel) {
			return nil
		}
		fileResults := searchOneFile(path, rel, query, limit-len(results), s.config.MaxFileBytes)
		results = append(results, fileResults...)
		return nil
	})

	if len(results) == 0 {
		return s.applyOutputBudget("No matches found.\n")
	}
	return s.applyOutputBudget(strings.Join(results, "\n") + "\n")
}

func (s *codeResearchSession) readFile(path string, offset int, limit int) string {
	resolved, rel, err := s.resolveReadablePath(path)
	if err != nil {
		return s.applyOutputBudget("ERROR: " + err.Error() + "\n")
	}
	limit = normalizeToolLimit(limit, 120, 500)
	if offset <= 0 {
		offset = 1
	}

	file, err := os.Open(resolved)
	if err != nil {
		return s.applyOutputBudget("ERROR: read_file failed: " + err.Error() + "\n")
	}
	defer file.Close()

	limited := io.LimitReader(file, int64(s.config.MaxFileBytes)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return s.applyOutputBudget("ERROR: read_file failed: " + err.Error() + "\n")
	}
	truncatedByFileBytes := len(data) > s.config.MaxFileBytes
	if truncatedByFileBytes {
		data = data[:s.config.MaxFileBytes]
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return s.applyOutputBudget("ERROR: binary file content is not readable.\n")
	}

	lines := strings.Split(string(data), "\n")
	start := offset - 1
	if start >= len(lines) {
		return s.applyOutputBudget(fmt.Sprintf("File: %s\nNo lines at or after offset %d.\n", rel, offset))
	}
	end := min(start+limit, len(lines))
	var builder strings.Builder
	builder.WriteString("File: ")
	builder.WriteString(rel)
	builder.WriteByte('\n')
	for i := start; i < end; i++ {
		builder.WriteString(fmt.Sprintf("%d: %s\n", i+1, lines[i]))
	}
	if end < len(lines) || truncatedByFileBytes {
		builder.WriteString("[truncated: read_file limit reached]\n")
	}
	return s.applyOutputBudget(builder.String())
}

func (s *codeResearchSession) gitDiff() string {
	if !s.config.IncludeGitDiff {
		return s.applyOutputBudget("ERROR: git_diff is disabled for this request.\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitDiffTimeout)
	defer cancel()
	stat := runFixedGitDiff(ctx, s.config.Workdir, maxGitDiffBytes, "--stat")
	diff := runFixedGitDiff(ctx, s.config.Workdir, maxGitDiffBytes)
	text := "git diff --stat:\n" + stat + "\n\ngit diff:\n" + diff
	return s.applyOutputBudget(text)
}

func (s *codeResearchSession) resolveReadablePath(input string) (string, string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", errors.New("read_file path is required")
	}
	cleaned := filepath.Clean(input)
	var abs string
	if filepath.IsAbs(cleaned) {
		abs = cleaned
	} else {
		abs = filepath.Join(s.config.Workdir, cleaned)
	}
	abs = filepath.Clean(abs)
	if !isWithinDir(s.config.Workdir, abs) {
		return "", "", fmt.Errorf("read_file path is outside workdir: %q", input)
	}
	// Reject the target if the path itself is a symbolic link. This prevents
	// symlink-based escape attacks even if the link points inside the workdir.
	linfo, err := os.Lstat(abs)
	if err != nil {
		return "", "", fmt.Errorf("read_file stat failed: %w", err)
	}
	if linfo.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("read_file refused symlink: %q", input)
	}
	// Resolve any symlinks in the path and verify the canonical path is still
	// contained within the workdir.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", fmt.Errorf("read_file path resolution failed: %w", err)
	}
	resolved = filepath.Clean(resolved)
	if !isWithinDir(s.config.Workdir, resolved) {
		return "", "", fmt.Errorf("read_file path is outside workdir: %q", input)
	}
	rel, err := filepath.Rel(s.config.Workdir, resolved)
	if err != nil {
		return "", "", fmt.Errorf("read_file path is outside workdir: %q", input)
	}
	rel = filepath.ToSlash(rel)
	if shouldSkipFile(rel) {
		return "", "", fmt.Errorf("read_file refused sensitive or unsupported file: %q", rel)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", fmt.Errorf("read_file stat failed: %w", err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("read_file path is a directory: %q", rel)
	}
	return resolved, rel, nil
}

func (s *codeResearchSession) applyOutputBudget(text string) string {
	if s.config.MaxTotalBytes <= 0 {
		return text
	}
	remaining := s.config.MaxTotalBytes - s.totalBytes
	if remaining <= 0 {
		return "ERROR: max_total_bytes exceeded; tool output omitted.\n"
	}
	if len(text) <= remaining {
		s.totalBytes += len(text)
		return text
	}
	marker := "\n[truncated: max_total_bytes reached]\n"
	if remaining <= len(marker) {
		out := text[:remaining]
		s.totalBytes += len(out)
		return out
	}
	keep := remaining - len(marker)
	out := text[:keep] + marker
	s.totalBytes += len(out)
	return out
}

func normalizeToolLimit(value int, fallback int, maxAllowed int) int {
	if value <= 0 {
		return fallback
	}
	if value > maxAllowed {
		return maxAllowed
	}
	return value
}

func shouldSkipDir(name string) bool {
	switch strings.ToLower(name) {
	case ".git", "node_modules", "vendor", "dist", "build", ".next", "coverage":
		return true
	default:
		return false
	}
}

func shouldSkipFile(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	ext := strings.ToLower(filepath.Ext(base))
	if base == ".env" || strings.HasPrefix(base, ".env.") || base == "auth.json" || strings.Contains(base, "credentials") || strings.Contains(base, "secrets") {
		return true
	}
	switch ext {
	case ".pem", ".key", ".sqlite", ".db", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".pdf", ".zip", ".gz", ".tar", ".exe", ".dll", ".so", ".dylib", ".ico":
		return true
	default:
		return false
	}
}

func matchSimpleGlob(pattern string, rel string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" || pattern == "*" || pattern == "**" || pattern == "**/*" {
		return true
	}
	base := filepath.Base(rel)
	if ok, _ := filepath.Match(pattern, rel); ok {
		return true
	}
	if !strings.Contains(pattern, "/") {
		if ok, _ := filepath.Match(pattern, base); ok {
			return true
		}
	}
	if strings.HasPrefix(pattern, "**/") {
		trimmed := strings.TrimPrefix(pattern, "**/")
		if ok, _ := filepath.Match(trimmed, rel); ok {
			return true
		}
		if ok, _ := filepath.Match(trimmed, base); ok {
			return true
		}
	}
	return false
}

func searchOneFile(path string, rel string, query string, limit int, maxFileBytes int) []string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, int64(maxFileBytes)))
	if err != nil || bytes.IndexByte(data, 0) >= 0 {
		return nil
	}

	lines := strings.Split(string(data), "\n")
	results := make([]string, 0, min(limit, 8))
	for i, line := range lines {
		if len(results) >= limit {
			break
		}
		if strings.Contains(line, query) {
			snippet := strings.TrimSpace(line)
			if len(snippet) > 240 {
				snippet = snippet[:240] + "..."
			}
			results = append(results, fmt.Sprintf("%s:%d:%s", rel, i+1, snippet))
		}
	}
	return results
}

func isWithinDir(root string, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func runFixedGitDiff(ctx context.Context, workdir string, maxBytes int, args ...string) string {
	if maxBytes <= 0 {
		maxBytes = maxGitDiffBytes
	}
	gitArgs := append([]string{"-C", workdir, "diff", "--no-ext-diff"}, args...)
	ctx, cancel := context.WithTimeout(ctx, gitDiffTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", gitArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "ERROR: git diff failed: " + err.Error() + "\n"
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return "ERROR: git diff failed: " + err.Error() + "\n"
	}
	var buf strings.Builder
	_, copyErr := io.CopyN(&buf, stdout, int64(maxBytes)+1)
	if buf.Len() > maxBytes {
		cancel()
		_ = cmd.Wait()
		return buf.String()[:maxBytes] + "\n[truncated: git diff output exceeded max bytes]\n"
	}
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		_ = cmd.Wait()
		return "ERROR: git diff failed: " + copyErr.Error() + "\n"
	}
	if err := cmd.Wait(); err != nil {
		return "ERROR: git diff failed: " + err.Error() + "\n"
	}
	return buf.String()
}

func parseCodeResearchResponse(content string) (codeResearchModelResponse, bool) {
	trimmed := strings.TrimSpace(content)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	var response codeResearchModelResponse
	if err := json.Unmarshal([]byte(trimmed), &response); err != nil {
		return codeResearchModelResponse{}, false
	}
	return response, true
}

func toolStringArg(args map[string]json.RawMessage, key string) string {
	var value string
	if raw, ok := args[key]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return value
}

func toolIntArg(args map[string]json.RawMessage, key string) int {
	var value int
	if raw, ok := args[key]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return value
}
