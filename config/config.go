package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DefaultSystemPrompt = "You are Rurushu, a concise terminal assistant. Use only capabilities actually provided by the host application."

type Config struct {
	BaseURL          string  `json:"base_url,omitempty"`
	APIKey           string  `json:"api_key,omitempty"`
	Model            string  `json:"model,omitempty"`
	SystemPrompt     string  `json:"system_prompt,omitempty"`
	MaxSteps         int     `json:"max_steps,omitempty"`
	MaxContextTokens int     `json:"max_context_tokens,omitempty"`
	CompactAt        float64 `json:"compact_at,omitempty"`
}

func Defaults() Config {
	return Config{
		BaseURL:          "https://api.openai.com/v1",
		SystemPrompt:     DefaultSystemPrompt,
		MaxSteps:         8,
		MaxContextTokens: 24576,
		CompactAt:        80,
	}
}

func Path() (string, error) {
	if root := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); root != "" {
		return filepath.Join(root, "rurushu", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "rurushu", "config.json"), nil
}

func Load() (Config, error) {
	cfg := Defaults()
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	applyDefaults(&cfg)
	return cfg, nil
}

func Save(cfg Config) (string, error) {
	applyDefaults(&cfg)
	path, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("chmod config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("replace config: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("chmod config: %w", err)
	}
	return path, nil
}

func applyDefaults(cfg *Config) {
	defaults := Defaults()
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = defaults.BaseURL
	}
	if strings.TrimSpace(cfg.SystemPrompt) == "" {
		cfg.SystemPrompt = defaults.SystemPrompt
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = defaults.MaxSteps
	}
	if cfg.MaxContextTokens <= 0 {
		cfg.MaxContextTokens = defaults.MaxContextTokens
	}
	if cfg.CompactAt <= 0 || cfg.CompactAt > 100 {
		cfg.CompactAt = defaults.CompactAt
	}
}
