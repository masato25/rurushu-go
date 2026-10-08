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
		lastCallFingerprint := ""
		repeatedCallCount := 0
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
					if !emitEvent(ctx, out, event) {
						return
					}
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
			forceFinalize := false
			for _, call := range toolCalls {
				if ctx.Err() != nil {
					return
				}
				start := provider.StreamEvent{Type: provider.EventToolStart, ToolExecution: &provider.ToolExecutionInfo{ID: call.ID, Tool: call.Function.Name, Args: call.Function.Arguments}}
				if !emitEvent(ctx, out, start) {
					return
				}
				fingerprint := toolCallFingerprint(call)
				if fingerprint == lastCallFingerprint {
					repeatedCallCount++
				} else {
					lastCallFingerprint = fingerprint
					repeatedCallCount = 1
				}

				output, isError := c.executeTool(ctx, call)
				if repeatedCallCount >= 3 {
					output += "\n\n[Harness note: this identical tool call has been repeated multiple times. Use the result already gathered and move toward answering the user.]"
					forceFinalize = true
				}
				result := provider.StreamEvent{Type: provider.EventToolResult, ToolExecution: &provider.ToolExecutionInfo{ID: call.ID, Tool: call.Function.Name, Args: call.Function.Arguments, Output: output, IsError: isError}}
				if !emitEvent(ctx, out, result) {
					return
				}
				messages = append(messages, provider.Message{Role: provider.RoleTool, ToolCallID: call.ID, Name: call.Function.Name, Content: output, ToolIsError: isError})
			}
			if forceFinalize {
				c.finalizeWithoutTools(ctx, initial, messages, out)
				return
			}
		}
		c.finalizeWithoutTools(ctx, initial, messages, out)
	}()
	return out
}

func (c *Client) finalizeWithoutTools(ctx context.Context, initial provider.CompletionRequest, messages []provider.Message, out chan<- provider.StreamEvent) {
	if compacted, info := c.compactContext(messages, nil); info != nil {
		messages = compacted
		if !emitEvent(ctx, out, provider.StreamEvent{Type: provider.EventContextCompact, Compaction: info}) {
			return
		}
	}

	finalInstruction := fmt.Sprintf(
		"Tool-use budget (%d steps) is exhausted. Do not request more tools. Answer the user's request now using the information already gathered. If the available evidence is incomplete, say so briefly.",
		c.toolMaxSteps,
	)
	messages = withFinalizationInstruction(messages, finalInstruction)
	req := initial
	req.Messages = messages
	req.Tools = nil

	stream, err := c.provider.Stream(ctx, req)
	if err != nil {
		emitEvent(ctx, out, provider.StreamEvent{Type: provider.EventError, Error: err})
		return
	}

	emittedAnswer := false
	var usage *provider.TokenUsage
	for event := range stream {
		if ctx.Err() != nil {
			return
		}
		switch event.Type {
		case provider.EventToken:
			if strings.TrimSpace(event.Text) != "" {
				emittedAnswer = true
			}
			if !emitEvent(ctx, out, event) {
				return
			}
		case provider.EventReasoning:
			if !emitEvent(ctx, out, event) {
				return
			}
		case provider.EventUsage:
			usage = event.Usage
			if !emitEvent(ctx, out, event) {
				return
			}
		case provider.EventDone:
			if event.Usage != nil {
				usage = event.Usage
			}
		case provider.EventError:
			emitEvent(ctx, out, event)
			return
		}
	}
	if !emittedAnswer {
		emitEvent(ctx, out, provider.StreamEvent{Type: provider.EventError, Error: MaxStepsError{Limit: c.toolMaxSteps}})
		return
	}
	emitEvent(ctx, out, provider.StreamEvent{Type: provider.EventDone, Usage: usage})
}

func withFinalizationInstruction(messages []provider.Message, instruction string) []provider.Message {
	result := append([]provider.Message(nil), messages...)
	for i := range result {
		if result[i].Role != provider.RoleSystem {
			continue
		}
		if strings.TrimSpace(result[i].Content) != "" {
			result[i].Content += "\n\n"
		}
		result[i].Content += instruction
		return result
	}
	return append([]provider.Message{{Role: provider.RoleSystem, Content: instruction}}, result...)
}

func toolCallFingerprint(call provider.ToolCall) string {
	args := strings.TrimSpace(call.Function.Arguments)
	if args == "" {
		args = "{}"
	} else {
		var value any
		if json.Unmarshal([]byte(args), &value) == nil {
			if normalized, err := json.Marshal(value); err == nil {
				args = string(normalized)
			}
		}
	}
	return call.Function.Name + "\x00" + args
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
