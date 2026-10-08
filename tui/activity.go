package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/masato25/rurushu-go/provider"
)

type ActivityMode string

const (
	ActivityNormal  ActivityMode = "normal"
	ActivityVerbose ActivityMode = "verbose"
	ActivityDebug   ActivityMode = "debug"
)

func ParseActivityMode(value string) (ActivityMode, error) {
	switch ActivityMode(strings.ToLower(strings.TrimSpace(value))) {
	case "", ActivityNormal:
		return ActivityNormal, nil
	case ActivityVerbose:
		return ActivityVerbose, nil
	case ActivityDebug:
		return ActivityDebug, nil
	default:
		return "", fmt.Errorf("activity mode must be normal, verbose, or debug")
	}
}

type activityEntry struct {
	Kind    string
	ID      string
	Tool    string
	Args    string
	Output  string
	IsError bool
	Done    bool
	Usage   *provider.TokenUsage
	Compact *provider.ContextCompactionInfo
}

func renderActivity(entry *activityEntry, mode ActivityMode, width int) string {
	if entry == nil || width <= 0 {
		return ""
	}
	switch entry.Kind {
	case "thinking":
		return mutedStyle.Render(truncate("◦ thinking", width))
	case "usage":
		if mode != ActivityDebug || entry.Usage == nil {
			return ""
		}
		return mutedStyle.Render(truncate(fmt.Sprintf("◦ usage  prompt=%d completion=%d total=%d", entry.Usage.PromptTokens, entry.Usage.CompletionTokens, entry.Usage.TotalTokens), width))
	case "compact":
		if mode != ActivityDebug || entry.Compact == nil {
			return ""
		}
		return mutedStyle.Render(truncate(fmt.Sprintf("◦ compact  %d→%d tokens  %d→%d messages", entry.Compact.BeforeTokens, entry.Compact.AfterTokens, entry.Compact.BeforeMessages, entry.Compact.AfterMessages), width))
	case "tool":
		return renderToolActivity(entry, mode, width)
	default:
		return ""
	}
}

func renderToolActivity(entry *activityEntry, mode ActivityMode, width int) string {
	marker := "◦"
	if entry.Done {
		marker = "✓"
		if entry.IsError {
			marker = "✗"
		}
	}
	target := summarizeToolArgs(entry.Tool, entry.Args)
	line := marker + " " + entry.Tool
	if target != "" {
		line += "  " + target
	}
	if entry.Done {
		if summary := summarizeToolOutput(entry.Tool, entry.Output, entry.IsError); summary != "" {
			line += " · " + summary
		}
	}

	var b strings.Builder
	b.WriteString(mutedStyle.Render(truncate(line, width)))
	if mode == ActivityNormal {
		return b.String()
	}
	if args := strings.TrimSpace(entry.Args); args != "" {
		b.WriteString("\n")
		b.WriteString(mutedStyle.Render(truncate("  args: "+compactJSON(args), width)))
	}
	if entry.Done && strings.TrimSpace(entry.Output) != "" {
		b.WriteString("\n")
		if mode == ActivityDebug {
			b.WriteString(renderIndentedBlock("  result: ", entry.Output, width))
		} else {
			b.WriteString(mutedStyle.Render(truncate("  result: "+firstUsefulLine(entry.Output), width)))
		}
	}
	return b.String()
}

func summarizeToolArgs(toolName, raw string) string {
	var args map[string]any
	if json.Unmarshal([]byte(raw), &args) != nil {
		return truncate(strings.TrimSpace(raw), 48)
	}
	get := func(key string) string {
		if value, ok := args[key].(string); ok {
			return strings.TrimSpace(value)
		}
		return ""
	}
	switch toolName {
	case "read":
		return get("path")
	case "glob":
		pattern := get("pattern")
		if path := get("path"); path != "" && path != "." {
			return path + " · " + pattern
		}
		return pattern
	case "grep":
		pattern := get("pattern")
		if path := get("path"); path != "" && path != "." {
			return fmt.Sprintf("%q in %s", pattern, path)
		}
		return fmt.Sprintf("%q", pattern)
	default:
		return truncate(compactJSON(raw), 48)
	}
}

func summarizeToolOutput(toolName, output string, isError bool) string {
	if isError {
		return truncate(firstUsefulLine(output), 56)
	}
	lines := usefulOutputLines(output)
	switch toolName {
	case "read":
		if len(lines) > 0 {
			return fmt.Sprintf("%d lines", len(lines))
		}
	case "glob":
		if len(lines) > 0 {
			return fmt.Sprintf("%d files", len(lines))
		}
	case "grep":
		if len(lines) > 0 {
			return fmt.Sprintf("%d matches", len(lines))
		}
	}
	return truncate(firstUsefulLine(output), 56)
}

func usefulOutputLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "(") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func firstUsefulLine(output string) string {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if value := strings.TrimSpace(line); value != "" {
			return value
		}
	}
	return "completed"
}

func compactJSON(raw string) string {
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return strings.TrimSpace(raw)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return string(data)
}

func renderIndentedBlock(prefix, output string, width int) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		lead := "          "
		if i == 0 {
			lead = prefix
		}
		available := max(1, width-lipgloss.Width(lead))
		b.WriteString(mutedStyle.Render(lead + truncate(line, available)))
	}
	return b.String()
}
