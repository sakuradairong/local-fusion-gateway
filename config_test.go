package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Create a temporary config file.
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	content := `
listen: ":9090"
virtual_model: "my-fusion/v1"
timeout_seconds: 60

providers:
  - name: "test-provider"
    base_url: "https://api.example.com/v1"
    api_key_env: "TEST_API_KEY"

  - name: "local"
    base_url: "http://localhost:11434/v1"
    api_key_env: ""

panel:
  - provider: "test-provider"
    model: "model-a"
  - provider: "test-provider"
    model: "model-b"

synthesizer:
  provider: "test-provider"
  model: "model-synth"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Listen != ":9090" {
		t.Errorf("expected listen :9090, got %q", cfg.Listen)
	}
	if cfg.VirtualModel != "my-fusion/v1" {
		t.Errorf("expected virtual_model my-fusion/v1, got %q", cfg.VirtualModel)
	}
	if cfg.TimeoutSeconds != 60 {
		t.Errorf("expected timeout_seconds 60, got %d", cfg.TimeoutSeconds)
	}
	if len(cfg.Providers) != 2 {
		t.Errorf("expected 2 providers, got %d", len(cfg.Providers))
	}
	if len(cfg.Panel) != 2 {
		t.Errorf("expected 2 panel entries, got %d", len(cfg.Panel))
	}
	if cfg.Synthesizer.Provider != "test-provider" {
		t.Errorf("expected synthesizer provider test-provider, got %q", cfg.Synthesizer.Provider)
	}
	if cfg.Synthesizer.Model != "model-synth" {
		t.Errorf("expected synthesizer model model-synth, got %q", cfg.Synthesizer.Model)
	}

	// Validate should pass.
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate should pass: %v", err)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	content := `
providers:
  - name: "p1"
    base_url: "http://localhost:8080/v1"

panel:
  - provider: "p1"
    model: "m1"

synthesizer:
  provider: "p1"
  model: "s1"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.Listen != ":8080" {
		t.Errorf("expected default listen :8080, got %q", cfg.Listen)
	}
	if cfg.VirtualModel != "local/fusion" {
		t.Errorf("expected default virtual_model local/fusion, got %q", cfg.VirtualModel)
	}
	if cfg.TimeoutSeconds != 120 {
		t.Errorf("expected default timeout_seconds 120, got %d", cfg.TimeoutSeconds)
	}
	if cfg.MaxBodyBytes != defaultMaxBodyBytes {
		t.Errorf("expected default max_body_bytes %d, got %d", defaultMaxBodyBytes, cfg.MaxBodyBytes)
	}
	if cfg.Debug.Enabled {
		t.Error("expected debug to be disabled by default")
	}
	if cfg.Debug.Dir != "debug" {
		t.Errorf("expected default debug dir debug, got %q", cfg.Debug.Dir)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	if err := os.WriteFile(configPath, []byte(": invalid yaml: ["), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestLoadConfigNoPanel(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	content := `
providers:
  - name: "p1"
    base_url: "http://localhost:8080/v1"

panel: []

synthesizer:
  provider: "p1"
  model: "s1"
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Error("expected error for empty panel")
	}
}

func TestConfigGetAPIKey(t *testing.T) {
	os.Setenv("MY_SECRET_KEY", "sk-test-123")
	defer os.Unsetenv("MY_SECRET_KEY")

	cfg := &Config{
		Providers: []Provider{
			{Name: "p1", APIKeyEnv: "MY_SECRET_KEY"},
			{Name: "p2", APIKeyEnv: ""},
		},
	}

	if key := cfg.GetAPIKey("p1"); key != "sk-test-123" {
		t.Errorf("expected sk-test-123, got %q", key)
	}
	if key := cfg.GetAPIKey("p2"); key != "" {
		t.Errorf("expected empty key, got %q", key)
	}
	if key := cfg.GetAPIKey("unknown"); key != "" {
		t.Errorf("expected empty key for unknown provider, got %q", key)
	}
}

func TestConfigValidateUnknownProviderInPanel(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{
			{Name: "p1", BaseURL: "http://localhost:8080/v1"},
		},
		Panel: []PanelEntry{
			{Provider: "unknown", Model: "m1"},
		},
		Synthesizer: Synthesizer{Provider: "p1", Model: "s1"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for unknown panel provider")
	}
}

func TestConfigValidateUnknownProviderInSynthesizer(t *testing.T) {
	cfg := &Config{
		Providers: []Provider{
			{Name: "p1", BaseURL: "http://localhost:8080/v1"},
		},
		Panel: []PanelEntry{
			{Provider: "p1", Model: "m1"},
		},
		Synthesizer: Synthesizer{Provider: "unknown", Model: "s1"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for unknown synthesizer provider")
	}
}

func TestConfigValidateAgentProfile(t *testing.T) {
	base := Config{
		Providers:   []Provider{{Name: "p1", BaseURL: "http://localhost:8080/v1"}},
		Panel:       []PanelEntry{{Provider: "p1", Model: "m1"}},
		Synthesizer: Synthesizer{Provider: "p1", Model: "s1"},
	}

	valid := base
	valid.AgentProfiles.Pi = AgentProfile{Provider: "p1", Model: "agent-model", Mode: "passthrough"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected valid pi agent profile, got %v", err)
	}

	missingModel := base
	missingModel.AgentProfiles.Pi = AgentProfile{Provider: "p1", Mode: "passthrough"}
	if err := missingModel.Validate(); err == nil {
		t.Fatal("expected error for missing pi profile model")
	}

	unknownProvider := base
	unknownProvider.AgentProfiles.Pi = AgentProfile{Provider: "missing", Model: "agent-model"}
	if err := unknownProvider.Validate(); err == nil {
		t.Fatal("expected error for unknown pi profile provider")
	}

	badMode := base
	badMode.AgentProfiles.Pi = AgentProfile{Provider: "p1", Model: "agent-model", Mode: "fusion"}
	if err := badMode.Validate(); err == nil {
		t.Fatal("expected error for unsupported pi profile mode")
	}
}
