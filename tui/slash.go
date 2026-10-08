package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// SlashCommand is a lightweight local command handled by the TUI before any
// prompt is sent to the model.
type SlashCommand struct {
	Name        string
	Usage       string
	Description string
	Run         func(args string) (SlashCommandResult, error)
}

type SlashCommandResult struct {
	Output       string
	ClearSession bool
	Quit         bool
}

func normalizeSlashName(name string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "/")
}

func (m *Model) RegisterSlashCommand(command SlashCommand) error {
	name := normalizeSlashName(command.Name)
	if name == "" || strings.ContainsAny(name, " \t\r\n/") {
		return fmt.Errorf("invalid slash command name %q", command.Name)
	}
	if command.Run == nil {
		return fmt.Errorf("slash command %q requires a handler", name)
	}
	command.Name = name
	command.Usage = strings.TrimSpace(command.Usage)
	command.Description = strings.TrimSpace(command.Description)
	if command.Usage == "" {
		command.Usage = "/" + name
	}
	if m.slashCommands == nil {
		m.slashCommands = make(map[string]SlashCommand)
	}
	m.slashCommands[name] = command
	m.updateSlashAutocomplete()
	return nil
}

func (m *Model) registerDefaultSlashCommands() {
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "help",
		Usage:       "/help",
		Description: "show available local commands",
		Run: func(string) (SlashCommandResult, error) {
			return SlashCommandResult{Output: m.slashHelp()}, nil
		},
	})
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "clear",
		Usage:       "/clear",
		Description: "clear the transcript and model conversation history",
		Run: func(string) (SlashCommandResult, error) {
			return SlashCommandResult{ClearSession: true}, nil
		},
	})
	_ = m.RegisterSlashCommand(SlashCommand{
		Name:        "exit",
		Usage:       "/exit",
		Description: "exit the TUI",
		Run: func(args string) (SlashCommandResult, error) {
			if strings.TrimSpace(args) != "" {
				return SlashCommandResult{}, fmt.Errorf("/exit does not accept arguments")
			}
			return SlashCommandResult{Quit: true}, nil
		},
	})
}

func (m *Model) slashHelp() string {
	commands := make([]SlashCommand, 0, len(m.slashCommands))
	for _, command := range m.slashCommands {
		commands = append(commands, command)
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Name < commands[j].Name })

	var b strings.Builder
	b.WriteString("local commands")
	for _, command := range commands {
		b.WriteString("\n  ")
		b.WriteString(command.Usage)
		if command.Description != "" {
			b.WriteString(" — ")
			b.WriteString(command.Description)
		}
	}
	b.WriteString("\n\nAutocomplete: ↑/↓ select, Tab or Enter complete. Use //text to send /text to the model instead.")
	return b.String()
}

func parseSlashCommand(input string) (name, args string, ok bool) {
	input = strings.TrimSpace(input)
	if len(input) < 2 || input[0] != '/' || input[1] == '/' {
		return "", "", false
	}
	body := strings.TrimSpace(input[1:])
	if body == "" {
		return "", "", false
	}
	if index := strings.IndexAny(body, " \t\r\n"); index >= 0 {
		return normalizeSlashName(body[:index]), strings.TrimSpace(body[index+1:]), true
	}
	return normalizeSlashName(body), "", true
}

func unescapeSlashPrompt(input string) string {
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "//") {
		return trimmed[1:]
	}
	return input
}

func (m *Model) runSlashCommand(input string) (bool, tea.Cmd) {
	name, args, ok := parseSlashCommand(input)
	if !ok {
		return false, nil
	}

	command, exists := m.slashCommands[name]
	if !exists {
		m.messages = append(m.messages,
			Message{Role: roleUser, Content: input, Local: true},
			Message{Role: roleAssistant, Content: fmt.Sprintf("unknown local command /%s\n\nUse /help to list available commands.", name), Local: true},
		)
		return true, nil
	}

	result, err := command.Run(args)
	if err != nil {
		m.messages = append(m.messages,
			Message{Role: roleUser, Content: input, Local: true},
			Message{Role: roleAssistant, Content: "error: " + err.Error(), Local: true},
		)
		return true, nil
	}
	if result.ClearSession {
		m.messages = nil
		m.history = nil
		m.streamAssistantText = ""
		m.streamReasoningText = ""
		m.toolActivity = ""
		m.thinkingVisible = false
	}
	if result.Quit {
		return true, tea.Quit
	}
	if result.ClearSession {
		return true, nil
	}

	m.messages = append(m.messages, Message{Role: roleUser, Content: input, Local: true})
	output := strings.TrimSpace(result.Output)
	if output != "" {
		m.messages = append(m.messages, Message{Role: roleAssistant, Content: output, Local: true})
	}
	return true, nil
}

func (m *Model) updateSlashAutocomplete() {
	query, ok := slashAutocompleteQuery(m.composer.Value())
	if !ok {
		m.slashMatches = nil
		m.slashMatchIndex = 0
		return
	}

	matches := make([]SlashCommand, 0, len(m.slashCommands))
	for _, command := range m.slashCommands {
		if strings.HasPrefix(command.Name, query) {
			matches = append(matches, command)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
	m.slashMatches = matches
	if len(matches) == 0 {
		m.slashMatchIndex = 0
		return
	}
	if m.slashMatchIndex >= len(matches) {
		m.slashMatchIndex = len(matches) - 1
	}
}

func slashAutocompleteQuery(input string) (string, bool) {
	input = strings.TrimLeft(input, " \t")
	if !strings.HasPrefix(input, "/") || strings.HasPrefix(input, "//") {
		return "", false
	}
	body := strings.TrimPrefix(input, "/")
	if strings.ContainsAny(body, " \t\r\n") {
		return "", false
	}
	return normalizeSlashName(body), true
}

func (m *Model) currentSlashCommandIsExact() bool {
	query, ok := slashAutocompleteQuery(m.composer.Value())
	if !ok || query == "" {
		return false
	}
	_, exists := m.slashCommands[query]
	return exists
}

func (m *Model) completeSlashAutocomplete() {
	if len(m.slashMatches) == 0 {
		return
	}
	index := min(max(0, m.slashMatchIndex), len(m.slashMatches)-1)
	command := m.slashMatches[index]
	value := "/" + command.Name
	if len(strings.Fields(command.Usage)) > 1 {
		value += " "
	}
	m.composer.SetValue(value)
	m.composer.CursorEnd()
	m.updateSlashAutocomplete()
}

func (m *Model) renderSlashAutocomplete(width int) string {
	if len(m.slashMatches) == 0 || width <= 0 {
		return ""
	}
	const maxVisible = 5
	limit := min(maxVisible, len(m.slashMatches))
	start := 0
	if m.slashMatchIndex >= limit {
		start = m.slashMatchIndex - limit + 1
	}
	end := min(len(m.slashMatches), start+limit)

	var b strings.Builder
	for i := start; i < end; i++ {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		command := m.slashMatches[i]
		line := command.Usage
		if command.Description != "" {
			line += " — " + command.Description
		}
		marker := "  "
		style := mutedStyle
		if i == m.slashMatchIndex {
			marker = "› "
			style = statusNameStyle
		}
		b.WriteString(style.Render(truncate(marker+line, width)))
	}
	return b.String()
}
