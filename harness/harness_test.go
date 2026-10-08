package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/arborlogic/rurushu-go/permission"
	"github.com/arborlogic/rurushu-go/provider"
	"github.com/arborlogic/rurushu-go/tool"
)

type scriptedProvider struct {
	mu    sync.Mutex
	calls []provider.CompletionRequest
}

func (*scriptedProvider) ID() string                                   { return "scripted" }
func (*scriptedProvider) Name() string                                 { return "Scripted" }
func (*scriptedProvider) ListModels(context.Context) ([]string, error) { return []string{"test"}, nil }

func (p *scriptedProvider) Stream(_ context.Context, req provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	p.mu.Lock()
	p.calls = append(p.calls, req)
	n := len(p.calls)
	p.mu.Unlock()
	out := make(chan provider.StreamEvent, 4)
	if n == 1 {
		out <- provider.StreamEvent{Type: provider.EventToolCall, ToolCalls: []provider.ToolCall{{
			ID: "call-1", Type: "function", Function: provider.FunctionCall{Name: "echo", Arguments: `{"value":"hi"}`},
		}}}
		out <- provider.StreamEvent{Type: provider.EventDone}
	} else {
		out <- provider.StreamEvent{Type: provider.EventToken, Text: "finished"}
		out <- provider.StreamEvent{Type: provider.EventDone, Usage: &provider.TokenUsage{TotalTokens: 12}}
	}
	close(out)
	return out, nil
}

type echoTool struct{}

func (echoTool) ID() string                 { return "echo" }
func (echoTool) Description() string        { return "echo a value" }
func (echoTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (echoTool) Execute(_ context.Context, args json.RawMessage, execCtx *tool.ExecutionContext) (*tool.Result, error) {
	if execCtx.AskPermission == nil || !execCtx.AskPermission(permission.Request{Tool: "echo"}) {
		return nil, errors.New("permission denied")
	}
	var in struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, err
	}
	return &tool.Result{Output: in.Value}, nil
}

func TestRunExecutesToolLoop(t *testing.T) {
	prov := &scriptedProvider{}
	registry := tool.NewRegistry()
	registry.Register(echoTool{})
	client, err := New(prov, Config{Tools: registry, Permission: permission.AllowAll{}, SystemPrompt: "system"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Run(context.Background(), provider.CompletionRequest{
		Model: "test", Messages: []provider.Message{{Role: provider.RoleUser, Content: "go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "finished" || result.ToolCalls != 1 || result.Usage.TotalTokens != 12 {
		t.Fatalf("result = %#v", result)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.calls) != 2 {
		t.Fatalf("provider calls = %d", len(prov.calls))
	}
	last := prov.calls[1].Messages[len(prov.calls[1].Messages)-1]
	if last.Role != provider.RoleTool || last.Content != "hi" {
		t.Fatalf("tool result message = %#v", last)
	}
}

func TestEnrichUsesGenericContextSource(t *testing.T) {
	prov := &scriptedProvider{}
	client, err := New(prov, Config{
		SystemPrompt: "base",
		Context: ContextFunc(func(context.Context, string, int) ([]string, error) {
			return []string{"fact one", "fact two"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := client.Enrich(context.Background(), provider.CompletionRequest{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 || !strings.Contains(req.Messages[0].Content, "fact two") {
		t.Fatalf("messages = %#v", req.Messages)
	}
}

func TestValidator(t *testing.T) {
	prov := &scriptedProvider{}
	client, err := New(prov, Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Run(context.Background(), provider.CompletionRequest{}, func(Result) error {
		return errors.New("rejected")
	})
	if err == nil || err.Error() != "rejected" {
		t.Fatalf("validator error = %v", err)
	}
}

type loopingProvider struct{}

func (loopingProvider) Stream(context.Context, provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	out := make(chan provider.StreamEvent, 2)
	out <- provider.StreamEvent{Type: provider.EventToolCall, ToolCalls: []provider.ToolCall{{
		ID: "loop", Type: "function", Function: provider.FunctionCall{Name: "echo", Arguments: `{"value":"again"}`},
	}}}
	out <- provider.StreamEvent{Type: provider.EventDone}
	close(out)
	return out, nil
}

func TestRunStopsAtToolStepLimit(t *testing.T) {
	registry := tool.NewRegistry()
	registry.Register(echoTool{})
	client, err := New(loopingProvider{}, Config{Tools: registry, Permission: permission.AllowAll{}, ToolMaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Run(context.Background(), provider.CompletionRequest{Model: "test"})
	if err == nil || !errors.Is(err, ErrMaxSteps) || !strings.Contains(err.Error(), "maximum step limit (2)") {
		t.Fatalf("max-step error = %v", err)
	}
}

func TestStreamForwardsToolCallEvent(t *testing.T) {
	prov := &scriptedProvider{}
	registry := tool.NewRegistry()
	registry.Register(echoTool{})
	client, err := New(prov, Config{Tools: registry, Permission: permission.AllowAll{}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.Stream(context.Background(), provider.CompletionRequest{Model: "test", Messages: []provider.Message{{Role: provider.RoleUser, Content: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for event := range stream {
		if event.Type == provider.EventToolCall {
			found = true
			if len(event.ToolCalls) != 1 || event.ToolCalls[0].Function.Name != "echo" {
				t.Fatalf("tool call event = %#v", event)
			}
		}
	}
	if !found {
		t.Fatal("tool call event was not forwarded")
	}
}
