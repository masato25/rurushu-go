package projectstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/arborlogic/rurushu-go/provider"
)

func TestOpenCreatesPrivateProjectStateAndSession(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.State()
	if err != nil || state.ProjectID == "" || state.Version != Version {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	session, err := store.StartSession("test-model", "openai-compatible")
	if err != nil {
		t.Fatal(err)
	}
	if session.ID == "" {
		t.Fatal("session id is empty")
	}
	state, err = store.State()
	if err != nil || state.LastSession != session.ID {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	for _, path := range []string{
		filepath.Join(root, ".rurushu", "state.json"),
		filepath.Join(root, ".rurushu", "sessions", session.ID+".json"),
		filepath.Join(root, ".rurushu", ".gitignore"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s permissions=%o, want private", path, info.Mode().Perm())
		}
	}
}

func TestOpenRejectsSymlinkedStateDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".rurushu")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("expected symlinked .rurushu to be rejected")
	}
}

func TestSessionHistoryRoundTripAndFork(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("model-a", "openai-compatible")
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{
		{Role: provider.RoleUser, Content: "inspect"},
		{Role: provider.RoleAssistant, ReasoningContent: "hidden", ToolCalls: []provider.ToolCall{{ID: "call-1", Type: "function", Function: provider.FunctionCall{Name: "read", Arguments: `{"path":"README.md"}`}}}},
		{Role: provider.RoleTool, ToolCallID: "call-1", Name: "read", Content: "failed", ToolIsError: true},
		{Role: provider.RoleAssistant, Content: "done"},
	}
	saved, err := store.SaveSession(session.ID, "model-b", "openai-compatible", history)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ProjectID == "" || saved.Version != SessionVersion {
		t.Fatalf("saved=%+v", saved)
	}
	loaded, err := store.LoadSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantHistory := cloneMessages(history)
	wantHistory[1].ReasoningContent = ""
	if !reflect.DeepEqual(loaded.Messages, wantHistory) {
		t.Fatalf("loaded history mismatch\n got=%#v\nwant=%#v", loaded.Messages, wantHistory)
	}
	if loaded.Model != "model-b" {
		t.Fatalf("model=%q", loaded.Model)
	}

	fork, err := store.ForkSession(session.ID, 1, "model-b", "openai-compatible", history[:0])
	if err != nil {
		t.Fatal(err)
	}
	if fork.ID == session.ID || fork.ParentSession != session.ID || fork.RewoundFromAsk != 1 {
		t.Fatalf("fork=%+v", fork)
	}
	last, err := store.LastSession()
	if err != nil || last.ID != fork.ID {
		t.Fatalf("last=%+v err=%v", last, err)
	}
	all, err := store.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("sessions=%d", len(all))
	}
}

func TestLoadLegacyMetadataOnlySession(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "ses_0123456789abcdef"
	legacy := map[string]any{
		"id":       id,
		"model":    "old-model",
		"provider": "openai-compatible",
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "sessions", id+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := store.LoadSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if session.Version != SessionVersion || len(session.Messages) != 0 {
		t.Fatalf("legacy session=%+v", session)
	}
}

func TestSessionIDTraversalAndSymlinkAreRejected(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../outside", "ses_bad", "ses_0123456789abcdef.json"} {
		if _, err := store.LoadSession(id); err == nil {
			t.Fatalf("LoadSession(%q) succeeded", id)
		}
	}

	id := "ses_0123456789abcdef"
	target := filepath.Join(root, "outside.json")
	if err := os.WriteFile(target, []byte(`{"id":"ses_0123456789abcdef"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(store.Dir, "sessions", id+".json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := store.LoadSession(id); err == nil {
		t.Fatal("expected symlinked session file to be rejected")
	}
}
