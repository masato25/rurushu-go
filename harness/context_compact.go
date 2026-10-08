package harness

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/masato25/rurushu-go/provider"
)

const (
	charsPerTokenEstimate = 3.5
	summaryMaxChars       = 6000
	toolSnippetChars      = 600
	retainedToolTurns     = 2
)

func estimateContextTokens(messages []provider.Message, toolSchemas []map[string]any) int {
	chars := 0
	for _, message := range messages {
		chars += 14 + len(message.Role) + len(message.Content) + len(message.ReasoningContent) + len(message.Name) + len(message.ToolCallID)
		for _, call := range message.ToolCalls {
			chars += len(call.ID) + len(call.Function.Name) + len(call.Function.Arguments)
		}
	}
	if len(toolSchemas) > 0 {
		if data, err := json.Marshal(toolSchemas); err == nil {
			chars += len(data)
		}
	}
	return int(float64(chars) / charsPerTokenEstimate)
}

func (c *Client) compactContext(messages []provider.Message, toolSchemas []map[string]any) ([]provider.Message, *provider.ContextCompactionInfo) {
	if c == nil || c.autoCompactPercent <= 0 || c.maxContextTokens <= 0 {
		return messages, nil
	}
	threshold := int(float64(c.maxContextTokens) * c.autoCompactPercent / 100)
	if threshold <= 0 {
		return messages, nil
	}
	beforeTokens := estimateContextTokens(messages, toolSchemas)
	if beforeTokens < threshold {
		return messages, nil
	}
	compacted := capOversizedMessages(messages, c.maxContextTokens)
	compacted = compactMessages(compacted, retainedToolTurns)
	compacted = capOversizedMessages(compacted, c.maxContextTokens)
	for limit := c.maxContextTokens / 2; estimateContextTokens(compacted, toolSchemas) > threshold && limit >= 64; limit /= 2 {
		compacted = capOversizedMessages(compacted, limit)
	}
	afterTokens := estimateContextTokens(compacted, toolSchemas)
	if afterTokens >= beforeTokens {
		return messages, nil
	}
	return compacted, &provider.ContextCompactionInfo{
		BeforeTokens: beforeTokens, AfterTokens: afterTokens,
		BeforeMessages: len(messages), AfterMessages: len(compacted), Threshold: threshold,
		Percent: int(c.autoCompactPercent),
	}
}

func compactMessages(messages []provider.Message, retainToolTurns int) []provider.Message {
	if len(messages) < 5 || retainToolTurns <= 0 {
		return append([]provider.Message(nil), messages...)
	}
	prefixEnd := 0
	if messages[0].Role == provider.RoleSystem {
		prefixEnd = 1
	}
	if prefixEnd < len(messages) && messages[prefixEnd].Role == provider.RoleUser {
		prefixEnd++
	}
	startRecent := recentToolTurnStart(messages, prefixEnd, retainToolTurns)
	if startRecent <= prefixEnd {
		return append([]provider.Message(nil), messages...)
	}
	older := messages[prefixEnd:startRecent]
	if len(older) == 0 {
		return append([]provider.Message(nil), messages...)
	}
	summary := summarizeMessages(older)
	result := make([]provider.Message, 0, prefixEnd+1+len(messages)-startRecent)
	result = append(result, messages[:prefixEnd]...)
	result = append(result, provider.Message{
		Role: provider.RoleUser,
		Content: "# Compacted tool evidence\n" + summary +
			"\nUse this as evidence from earlier steps in this run. Re-open durable sources if an omitted detail becomes necessary.",
	})
	result = append(result, messages[startRecent:]...)
	return result
}

func recentToolTurnStart(messages []provider.Message, minIndex, retainTurns int) int {
	seen := 0
	for i := len(messages) - 1; i >= minIndex; i-- {
		if messages[i].Role == provider.RoleAssistant && len(messages[i].ToolCalls) > 0 {
			seen++
			if seen >= retainTurns {
				return i
			}
		}
	}
	return minIndex
}

func summarizeMessages(messages []provider.Message) string {
	var b strings.Builder
	for _, message := range messages {
		if b.Len() >= summaryMaxChars {
			break
		}
		switch message.Role {
		case provider.RoleAssistant:
			if text := compactSnippet(message.Content, 300); text != "" {
				fmt.Fprintf(&b, "- assistant observation: %s\n", text)
			}
			if len(message.ToolCalls) > 0 {
				names := make([]string, 0, len(message.ToolCalls))
				for _, call := range message.ToolCalls {
					names = append(names, call.Function.Name)
				}
				fmt.Fprintf(&b, "- tools requested: %s\n", strings.Join(names, ", "))
			}
		case provider.RoleTool:
			name := strings.TrimSpace(message.Name)
			if name == "" {
				name = "tool"
			}
			fmt.Fprintf(&b, "- %s result: %s\n", name, compactSnippet(message.Content, toolSnippetChars))
		case provider.RoleUser:
			if strings.Contains(message.Content, "# Compacted tool evidence") {
				fmt.Fprintf(&b, "- prior compacted evidence: %s\n", compactSnippet(message.Content, 900))
			}
		}
	}
	text := strings.TrimSpace(b.String())
	if len(text) > summaryMaxChars {
		text = text[:summaryMaxChars] + "\n[older evidence truncated]"
	}
	if text == "" {
		return "Earlier tool work was compacted; consult durable sources for exact details."
	}
	return text
}

func compactSnippet(value string, limit int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

func capOversizedMessages(messages []provider.Message, maxTokens int) []provider.Message {
	if maxTokens <= 0 {
		return messages
	}
	result := append([]provider.Message(nil), messages...)
	maxToolChars := int(float64(maxTokens) * charsPerTokenEstimate * 0.30)
	if maxToolChars < 64 {
		maxToolChars = 64
	}
	for i := range result {
		if result[i].Role != provider.RoleTool || len(result[i].Content) <= maxToolChars {
			continue
		}
		head := maxToolChars * 3 / 4
		tail := maxToolChars - head
		result[i].Content = result[i].Content[:head] +
			fmt.Sprintf("\n[tool output truncated from %d bytes for context]\n", len(result[i].Content)) +
			result[i].Content[len(result[i].Content)-tail:]
	}
	return result
}
