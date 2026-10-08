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
}
