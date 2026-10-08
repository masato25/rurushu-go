package projectstate

import (
	"os"
	"path/filepath"
	"testing"
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
