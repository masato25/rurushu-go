package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAICompatibleProviderStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret-key" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher support")
		}

		// Stream reasoning chunk
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"Thinking about the problem...\"}}]}\n\n")
		flusher.Flush()

		// Stream text chunk
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello from OpenAI-compatible!\"}}]}\n\n")
		flusher.Flush()

		// Stream tool call chunk
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_123\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"main.go\\\"}\"}}]}}]}\n\n")
		flusher.Flush()

		// Stream usage chunk
		fmt.Fprintf(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"total_tokens\":30}}\n\n")
		flusher.Flush()

		// Stream [DONE]
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	prov := NewOpenAICompatibleProvider(server.URL, "test-secret-key")
	if prov.ID() != "openai-compatible" {
		t.Fatalf("expected provider ID 'openai-compatible', got '%s'", prov.ID())
	}

	events, err := prov.Stream(context.Background(), CompletionRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: RoleUser, Content: "Hello"},
		},
	})
	if err != nil {
		t.Fatalf("failed to stream from openai-compatible: %v", err)
	}

	var hasReasoning, hasContent, hasToolCall, hasUsage, hasDone bool

	for ev := range events {
		switch ev.Type {
		case EventReasoning:
			hasReasoning = true
			if ev.ReasoningText != "Thinking about the problem..." {
				t.Fatalf("unexpected reasoning text: %s", ev.ReasoningText)
			}
		case EventToken:
			hasContent = true
			if ev.Text != "Hello from OpenAI-compatible!" {
				t.Fatalf("unexpected content token: %s", ev.Text)
			}
		case EventToolCall:
			hasToolCall = true
			if len(ev.ToolCalls) == 0 || ev.ToolCalls[0].Function.Name != "read" {
				t.Fatalf("unexpected tool calls: %+v", ev.ToolCalls)
			}
		case EventUsage:
			hasUsage = true
			if ev.Usage.TotalTokens != 30 {
				t.Fatalf("unexpected total tokens: %d", ev.Usage.TotalTokens)
			}
		case EventDone:
			hasDone = true
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Error)
		}
	}

	if !hasReasoning || !hasContent || !hasToolCall || !hasUsage || !hasDone {
		t.Fatalf("missing events: reasoning=%v, content=%v, toolCall=%v, usage=%v, done=%v",
			hasReasoning, hasContent, hasToolCall, hasUsage, hasDone)
	}
}

func TestZeroAPIResponsesStyleDoneCarriesFinalToolArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: response.output_item.added\n")
		fmt.Fprint(w, `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":""}}`+"\n\n")
		fmt.Fprint(w, "event: response.output_item.done\n")
		fmt.Fprint(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"GOAL.md\"}"}}`+"\n\n")
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[]}}`+"\n\n")
	}))
	defer server.Close()

	prov := NewOpenAICompatibleProvider(server.URL, "")
	events, err := prov.Stream(context.Background(), CompletionRequest{Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	var calls []ToolCall
	for ev := range events {
		if ev.Type == EventToolCall {
			calls = ev.ToolCalls
		}
	}
	if len(calls) != 1 || calls[0].Function.Arguments != `{"path":"GOAL.md"}` {
		t.Fatalf("responses-style final arguments not reconstructed: %+v", calls)
	}
}

func TestOpenAICompatibleProviderUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	prov := NewOpenAICompatibleProvider(server.URL, "wrong-key")
	_, err := prov.Stream(context.Background(), CompletionRequest{Model: "gpt-4o"})
	if err == nil {
		t.Fatal("expected error for 403 Forbidden response")
	}
}

func TestZeroAPIRateLimitErrorPreservesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, "quota exceeded", http.StatusTooManyRequests)
	}))
	defer server.Close()

	prov := NewOpenAICompatibleProvider(server.URL, "test-key")
	_, err := prov.Stream(context.Background(), CompletionRequest{Model: "gpt-test"})
	if err == nil {
		t.Fatal("expected rate-limit error")
	}
	httpErr, ok := IsRateLimitError(err)
	if !ok {
		t.Fatalf("expected rate-limit classification, got %T: %v", err, err)
	}
	if httpErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d", httpErr.StatusCode)
	}
	if httpErr.RetryAfter != 7*time.Second {
		t.Fatalf("retry_after=%s, want 7s", httpErr.RetryAfter)
	}
}

func TestIsRateLimitErrorRecognizesGatewayQuotaMessage(t *testing.T) {
	err := &HTTPError{StatusCode: http.StatusForbidden, Body: "Usage limit reached for this account"}
	if _, ok := IsRateLimitError(err); !ok {
		t.Fatal("expected quota phrase to be classified as rate limit")
	}
}

func TestOpenAICompatibleProviderCredentialCanBeUpdated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer updated-key" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"test-model"}]}`)
	}))
	defer server.Close()

	prov := NewOpenAICompatibleProvider(server.URL, "")
	if prov.HasAPIKey() {
		t.Fatal("expected client to start without a configured key")
	}
	prov.SetAPIKey(" updated-key ")
	if !prov.HasAPIKey() {
		t.Fatal("expected updated key to be configured")
	}
	models, err := prov.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0] != "test-model" {
		t.Fatalf("expected runtime key to authorize model listing, models=%v err=%v", models, err)
	}
}

func TestOpenAICompatibleUsesExactBaseURL(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer server.Close()

	prov := NewOpenAICompatibleProvider(server.URL+"/custom/v1", "")
	if _, err := prov.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/custom/v1/models" {
		t.Fatalf("path = %q", gotPath)
	}
}
