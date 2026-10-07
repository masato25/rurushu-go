package provider

import (
	"context"
	"strings"
)

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content,omitempty"`
	Images           []string   `json:"images,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
}

type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type CompletionRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Tools       []map[string]any `json:"tools,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	MaxTokens   *int             `json:"max_tokens,omitempty"`
}

type EventType string

const (
	EventToken          EventType = "token"
	EventReasoning      EventType = "reasoning"
	EventToolCall       EventType = "tool_call"
	EventToolStart      EventType = "tool_start"
	EventToolResult     EventType = "tool_result"
	EventUsage          EventType = "usage"
	EventContextCompact EventType = "context_compact"
	EventError          EventType = "error"
	EventDone           EventType = "done"
)

type ToolExecutionInfo struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Args    string `json:"args,omitempty"`
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

type ContextCompactionInfo struct {
	BeforeTokens   int `json:"before_tokens"`
	AfterTokens    int `json:"after_tokens"`
	BeforeMessages int `json:"before_messages"`
	AfterMessages  int `json:"after_messages"`
	Threshold      int `json:"threshold"`
	Percent        int `json:"percent"`
}

type StreamEvent struct {
	Type          EventType              `json:"type"`
	Text          string                 `json:"text,omitempty"`
	ReasoningText string                 `json:"reasoning_text,omitempty"`
	ToolCalls     []ToolCall             `json:"tool_calls,omitempty"`
	ToolExecution *ToolExecutionInfo     `json:"tool_execution,omitempty"`
	Compaction    *ContextCompactionInfo `json:"compaction,omitempty"`
	Usage         *TokenUsage            `json:"usage,omitempty"`
	Error         error                  `json:"error,omitempty"`
}

// Provider is the only model-backend contract required by the harness.
type Provider interface {
	Stream(ctx context.Context, req CompletionRequest) (<-chan StreamEvent, error)
}

// ModelLister is an optional provider capability for applications that expose
// model discovery in a CLI or UI. The harness itself does not require it.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

// SanitizeMessages normalizes tool-call history for OpenAI-compatible APIs.
func SanitizeMessages(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}
	validToolCallIDs := make(map[string]bool)
	cleaned := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == RoleUser && strings.TrimSpace(m.Content) == "" && len(m.Images) == 0 {
			continue
		}
		if len(cleaned) > 0 && m.Role == RoleUser && cleaned[len(cleaned)-1].Role == RoleUser && cleaned[len(cleaned)-1].Content == m.Content {
			continue
		}
		if m.Role == RoleAssistant {
			if m.Content == "" && len(m.ToolCalls) == 0 {
				if m.ReasoningContent != "" {
					m.Content = m.ReasoningContent
				} else {
					m.Content = "(done)"
				}
			}
			for i := range m.ToolCalls {
				tc := &m.ToolCalls[i]
				if strings.TrimSpace(tc.Function.Arguments) == "" {
					tc.Function.Arguments = "{}"
				}
				if tc.Type == "" {
					tc.Type = "function"
				}
				if tc.ID != "" {
					validToolCallIDs[tc.ID] = true
				}
			}
		}
		if m.Role == RoleTool && m.ToolCallID != "" && !validToolCallIDs[m.ToolCallID] {
			continue
		}
		cleaned = append(cleaned, m)
	}
	return cleaned
}
