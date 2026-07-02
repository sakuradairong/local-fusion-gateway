package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultMaxBodyBytes int64 = 2 * 1024 * 1024

// Config holds all configuration for the fusion gateway.
type Config struct {
	Listen         string                  `yaml:"listen"`
	VirtualModel   string                  `yaml:"virtual_model"`
	TimeoutSeconds int                     `yaml:"timeout_seconds"`
	AuthTokenEnv   string                  `yaml:"auth_token_env"`
	MaxBodyBytes   int64                   `yaml:"max_body_bytes"`
	Debug          DebugConfig             `yaml:"debug"`
	AgentProfiles  AgentProfiles           `yaml:"agent_profiles"`
	Providers      []Provider              `yaml:"providers"`
	Panel          []PanelEntry            `yaml:"panel"`
	Synthesizer    Synthesizer             `yaml:"synthesizer"`
	DefaultPreset  string                  `yaml:"default_preset"`
	Presets        map[string]FusionPreset `yaml:"presets"`
}

// DebugConfig controls optional per-request debug artifacts.
type DebugConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Dir            string `yaml:"dir"`
	CaptureContent bool   `yaml:"capture_content"`
}

// AgentProfiles defines per-coding-agent passthrough targets.
type AgentProfiles struct {
	Pi AgentProfile `yaml:"pi"`
}

// AgentProfile defines a single agent passthrough profile.
type AgentProfile struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	Mode     string `yaml:"mode"`
}

// Enabled reports whether the profile has a dedicated target.
func (p AgentProfile) Enabled() bool {
	return p.Provider != "" || p.Model != "" || p.Mode != ""
}

// Provider defines an upstream LLM provider.
type Provider struct {
	Name      string `yaml:"name"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
}

// PanelEntry maps a provider name to a specific model for the panel.
type PanelEntry struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Synthesizer defines the model used to synthesize panel results.
type Synthesizer struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// FusionPreset defines a named panel/synthesizer configuration.
type FusionPreset struct {
	Panel       []PanelEntry `yaml:"panel"`
	Synthesizer Synthesizer  `yaml:"synthesizer"`
}

// WithDefaults returns a copy of the configuration with default values applied.
func (c Config) WithDefaults() Config {
	cfg := c
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.VirtualModel == "" {
		cfg.VirtualModel = "local/fusion"
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 120
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.Debug.Dir == "" {
		cfg.Debug.Dir = "debug"
	}
	return cfg
}

// LoadConfig reads and parses a YAML configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg = cfg.WithDefaults()
	if len(cfg.Panel) == 0 {
		return nil, fmt.Errorf("config: at least one panel entry is required")
	}
	if cfg.Synthesizer.Provider == "" || cfg.Synthesizer.Model == "" {
		return nil, fmt.Errorf("config: synthesizer provider and model are required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// GetAPIKey reads the API key for a provider from the environment.
func (c *Config) GetAPIKey(providerName string) string {
	for _, p := range c.Providers {
		if p.Name == providerName {
			if p.APIKeyEnv != "" {
				return os.Getenv(p.APIKeyEnv)
			}
			return ""
		}
	}
	return ""
}

// GetAuthToken reads the local gateway bearer token from the configured environment variable.
func (c *Config) GetAuthToken() string {
	if c.AuthTokenEnv == "" {
		return ""
	}
	return os.Getenv(c.AuthTokenEnv)
}

// GetProvider looks up a provider by name.
func (c *Config) GetProvider(providerName string) (*Provider, bool) {
	for i := range c.Providers {
		if c.Providers[i].Name == providerName {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

// Validate checks that the configuration references are correct.
func (c *Config) Validate() error {
	knownProviders := make(map[string]bool)
	for _, p := range c.Providers {
		knownProviders[p.Name] = true
	}
	if err := validateFusionTarget("config", c.Panel, c.Synthesizer, knownProviders); err != nil {
		return err
	}
	if c.DefaultPreset != "" {
		if _, ok := c.Presets[c.DefaultPreset]; !ok {
			return fmt.Errorf("default preset %q is not defined", c.DefaultPreset)
		}
	}
	for name, preset := range c.Presets {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("preset name cannot be empty")
		}
		if strings.Contains(name, "/") {
			return fmt.Errorf("preset %q cannot contain '/'", name)
		}
		if err := validateFusionTarget("preset "+name, preset.Panel, preset.Synthesizer, knownProviders); err != nil {
			return err
		}
	}
	if err := validateAgentProfile("pi", c.AgentProfiles.Pi, knownProviders); err != nil {
		return err
	}
	return nil
}

// FusionConfigForModel returns a copy of the config with the preset selected by
// the requested model. The base virtual model uses default_preset when set;
// virtual_model/<preset> selects that named preset explicitly.
func (c *Config) FusionConfigForModel(model string) (*Config, error) {
	cfg := c.WithDefaults()
	cfg.Presets = copyFusionPresets(cfg.Presets)
	presetName := ""
	if model == cfg.VirtualModel {
		presetName = cfg.DefaultPreset
	} else if strings.HasPrefix(model, cfg.VirtualModel+"/") {
		presetName = strings.TrimPrefix(model, cfg.VirtualModel+"/")
	}
	if presetName == "" {
		return &cfg, nil
	}
	preset, ok := cfg.Presets[presetName]
	if !ok {
		return nil, fmt.Errorf("unknown fusion preset %q", presetName)
	}
	cfg.Panel = append([]PanelEntry(nil), preset.Panel...)
	cfg.Synthesizer = preset.Synthesizer
	return &cfg, nil
}

func copyFusionPresets(presets map[string]FusionPreset) map[string]FusionPreset {
	if presets == nil {
		return nil
	}
	copied := make(map[string]FusionPreset, len(presets))
	for name, preset := range presets {
		copied[name] = FusionPreset{
			Panel:       append([]PanelEntry(nil), preset.Panel...),
			Synthesizer: preset.Synthesizer,
		}
	}
	return copied
}

func validateFusionTarget(name string, panel []PanelEntry, synthesizer Synthesizer, knownProviders map[string]bool) error {
	if len(panel) == 0 {
		return fmt.Errorf("%s: at least one panel entry is required", name)
	}
	for _, pe := range panel {
		if !knownProviders[pe.Provider] {
			return fmt.Errorf("%s panel references unknown provider %q", name, pe.Provider)
		}
		if pe.Model == "" {
			return fmt.Errorf("%s panel entry for provider %q has no model", name, pe.Provider)
		}
	}
	if !knownProviders[synthesizer.Provider] {
		return fmt.Errorf("%s synthesizer references unknown provider %q", name, synthesizer.Provider)
	}
	if synthesizer.Model == "" {
		return fmt.Errorf("%s synthesizer has no model set", name)
	}
	return nil
}

func validateAgentProfile(name string, profile AgentProfile, knownProviders map[string]bool) error {
	if !profile.Enabled() {
		return nil
	}
	if profile.Provider == "" || profile.Model == "" {
		return fmt.Errorf("agent profile %q requires provider and model", name)
	}
	if !knownProviders[profile.Provider] {
		return fmt.Errorf("agent profile %q references unknown provider %q", name, profile.Provider)
	}
	if profile.Mode != "" && profile.Mode != "passthrough" {
		return fmt.Errorf("agent profile %q has unsupported mode %q", name, profile.Mode)
	}
	return nil
}
