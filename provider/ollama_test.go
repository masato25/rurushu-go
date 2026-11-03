package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOllamaProviderUsesNativeChatAndDisablesThinking(t *testing.T) {
	var gotPath string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"OK"},"done":false}`)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"."},"done":false}`)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":20,"eval_count":3}`)
	}))
	defer server.Close()

	p := NewOllamaProvider(server.URL + "/v1/")
	max := 32
	events, err := p.Stream(context.Background(), CompletionRequest{
		Model: "qwen3:8b", ReasoningEffort: "medium", MaxTokens: &max,
		Messages: []Message{{Role: RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var done bool
	for ev := range events {
		if ev.Type == EventToken {
			text += ev.Text
		}
		if ev.Type == EventDone {
			done = true
		}
	}
	if gotPath != "/api/chat" || text != "OK." || !done {
		t.Fatalf("path=%q text=%q done=%v", gotPath, text, done)
	}
	if body["think"] != false {
		t.Fatalf("think=%#v", body["think"])
	}
	options := body["options"].(map[string]any)
	if options["num_predict"] != float64(32) {
		t.Fatalf("options=%#v", options)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort leaked to Ollama: %#v", body)
	}
}

func TestOllamaProviderListModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		fmt.Fprint(w, `{"models":[{"name":"qwen3:8b"},{"name":"qwen-vlm:latest"}]}`)
	}))
	defer server.Close()
	p := NewOllamaProvider(server.URL)
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "qwen3:8b" {
		t.Fatalf("models=%v", models)
	}
}
