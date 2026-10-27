package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/masato25/rurushu-go/duel"
	"github.com/masato25/rurushu-go/projectstate"
)

func TestDuelRejectsMissingGoal(t *testing.T) {
	if err := runDuel([]string{"--cwd", t.TempDir()}); err == nil {
		t.Fatal("missing goal accepted")
	}
}
func TestDuelResumeRequiresState(t *testing.T) {
	if err := runDuel([]string{"--cwd", t.TempDir(), "--resume"}); err == nil {
		t.Fatal("missing state accepted")
	}
}

func TestDuelLockRejectsSecondInstanceInSameWorkspace(t *testing.T) {
	store, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := acquireDuelLock(store.Dir, store.ProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := acquireDuelLock(store.Dir, store.ProjectRoot); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("second instance was not rejected: %v", err)
	}
}

func TestDuelLockAllowsDifferentWorkspaces(t *testing.T) {
	storeA, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := acquireDuelLock(storeA.Dir, storeA.ProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := acquireDuelLock(storeB.Dir, storeB.ProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
}

func TestDuelLockReclaimsStaleOwner(t *testing.T) {
	store, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(store.Dir, "duel-active.lock")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(duelLockMeta{PID: 2147483647, StartedAt: time.Now().Add(-time.Hour), CWD: store.ProjectRoot, Token: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "owner.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireDuelLock(store.Dir, store.ProjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	owner, err := os.ReadFile(filepath.Join(dir, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta duelLockMeta
	if err := json.Unmarshal(owner, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.PID != os.Getpid() || meta.Token == "stale" {
		t.Fatalf("stale lock was not replaced: %+v", meta)
	}
}

func TestDuelLockCanRestartAfterReleaseForErrorOrInterrupt(t *testing.T) {
	for _, outcome := range []string{"error", "interrupted"} {
		t.Run(outcome, func(t *testing.T) {
			store, err := projectstate.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			first, err := acquireDuelLock(store.Dir, store.ProjectRoot)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Release(); err != nil {
				t.Fatal(err)
			}
			second, err := acquireDuelLock(store.Dir, store.ProjectRoot)
			if err != nil {
				t.Fatalf("restart after %s failed: %v", outcome, err)
			}
			defer second.Release()
		})
	}
}

func TestResolveDuelStateReusesExistingPairByDefault(t *testing.T) {
	dir := t.TempDir()
	store, err := projectstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	left, err := store.StartSession("chatgpt-web", "cos")
	if err != nil {
		t.Fatal(err)
	}
	right, err := store.StartSession("chatgpt-web", "cos")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, "duel-state.json")
	want := duelState{Goal: "keep improving", LeftID: left.ID, RightID: right.ID, Rounds: 4, Status: "interrupted", Summary: "saved"}
	if err := saveDuelState(path, want); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(filepath.Join(store.Dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	got, reused, err := resolveDuelState(store, path, "keep improving", false, false)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(filepath.Join(store.Dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if !reused || got != want {
		t.Fatalf("got=%+v reused=%v want=%+v", got, reused, want)
	}
	if len(after) != len(before) {
		t.Fatalf("default resume created sessions: before=%d after=%d", len(before), len(after))
	}
}

func TestResolveDuelStateRequiresNewForDifferentGoal(t *testing.T) {
	dir := t.TempDir()
	store, err := projectstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := store.StartSession("chatgpt-web", "cos")
	right, _ := store.StartSession("chatgpt-web", "cos")
	path := filepath.Join(store.Dir, "duel-state.json")
	if err := saveDuelState(path, duelState{Goal: "old goal", LeftID: left.ID, RightID: right.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveDuelState(store, path, "new goal", false, false); err == nil || !strings.Contains(err.Error(), "--new") {
		t.Fatalf("different goal should require --new, got %v", err)
	}
}

func TestResolveDuelStateNewCreatesFreshPair(t *testing.T) {
	dir := t.TempDir()
	store, err := projectstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	oldLeft, _ := store.StartSession("chatgpt-web", "cos")
	oldRight, _ := store.StartSession("chatgpt-web", "cos")
	path := filepath.Join(store.Dir, "duel-state.json")
	if err := saveDuelState(path, duelState{Goal: "old", LeftID: oldLeft.ID, RightID: oldRight.ID}); err != nil {
		t.Fatal(err)
	}
	got, reused, err := resolveDuelState(store, path, "new", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if reused || got.Goal != "new" || got.LeftID == oldLeft.ID || got.RightID == oldRight.ID || got.LeftID == got.RightID {
		t.Fatalf("fresh duel not created correctly: %+v reused=%v", got, reused)
	}
}

func TestResolveDuelStateRejectsResumeAndNew(t *testing.T) {
	store, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveDuelState(store, filepath.Join(store.Dir, "duel-state.json"), "goal", true, true); err == nil {
		t.Fatal("--resume --new accepted")
	}
}
func TestDuelStatePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duel-state.json")
	want := duelState{Goal: "goal", LeftID: "l", RightID: "r", Rounds: 3}
	if err := saveDuelState(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v", info.Mode())
	}
}

func TestLoadDuelStatePreservesResumeContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duel-state.json")
	want := duelState{
		Goal:    "continue improving",
		LeftID:  "left-session",
		RightID: "right-session",
		Rounds:  7,
		Status:  "running",
		Summary: "round seven uncovered an edge case",
	}
	if err := saveDuelState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadDuelState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resume state mismatch: got %+v want %+v", got, want)
	}
}

func TestLoadDuelStateRejectsInvalidSavedSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duel-state.json")
	if err := os.WriteFile(path, []byte(`{"goal":"g","left_session":"same","right_session":"same"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDuelState(path); err == nil {
		t.Fatal("invalid saved duel state accepted")
	}
}

func TestApplyDuelResultPersistsTerminalState(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		result     duel.Result
		wantStatus string
		wantSum    string
	}{
		{name: "owner review", result: duel.Result{Status: "owner_review", Summary: "verified"}, wantStatus: "owner_review", wantSum: "verified"},
		{name: "cancelled", err: context.Canceled, wantStatus: "interrupted", wantSum: "previous"},
		{name: "provider error", err: errors.New("provider failed"), wantStatus: "error", wantSum: "previous"},
		{name: "error with partial summary", err: errors.New("provider failed"), result: duel.Result{Summary: "partial"}, wantStatus: "error", wantSum: "partial"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := duelState{Status: "running", Summary: "previous"}
			applyDuelResult(&state, tt.result, tt.err)
			if state.Status != tt.wantStatus || state.Summary != tt.wantSum {
				t.Fatalf("state=%+v want status=%q summary=%q", state, tt.wantStatus, tt.wantSum)
			}
		})
	}
}
