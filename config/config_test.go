package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoad(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)

	want := Config{
		BaseURL:          "http://localhost:8081/v1",
		APIKey:           "secret",
		Model:            "test-model",
		SystemPrompt:     "test prompt",
		MaxSteps:         12,
		MaxContextTokens: 32000,
		CompactAt:        75,
		AutoApprove:      true,
	}
	path, err := Save(want)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "rurushu", "config.json") {
		t.Fatalf("path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestLoadMissingUsesDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL == "" || got.SystemPrompt == "" || got.MaxSteps <= 0 {
		t.Fatalf("defaults not applied: %#v", got)
	}
	if got.AutoApprove {
		t.Fatal("auto approve must be disabled by default")
	}
}

func TestLoadForDirOverlaysProjectRoleDefaults(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	_, err := Save(Config{
		Single: RoleConfig{Provider: "cos", Model: "chatgpt-web"},
		Duel: DuelConfig{
			Left:      RoleConfig{Provider: "cos", Model: "chatgpt-web"},
			Right:     RoleConfig{Provider: "ollama", Model: "global-right"},
			OllamaURL: "http://global:11434",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".rurushu"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".rurushu", "config.json"), []byte(`{"duel":{"right":{"model":"project-right"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadForDir(project)
	if err != nil {
		t.Fatal(err)
	}
	if got.Single.Provider != "cos" || got.Duel.Right.Provider != "ollama" || got.Duel.Right.Model != "project-right" || got.Duel.OllamaURL != "http://global:11434" {
		t.Fatalf("merged config=%#v", got)
	}
}
