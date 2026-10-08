package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty(" ", " second ", "third"); got != "second" {
		t.Fatalf("got %q", got)
	}
}

func TestRunRequiresModel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("RURUSHU_MODEL", "")
	t.Setenv("OPENAI_MODEL", "")
	err := run([]string{"--cwd", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunListModelsWithoutModel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"test-model"}]}`)
	}))
	defer server.Close()

	t.Setenv("RURUSHU_MODEL", "")
	t.Setenv("OPENAI_MODEL", "")
	if err := run([]string{"--base-url", server.URL, "--list-models", "--cwd", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}
