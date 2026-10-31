package projectstate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/masato25/rurushu-go/provider"
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

func TestSaveCurrentSessionDoesNotRewriteProjectState(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("model", "cos")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "state.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveSession(session.ID, "model", "cos", []provider.Message{{Role: provider.RoleUser, Content: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("state.json was unnecessarily replaced")
	}
	loaded, err := store.LoadSession(session.ID)
	if err != nil || len(loaded.Messages) != 1 {
		t.Fatalf("session not saved: %+v %v", loaded, err)
	}
}

func TestSessionJournalAppendRestartAndSnapshotReset(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("model", "cos")
	if err != nil {
		t.Fatal(err)
	}
	base := store.sessionPath(session.ID)
	before, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{{Role: provider.RoleUser, Content: "one"}}
	if _, err := store.SaveSession(session.ID, "model", "cos", history); err != nil {
		t.Fatal(err)
	}
	history = append(history, provider.Message{Role: provider.RoleAssistant, Content: "two"})
	if _, err := store.SaveSession(session.ID, "model", "cos", history); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("append-only save changed the snapshot")
	}
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadSession(session.ID)
	if err != nil || !reflect.DeepEqual(loaded.Messages, history) {
		t.Fatalf("journal recovery got=%+v err=%v", loaded.Messages, err)
	}
	// A truncated tail is ignored; the next append removes it.
	f, err := os.OpenFile(store.deltaPath(session.ID), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"base_hash":"partial`)
	_ = f.Close()
	loaded, err = store.LoadSession(session.ID)
	if err != nil || len(loaded.Messages) != 2 {
		t.Fatalf("partial tail recovery: %v %+v", err, loaded.Messages)
	}
	history = append(history, provider.Message{Role: provider.RoleUser, Content: "three"})
	if _, err = store.SaveSession(session.ID, "model", "cos", history); err != nil {
		t.Fatal(err)
	}
	// Rewriting prior content must replace the snapshot, and old journal is stale.
	history[0].Content = "revised"
	if _, err = store.SaveSession(session.ID, "model", "cos", history); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.LoadSession(session.ID)
	if err != nil || !reflect.DeepEqual(loaded.Messages, history) {
		t.Fatalf("snapshot reset got=%+v err=%v", loaded.Messages, err)
	}
	history = append(history, provider.Message{Role: provider.RoleAssistant, Content: "four"})
	if _, err = store.SaveSession(session.ID, "model", "cos", history); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.LastSession()
	if err != nil || !reflect.DeepEqual(loaded.Messages, history) {
		t.Fatalf("after snapshot reset got=%+v err=%v", loaded.Messages, err)
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
