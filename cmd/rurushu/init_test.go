package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitCreatesCOSConfigWithoutOverwriting(t *testing.T) {
	root := t.TempDir()
	args := []string{"--cwd", root}
	if err := run([]string{"--init", "--cwd", root}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".rurushugo", "cos.json")
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"projectUrl"`) {
		t.Fatalf("created config: %q %v", data, err)
	}
	if err := os.WriteFile(path, []byte(`{"project":"custom","projectUrl":"https://example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInit(args); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"custom"`) {
		t.Fatalf("config was overwritten: %q %v", data, err)
	}
	if err := runInit([]string{"--cwd", root, "extra"}); err == nil {
		t.Fatal("unexpected positional argument accepted")
	}
}
