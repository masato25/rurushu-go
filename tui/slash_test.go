package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/arborlogic/rurushu-go/provider"
)

type slashTestStreamer struct {
	req provider.CompletionRequest
}

func (s *slashTestStreamer) Stream(_ context.Context, req provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	s.req = req
	ch := make(chan provider.StreamEvent, 1)
	ch <- provider.StreamEvent{Type: provider.EventDone}
	close(ch)
	return ch, nil
}

func submitSlashTest(t *testing.T, m *Model, value string) tea.Cmd {
	t.Helper()
	m.composer.SetValue(value)
	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	return cmd
}

func TestDefaultSlashHelpIsLocal(t *testing.T) {
	streamer := &slashTestStreamer{}
	m := NewWithStreamer("test-model", "test-provider", streamer)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	if cmd := submitSlashTest(t, m, "/help"); cmd != nil {
		t.Fatal("/help unexpectedly started async work")
	}
	if len(streamer.req.Messages) != 0 {
		t.Fatalf("/help reached model: %#v", streamer.req.Messages)
	}
	if len(m.messages) != 2 {
		t.Fatalf("messages=%d, want 2", len(m.messages))
	}
	output := m.messages[1].Content
	for _, want := range []string{"/clear", "/exit", "/help", "Autocomplete:", "//text"} {
		if !strings.Contains(output, want) {
			t.Fatalf("help output missing %q: %q", want, output)
		}
	}
}

func TestCustomSlashCommandReceivesArguments(t *testing.T) {
	m := New("test-model", "test-provider")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	var got string
	err := m.RegisterSlashCommand(SlashCommand{
		Name:        "echo",
		Usage:       "/echo <text>",
		Description: "echo local text",
		Run: func(args string) (SlashCommandResult, error) {
			got = args
			return SlashCommandResult{Output: "local: " + args}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if cmd := submitSlashTest(t, m, "/echo hello world"); cmd != nil {
		t.Fatal("custom slash command unexpectedly started async work")
	}
	if got != "hello world" {
		t.Fatalf("args=%q", got)
	}
	if len(m.messages) != 2 || m.messages[1].Content != "local: hello world" {
		t.Fatalf("unexpected transcript: %#v", m.messages)
	}
	if len(m.history) != 0 {
		t.Fatalf("local command polluted model history: %#v", m.history)
	}
}

func TestUnknownSlashCommandDoesNotReachModel(t *testing.T) {
	streamer := &slashTestStreamer{}
	m := NewWithStreamer("test-model", "test-provider", streamer)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	if cmd := submitSlashTest(t, m, "/missing whatever"); cmd != nil {
		t.Fatal("unknown slash command unexpectedly started async work")
	}
	if len(streamer.req.Messages) != 0 {
		t.Fatalf("unknown command reached model: %#v", streamer.req.Messages)
	}
	if len(m.messages) != 2 || !strings.Contains(m.messages[1].Content, "unknown local command /missing") {
		t.Fatalf("unexpected transcript: %#v", m.messages)
	}
}

func TestDoubleSlashEscapesToModelPrompt(t *testing.T) {
	streamer := &slashTestStreamer{}
	m := NewWithStreamer("test-model", "test-provider", streamer)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	cmd := submitSlashTest(t, m, "//help")
	if cmd == nil || !m.streaming {
		t.Fatal("escaped slash prompt did not start model stream")
	}
	_ = cmd()
	if len(streamer.req.Messages) != 1 || streamer.req.Messages[0].Content != "/help" {
		t.Fatalf("escaped slash prompt mismatch: %#v", streamer.req.Messages)
	}
}

func TestClearSlashCommandResetsTranscriptAndHistory(t *testing.T) {
	m := New("test-model", "test-provider")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.messages = append(m.messages, Message{Role: roleUser, Content: "old"})
	m.history = append(m.history, provider.Message{Role: provider.RoleUser, Content: "old"})

	if cmd := submitSlashTest(t, m, "/clear"); cmd != nil {
		t.Fatal("/clear unexpectedly started async work")
	}
	if len(m.messages) != 0 || len(m.history) != 0 {
		t.Fatalf("/clear did not reset session: messages=%#v history=%#v", m.messages, m.history)
	}
}

func TestRegisterSlashCommandValidation(t *testing.T) {
	m := New("test-model", "test-provider")
	for _, command := range []SlashCommand{
		{Name: ""},
		{Name: "bad name", Run: func(string) (SlashCommandResult, error) { return SlashCommandResult{}, nil }},
		{Name: "ok"},
	} {
		if err := m.RegisterSlashCommand(command); err == nil {
			t.Fatalf("RegisterSlashCommand(%q) succeeded unexpectedly", command.Name)
		}
	}
}

func TestSlashAutocompleteFiltersAndCompletesWithEnter(t *testing.T) {
	m := New("test-model", "test-provider")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.composer.SetValue("/he")
	m.updateSlashAutocomplete()
	m.refreshConversation()

	if len(m.slashMatches) != 1 || m.slashMatches[0].Name != "help" {
		t.Fatalf("matches = %#v", m.slashMatches)
	}
	if view := m.viewport.View(); !strings.Contains(view, "/help") {
		t.Fatalf("autocomplete not rendered: %q", view)
	}

	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if cmd != nil {
		t.Fatal("partial slash autocomplete unexpectedly started async work")
	}
	if got := m.composer.Value(); got != "/help" {
		t.Fatalf("completed value = %q", got)
	}
	if len(m.messages) != 0 {
		t.Fatalf("partial autocomplete executed command: %#v", m.messages)
	}

	_, cmd = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if cmd != nil {
		t.Fatal("exact /help unexpectedly started async work")
	}
	if len(m.messages) != 2 || !strings.Contains(m.messages[1].Content, "local commands") {
		t.Fatalf("exact /help did not execute: %#v", m.messages)
	}
}

func TestSlashAutocompleteNavigationAndTab(t *testing.T) {
	m := New("test-model", "test-provider")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.composer.SetValue("/")
	m.updateSlashAutocomplete()
	if len(m.slashMatches) < 3 || m.slashMatches[0].Name != "clear" || m.slashMatches[1].Name != "exit" || m.slashMatches[2].Name != "help" {
		t.Fatalf("unexpected sorted matches: %#v", m.slashMatches)
	}

	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	if m.slashMatchIndex != 2 {
		t.Fatalf("selected index = %d", m.slashMatchIndex)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	if got := m.composer.Value(); got != "/help" {
		t.Fatalf("tab completion = %q", got)
	}
}

func TestExitSlashCommandQuitsLocally(t *testing.T) {
	streamer := &slashTestStreamer{}
	m := NewWithStreamer("test-model", "test-provider", streamer)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	cmd := submitSlashTest(t, m, "/exit")
	if cmd == nil {
		t.Fatal("/exit did not return quit command")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("/exit command returned %T, want tea.QuitMsg", msg)
	}
	if len(streamer.req.Messages) != 0 {
		t.Fatalf("/exit reached model: %#v", streamer.req.Messages)
	}
}

func TestExitSlashCommandRejectsArguments(t *testing.T) {
	m := New("test-model", "test-provider")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	if cmd := submitSlashTest(t, m, "/exit now"); cmd != nil {
		t.Fatal("/exit with arguments unexpectedly quit")
	}
	if len(m.messages) != 2 || !strings.Contains(m.messages[1].Content, "/exit does not accept arguments") {
		t.Fatalf("unexpected /exit argument error: %#v", m.messages)
	}
}

func TestCustomSlashCommandParticipatesInAutocomplete(t *testing.T) {
	m := New("test-model", "test-provider")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	err := m.RegisterSlashCommand(SlashCommand{
		Name:        "project",
		Usage:       "/project <name>",
		Description: "select project",
		Run: func(string) (SlashCommandResult, error) {
			return SlashCommandResult{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m.composer.SetValue("/pro")
	m.updateSlashAutocomplete()
	if len(m.slashMatches) != 1 || m.slashMatches[0].Name != "project" {
		t.Fatalf("custom command matches = %#v", m.slashMatches)
	}
	m.completeSlashAutocomplete()
	if got := m.composer.Value(); got != "/project " {
		t.Fatalf("custom completion = %q", got)
	}
	if len(m.slashMatches) != 0 {
		t.Fatalf("autocomplete should close once arguments begin: %#v", m.slashMatches)
	}
}

func TestDoubleSlashDoesNotAutocomplete(t *testing.T) {
	m := New("test-model", "test-provider")
	m.composer.SetValue("//he")
	m.updateSlashAutocomplete()
	if len(m.slashMatches) != 0 {
		t.Fatalf("double slash unexpectedly autocompleted: %#v", m.slashMatches)
	}
}
