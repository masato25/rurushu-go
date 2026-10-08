package tui

import (
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"github.com/arborlogic/rurushu-go/projectstate"
	"github.com/arborlogic/rurushu-go/provider"
)

func TestProjectSessionRestoreRebuildsExactHistoryAndToolTranscript(t *testing.T) {
	store, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("test-model", "openai-compatible")
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "inspect"},
		{Role: provider.RoleAssistant, ReasoningContent: "do not render this", ToolCalls: []provider.ToolCall{{ID: "call-1", Type: "function", Function: provider.FunctionCall{Name: "read", Arguments: `{"path":"README.md"}`}}}},
		{Role: provider.RoleTool, ToolCallID: "call-1", Name: "read", Content: "path is required", ToolIsError: true},
		{Role: provider.RoleAssistant, Content: "I could not read it."},
	}
	session, err = store.SaveSession(session.ID, "test-model", "openai-compatible", history)
	if err != nil {
		t.Fatal(err)
	}
	history[1].ReasoningContent = ""

	m := New("test-model", "openai-compatible")
	m.SetActivityMode(ActivityDebug)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	m.SetProjectSession(store, session)
	if !reflect.DeepEqual(m.history, history) {
		t.Fatalf("history mismatch\n got=%#v\nwant=%#v", m.history, history)
	}
	m.refreshConversation()
	view := m.viewport.View()
	for _, want := range []string{"inspect", "README.md", "path is required", "I could not read it."} {
		if !strings.Contains(view, want) {
			t.Fatalf("restored transcript missing %q:\n%s", want, view)
		}
	}
	if len(m.messages) < 2 || m.messages[1].Activity == nil || !m.messages[1].Activity.Done || !m.messages[1].Activity.IsError {
		t.Fatalf("restored tool activity lost error state: %#v", m.messages)
	}
	if strings.Contains(view, "do not render this") {
		t.Fatalf("reasoning leaked into restored transcript: %q", view)
	}
}

func TestRewindForksSessionAndLoadsAskIntoComposer(t *testing.T) {
	store, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original, err := store.StartSession("test-model", "openai-compatible")
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "first ask"},
		{Role: provider.RoleAssistant, Content: "first answer"},
		{Role: provider.RoleUser, Content: "second ask to edit"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "call-2", Type: "function", Function: provider.FunctionCall{Name: "glob", Arguments: `{"pattern":"*"}`}}}},
		{Role: provider.RoleTool, ToolCallID: "call-2", Name: "glob", Content: "README.md"},
		{Role: provider.RoleAssistant, Content: "second answer"},
		{Role: provider.RoleUser, Content: "third ask"},
		{Role: provider.RoleAssistant, Content: "third answer"},
	}
	original, err = store.SaveSession(original.ID, "test-model", "openai-compatible", history)
	if err != nil {
		t.Fatal(err)
	}

	m := New("test-model", "openai-compatible")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	m.SetProjectSession(store, original)
	if cmd := submitSlashTest(t, m, "/rewind 2"); cmd != nil {
		t.Fatal("/rewind unexpectedly returned async command")
	}
	if m.SessionID() == original.ID || m.SessionID() == "" {
		t.Fatalf("rewind did not fork session: %q", m.SessionID())
	}
	if got := m.composer.Value(); got != "second ask to edit" {
		t.Fatalf("rewound composer=%q", got)
	}
	wantPrefix := history[:2]
	if !reflect.DeepEqual(m.history, wantPrefix) {
		t.Fatalf("rewound history=%#v, want %#v", m.history, wantPrefix)
	}
	fork, err := store.LoadSession(m.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if fork.ParentSession != original.ID || fork.RewoundFromAsk != 2 || !reflect.DeepEqual(fork.Messages, wantPrefix) {
		t.Fatalf("fork=%+v", fork)
	}
	preserved, err := store.LoadSession(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(preserved.Messages, history) {
		t.Fatal("rewind modified original session")
	}

	if cmd := submitSlashTest(t, m, "/resume "+original.ID); cmd != nil {
		t.Fatal("/resume unexpectedly returned async command")
	}
	if m.SessionID() != original.ID || !reflect.DeepEqual(m.history, history) {
		t.Fatalf("resume failed: id=%q history=%#v", m.SessionID(), m.history)
	}
}

func TestCompletedTurnPersistsAndClearUpdatesSession(t *testing.T) {
	store, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("test-model", "openai-compatible")
	if err != nil {
		t.Fatal(err)
	}
	m := New("test-model", "openai-compatible")
	m.SetProjectSession(store, session)
	m.history = append(m.history, provider.Message{Role: provider.RoleUser, Content: "hello"})
	m.streamAssistantText = "world"
	m.streaming = true
	m.applyStreamEvent(provider.StreamEvent{Type: provider.EventDone})

	loaded, err := store.LoadSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.Message{{Role: provider.RoleUser, Content: "hello"}, {Role: provider.RoleAssistant, Content: "world"}}
	if !reflect.DeepEqual(loaded.Messages, want) {
		t.Fatalf("persisted=%#v want=%#v", loaded.Messages, want)
	}

	if cmd := submitSlashTest(t, m, "/clear"); cmd != nil {
		t.Fatal("/clear unexpectedly returned async command")
	}
	loaded, err = store.LoadSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 0 {
		t.Fatalf("clear did not persist: %#v", loaded.Messages)
	}
}

func TestSessionSlashCommandsAppearInHelp(t *testing.T) {
	store, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("test-model", "openai-compatible")
	if err != nil {
		t.Fatal(err)
	}
	m := New("test-model", "openai-compatible")
	m.SetProjectSession(store, session)
	help := m.slashHelp()
	for _, command := range []string{"/sessions", "/resume", "/new", "/rewind"} {
		if !strings.Contains(help, command) {
			t.Fatalf("help missing %s: %s", command, help)
		}
	}
}

func TestResumePickerSupportsArrowSelectionAndEnter(t *testing.T) {
	store, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.StartSession("test-model", "openai-compatible")
	if err != nil {
		t.Fatal(err)
	}
	firstHistory := []provider.Message{
		{Role: provider.RoleUser, Content: "first session ask"},
		{Role: provider.RoleAssistant, Content: "first session answer"},
	}
	first, err = store.SaveSession(first.ID, "test-model", "openai-compatible", firstHistory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.StartSession("test-model", "openai-compatible")
	if err != nil {
		t.Fatal(err)
	}
	secondHistory := []provider.Message{
		{Role: provider.RoleUser, Content: "second session ask"},
		{Role: provider.RoleAssistant, Content: "second session answer"},
	}
	second, err = store.SaveSession(second.ID, "test-model", "openai-compatible", secondHistory)
	if err != nil {
		t.Fatal(err)
	}

	m := New("test-model", "openai-compatible")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	m.SetProjectSession(store, second)
	if cmd := submitSlashTest(t, m, "/resume"); cmd != nil {
		t.Fatal("/resume picker unexpectedly returned async command")
	}
	if m.selectionPicker == nil || len(m.selectionPicker.Items) != 2 {
		t.Fatalf("resume picker=%#v", m.selectionPicker)
	}
	if !strings.Contains(m.selectionPicker.Items[0].Label, second.ID) {
		t.Fatalf("newest/current session was not first: %#v", m.selectionPicker.Items)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if m.selectionPicker != nil {
		t.Fatal("resume picker stayed open after Enter")
	}
	if m.SessionID() != first.ID || !reflect.DeepEqual(m.history, firstHistory) {
		t.Fatalf("selected session not restored: id=%q history=%#v", m.SessionID(), m.history)
	}
	if len(m.messages) < 2 || m.messages[len(m.messages)-2].Content != "/resume "+first.ID {
		t.Fatalf("selected resume action missing from transcript: %#v", m.messages)
	}
}

func TestRewindPickerSupportsArrowSelectionAndEnter(t *testing.T) {
	store, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("test-model", "openai-compatible")
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "first ask"},
		{Role: provider.RoleAssistant, Content: "first answer"},
		{Role: provider.RoleUser, Content: "second ask to revise"},
		{Role: provider.RoleAssistant, Content: "second answer"},
		{Role: provider.RoleUser, Content: "third ask"},
		{Role: provider.RoleAssistant, Content: "third answer"},
	}
	session, err = store.SaveSession(session.ID, "test-model", "openai-compatible", history)
	if err != nil {
		t.Fatal(err)
	}

	m := New("test-model", "openai-compatible")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	m.SetProjectSession(store, session)
	if cmd := submitSlashTest(t, m, "/rewind"); cmd != nil {
		t.Fatal("/rewind picker unexpectedly returned async command")
	}
	if m.selectionPicker == nil || len(m.selectionPicker.Items) != 3 {
		t.Fatalf("rewind picker=%#v", m.selectionPicker)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}))
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if m.selectionPicker != nil {
		t.Fatal("rewind picker stayed open after Enter")
	}
	if got := m.composer.Value(); got != "second ask to revise" {
		t.Fatalf("rewind selected wrong ask: %q", got)
	}
	if len(m.history) != 2 || m.history[0].Content != "first ask" || m.history[1].Content != "first answer" {
		t.Fatalf("rewound history=%#v", m.history)
	}
	if m.SessionID() == session.ID {
		t.Fatal("rewind picker did not fork session")
	}
}

func TestSelectionPickerEscClosesWithoutAction(t *testing.T) {
	m := New("test-model", "openai-compatible")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	run := false
	m.openSelectionPicker(&selectionPicker{Title: "test", Items: []selectionPickerItem{{
		Label: "item",
		Run: func() (string, error) {
			run = true
			return "", nil
		},
	}}})
	if m.selectionPicker == nil || m.composer.Focused() {
		t.Fatal("picker did not take input focus")
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape}))
	if m.selectionPicker != nil || !m.composer.Focused() || run {
		t.Fatalf("Esc did not close picker cleanly: picker=%#v focused=%v run=%v", m.selectionPicker, m.composer.Focused(), run)
	}
}
