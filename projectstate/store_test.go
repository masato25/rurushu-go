package projectstate

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestSessionJournalAutomaticCompactionAndCacheInvalidation(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("model", "cos")
	if err != nil {
		t.Fatal(err)
	}
	history := []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("x", int(sessionCompactJournalBytes)+20)}}
	if _, err = store.SaveSession(session.ID, "model", "cos", history); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(store.deltaPath(session.ID)); !os.IsNotExist(err) {
		t.Fatalf("compacted journal should be absent: %v", err)
	}
	loaded, err := store.LoadSession(session.ID)
	if err != nil || !reflect.DeepEqual(loaded.Messages, history) {
		t.Fatalf("compacted session: %v", err)
	}
	// An independent Store appends to this session: cached history must refresh.
	other, err := Open(store.ProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	external := append(append([]provider.Message(nil), history...), provider.Message{Role: provider.RoleAssistant, Content: "external"})
	if _, err = other.SaveSession(session.ID, "model", "cos", external); err != nil {
		t.Fatal(err)
	}
	external = append(external, provider.Message{Role: provider.RoleUser, Content: "next"})
	if _, err = store.SaveSession(session.ID, "model", "cos", external); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.LoadSession(session.ID)
	if err != nil || !reflect.DeepEqual(loaded.Messages, external) {
		t.Fatalf("cache invalidation: %v", err)
	}
}

func BenchmarkSessionIncrementalSave(b *testing.B) {
	store, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	session, err := store.StartSession("model", "cos")
	if err != nil {
		b.Fatal(err)
	}
	history := make([]provider.Message, 1000)
	for i := range history {
		history[i] = provider.Message{Role: provider.RoleUser, Content: strings.Repeat("x", 128)}
	}
	if _, err = store.SaveSession(session.ID, "model", "cos", history); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		history = append(history, provider.Message{Role: provider.RoleAssistant, Content: "reply"})
		if _, err = store.SaveSession(session.ID, "model", "cos", history); err != nil {
			b.Fatal(err)
		}
	}
}

func TestSessionLockCrossProcess(t *testing.T) {
	if os.Getenv("RURUSHU_LOCK_HELPER") == "1" {
		store, err := Open(os.Getenv("RURUSHU_LOCK_ROOT"))
		if err != nil {
			t.Fatal(err)
		}
		unlock, err := store.lockSession(os.Getenv("RURUSHU_LOCK_ID"))
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if err := os.WriteFile(os.Getenv("RURUSHU_LOCK_READY"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
		return
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("model", "cos")
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionLockCrossProcess$")
	cmd.Env = append(os.Environ(), "RURUSHU_LOCK_HELPER=1", "RURUSHU_LOCK_ROOT="+store.ProjectRoot, "RURUSHU_LOCK_ID="+session.ID, "RURUSHU_LOCK_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper never acquired lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	begin := time.Now()
	unlock, err := store.lockSession(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if time.Since(begin) < 150*time.Millisecond {
		t.Fatal("another process did not block on lock")
	}
}

func TestSessionOwnershipCrossProcess(t *testing.T) {
	if os.Getenv("RURUSHU_OWNER_HELPER") == "1" {
		store, err := Open(os.Getenv("RURUSHU_OWNER_ROOT"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.HoldSession(os.Getenv("RURUSHU_OWNER_ID")); err != nil {
			t.Fatal(err)
		}
		defer store.ReleaseSession()
		if err := os.WriteFile(os.Getenv("RURUSHU_OWNER_READY"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		return
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.StartSession("model", "cos")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.StartSession("model", "cos")
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionOwnershipCrossProcess$")
	cmd.Env = append(os.Environ(), "RURUSHU_OWNER_HELPER=1", "RURUSHU_OWNER_ROOT="+store.ProjectRoot, "RURUSHU_OWNER_ID="+first.ID, "RURUSHU_OWNER_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner helper did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if err := store.HoldSession(first.ID); err == nil {
		t.Fatal("another process owns the session")
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Fatal("ownership check blocked")
	}
	if err := store.HoldSession(second.ID); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := store.HoldSession(first.ID); err != nil {
		t.Fatalf("cannot acquire released lease: %v", err)
	}
	store.ReleaseSession()
}

func TestForceSessionOwnershipBypassesOnlyLease(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("model", "cos")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HoldSession(session.ID); err != nil {
		t.Fatal(err)
	}
	defer store.ReleaseSession()
	other, err := Open(store.ProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.HoldSession(session.ID); err == nil {
		t.Fatal("normal ownership unexpectedly succeeded")
	}
	other.SetForceSessionOwnership(true)
	if err := other.HoldSession(session.ID); err != nil {
		t.Fatalf("force ownership failed: %v", err)
	}
	history := []provider.Message{{Role: provider.RoleUser, Content: "forced continuation"}}
	if _, err := other.SaveSession(session.ID, "model", "cos", history); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadSession(session.ID)
	if err != nil || !reflect.DeepEqual(loaded.Messages, history) {
		t.Fatalf("forced history: %+v %v", loaded.Messages, err)
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
