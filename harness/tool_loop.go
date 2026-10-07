package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/arborlogic/rurushu-go/permission"
	"github.com/arborlogic/rurushu-go/provider"
	"github.com/arborlogic/rurushu-go/tool"
)

var ErrMaxSteps = errors.New("tool loop reached maximum step limit")

type MaxStepsError struct {
	Limit int
}

func (e MaxStepsError) Error() string {
	return fmt.Sprintf("tool loop reached maximum step limit (%d)", e.Limit)
}

func (e MaxStepsError) Unwrap() error { return ErrMaxSteps }

func (c *Client) streamWithTools(ctx context.Context, initial provider.CompletionRequest) <-chan provider.StreamEvent {
	out := make(chan provider.StreamEvent, 64)
	go func() {
		defer close(out)
		messages := append([]provider.Message(nil), initial.Messages...)
		for step := 1; step <= c.toolMaxSteps; step++ {
			if compacted, info := c.compactContext(messages, initial.Tools); info != nil {
				messages = compacted
				if !emitEvent(ctx, out, provider.StreamEvent{Type: provider.EventContextCompact, Compaction: info}) {
					return
				}
			}
			req := initial
			req.Messages = messages
			stream, err := c.provider.Stream(ctx, req)
			if err != nil {
				emitEvent(ctx, out, provider.StreamEvent{Type: provider.EventError, Error: err})
				return
			}
			var text, reasoning strings.Builder
			var toolCalls []provider.ToolCall
			var usage *provider.TokenUsage
			for event := range stream {
				if ctx.Err() != nil {
					return
				}
				switch event.Type {
				case provider.EventToken:
					text.WriteString(event.Text)
					if !emitEvent(ctx, out, event) {
						return
					}
				case provider.EventReasoning:
					reasoning.WriteString(event.ReasoningText)
					if !emitEvent(ctx, out, event) {
						return
					}
				case provider.EventToolCall:
					toolCalls = event.ToolCalls
				case provider.EventUsage:
					usage = event.Usage
					if !emitEvent(ctx, out, event) {
						return
					}
				case provider.EventError:
					emitEvent(ctx, out, event)
					return
				case provider.EventDone:
					if event.Usage != nil {
						usage = event.Usage
					}
				}
			}
			if len(toolCalls) == 0 {
				emitEvent(ctx, out, provider.StreamEvent{Type: provider.EventDone, Usage: usage})
				return
			}
			messages = append(messages, provider.Message{Role: provider.RoleAssistant, Content: text.String(), ReasoningContent: reasoning.String(), ToolCalls: toolCalls})
			for _, call := range toolCalls {
				if ctx.Err() != nil {
					return
				}
				start := provider.StreamEvent{Type: provider.EventToolStart, ToolExecution: &provider.ToolExecutionInfo{ID: call.ID, Tool: call.Function.Name, Args: call.Function.Arguments}}
				if !emitEvent(ctx, out, start) {
					return
				}
				output, isError := c.executeTool(ctx, call)
				result := provider.StreamEvent{Type: provider.EventToolResult, ToolExecution: &provider.ToolExecutionInfo{ID: call.ID, Tool: call.Function.Name, Args: call.Function.Arguments, Output: output, IsError: isError}}
				if !emitEvent(ctx, out, result) {
					return
				}
				messages = append(messages, provider.Message{Role: provider.RoleTool, ToolCallID: call.ID, Name: call.Function.Name, Content: output})
			}
		}
		emitEvent(ctx, out, provider.StreamEvent{Type: provider.EventError, Error: MaxStepsError{Limit: c.toolMaxSteps}})
	}()
	return out
}

func (c *Client) executeTool(ctx context.Context, call provider.ToolCall) (string, bool) {
	t, ok := c.tools.Get(call.Function.Name)
	if !ok {
		return fmt.Sprintf("tool %q is not registered", call.Function.Name), true
	}
	args := strings.TrimSpace(call.Function.Arguments)
	if args == "" {
		args = "{}"
	}
	execCtx := &tool.ExecutionContext{CWD: c.cwd}
	if c.permission != nil {
		execCtx.AskPermission = func(req permission.Request) bool {
			approved, err := c.permission.Ask(ctx, req)
			return err == nil && approved
		}
	}
	result, err := t.Execute(ctx, json.RawMessage(args), execCtx)
	if err != nil {
		return "tool execution error: " + err.Error(), true
	}
	if result == nil {
		return "(no output)", false
	}
	output := strings.TrimSpace(result.Output)
	if output == "" {
		output = "(no output)"
	}
	return output, result.IsError
}

func emitEvent(ctx context.Context, out chan<- provider.StreamEvent, event provider.StreamEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- event:
		return true
	}
}
