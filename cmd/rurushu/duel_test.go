package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/masato25/rurushu-go/duel"
	"github.com/masato25/rurushu-go/projectstate"
)

func TestResolveDuelStateRejectsMissingGoal(t *testing.T) {
	store, err := projectstate.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = resolveDuelState(store, filepath.Join(store.Dir, "duel-state.json"), "", false, false)
	if err == nil {
		t.Fatal("missing goal accepted by state creator")
	}
}

func TestDuelHelpExplainsRecurringLifecycle(t *testing.T) {
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = oldStderr })

	if err := runDuel([]string{"--help"}); err != nil {
		t.Fatalf("help failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		"Recurring duel:",
		"--run-for is the total lifetime",
		"--poll-every is how often RIGHT wakes up",
		"wait            no useful LEFT work yet",
		"plan/research",
		"rurushu duel --new",
		"rurushu duel --resume",
		"never shorten",
		"not a daemon or system service",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("help missing %q:\n%s", want, got)
		}
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
	if !reused || !reflect.DeepEqual(got, want) {
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
	wantTime := time.Date(2026, 10, 27, 15, 42, 0, 0, time.FixedZone("TST", 8*60*60))
	want := duelState{Goal: "goal", LeftID: "l", RightID: "r", Rounds: 3, RoundTimes: []time.Time{wantTime}}
	if err := saveDuelState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadDuelState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RoundTimes) != 1 || !got.RoundTimes[0].Equal(wantTime) {
		t.Fatalf("round timestamp did not persist: got=%v want=%v", got.RoundTimes, wantTime)
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
	started := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	want := duelState{
		Goal:            "continue improving",
		LeftID:          "left-session",
		RightID:         "right-session",
		Mode:            "recurring",
		PollEvery:       "30s",
		StartedAt:       started,
		Deadline:        started.Add(10 * time.Minute),
		NextPollAt:      started.Add(90 * time.Second),
		LastAction:      "wait",
		LastObservation: "executor attempts=0 Evidence: arm window expired",
		Rounds:          7,
		Status:          "interrupted",
		Summary:         "round seven uncovered an edge case",
	}
	if err := saveDuelState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadDuelState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resume state mismatch: got %+v want %+v", got, want)
	}
}

func TestResolveDuelStateResumeDoesNotResetRecurringDeadline(t *testing.T) {
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
	started := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	want := duelState{
		Goal:            "recurring resume",
		LeftID:          left.ID,
		RightID:         right.ID,
		Mode:            "recurring",
		PollEvery:       "30s",
		StartedAt:       started,
		Deadline:        started.Add(10 * time.Minute),
		NextPollAt:      started.Add(2 * time.Minute),
		LastAction:      "wait",
		LastObservation: "observer still running Evidence: no READY_TO_SEND",
		Rounds:          3,
		Status:          "interrupted",
		Summary:         "saved evidence",
	}
	path := filepath.Join(store.Dir, "duel-state.json")
	if err := saveDuelState(path, want); err != nil {
		t.Fatal(err)
	}
	got, reused, err := resolveDuelState(store, path, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reused || !reflect.DeepEqual(got, want) {
		t.Fatalf("resume changed recurring schedule: got=%+v reused=%v want=%+v", got, reused, want)
	}
}

func TestUpdateRecurringScheduleOnResume(t *testing.T) {
	now := time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC)
	original := duelState{Mode: "recurring", PollEvery: "30s", Deadline: now.Add(time.Hour), NextPollAt: now.Add(5 * time.Minute)}
	state := original
	if err := updateRecurringSchedule(&state, now, 0, false, 0, false); err != nil || !reflect.DeepEqual(state, original) {
		t.Fatalf("ordinary resume changed schedule: %+v %v", state, err)
	}
	if err := updateRecurringSchedule(&state, now, 610*time.Minute, true, 30*time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if !state.Deadline.Equal(now.Add(610*time.Minute)) || state.PollEvery != "30m0s" || !state.NextPollAt.Equal(now) {
		t.Fatalf("unexpected renewed schedule: %+v", state)
	}
	if err := updateRecurringSchedule(&state, now, time.Minute, true, 0, false); err != nil || !state.Deadline.Equal(now.Add(610*time.Minute)) {
		t.Fatalf("resume shortened the deadline: %+v %v", state, err)
	}
	if err := updateRecurringSchedule(&state, now, 0, true, 0, false); err == nil {
		t.Fatal("explicit zero lifetime accepted")
	}
}

func TestRecurringInitialSummaryIncludesPersistedObservation(t *testing.T) {
	state := duelState{
		Mode:            "recurring",
		Summary:         "short summary",
		LastAction:      "wait",
		LastObservation: "executor attempts=0 Evidence: arm window expired",
	}
	got := recurringInitialSummary(state)
	if !strings.Contains(got, "action=wait") || !strings.Contains(got, "attempts=0") || !strings.Contains(got, "arm window expired") {
		t.Fatalf("resume summary lost recurring observation: %q", got)
	}
}

func TestApplyDuelTurnStateRecurringCountsRightTickAndPersistsObservation(t *testing.T) {
	state := duelState{Mode: "recurring", Rounds: 4}
	shown := applyDuelTurnState(&state, "right", duel.Turn{
		Action:   "wait",
		Summary:  "observer has no READY_TO_SEND",
		Evidence: "preflight still running",
	})
	if shown != 5 || state.Rounds != 5 || state.LastAction != "wait" {
		t.Fatalf("state=%+v shown=%d", state, shown)
	}
	if !strings.Contains(state.LastObservation, "no READY_TO_SEND") || !strings.Contains(state.LastObservation, "preflight still running") {
		t.Fatalf("observation=%q", state.LastObservation)
	}
	shown = applyDuelTurnState(&state, "left", duel.Turn{Action: "continue", Summary: "executor started"})
	if shown != 5 || state.Rounds != 5 || state.Summary != "executor started" {
		t.Fatalf("LEFT should not double-count recurring tick: state=%+v shown=%d", state, shown)
	}
}

func TestEnsureRoundTimePreservesExistingTimestampAcrossResume(t *testing.T) {
	first := time.Date(2026, 10, 27, 15, 42, 0, 0, time.FixedZone("TST", 8*60*60))
	later := first.Add(2 * time.Hour)
	state := duelState{}
	if got := ensureRoundTime(&state, 1, first); !got.Equal(first) {
		t.Fatalf("first timestamp=%v want=%v", got, first)
	}
	if got := ensureRoundTime(&state, 1, later); !got.Equal(first) {
		t.Fatalf("timestamp drifted on resume: got=%v want=%v", got, first)
	}
}

func TestLoadLegacyDuelStateWithoutRoundTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duel-state.json")
	legacy := `{"goal":"legacy","left_session":"left","right_session":"right","rounds":4,"status":"interrupted","summary":"saved"}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := loadDuelState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rounds != 4 || len(got.RoundTimes) != 0 {
		t.Fatalf("legacy state changed unexpectedly: %+v", got)
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
			if tt.err != nil && !errors.Is(tt.err, context.Canceled) && state.LastError != tt.err.Error() {
				t.Fatalf("last error=%q want=%q", state.LastError, tt.err.Error())
			}
			if (tt.err == nil || errors.Is(tt.err, context.Canceled)) && state.LastError != "" {
				t.Fatalf("unexpected last error=%q", state.LastError)
			}
		})
	}
}
