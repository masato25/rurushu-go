package tui

import (
	"fmt"
	"sort"
	"strings"
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
	b.WriteString("\n\nUse //text to send /text to the model instead of treating it as a local command.")
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

func (m *Model) runSlashCommand(input string) bool {
	name, args, ok := parseSlashCommand(input)
	if !ok {
		return false
	}

	command, exists := m.slashCommands[name]
	if !exists {
		m.messages = append(m.messages,
			Message{Role: roleUser, Content: input, Local: true},
			Message{Role: roleAssistant, Content: fmt.Sprintf("unknown local command /%s\n\nUse /help to list available commands.", name), Local: true},
		)
		return true
	}

	result, err := command.Run(args)
	if result.ClearSession {
		m.messages = nil
		m.history = nil
		m.streamAssistantText = ""
		m.streamReasoningText = ""
		m.toolActivity = ""
		m.thinkingVisible = false
		return true
	}

	m.messages = append(m.messages, Message{Role: roleUser, Content: input, Local: true})
	output := strings.TrimSpace(result.Output)
	if err != nil {
		output = "error: " + err.Error()
	}
	if output != "" {
		m.messages = append(m.messages, Message{Role: roleAssistant, Content: output, Local: true})
	}
	return true
}
