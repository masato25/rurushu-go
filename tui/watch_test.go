package tui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/masato25/rurushu-go/projectstate"
)

func newWatchTestModel(t *testing.T) *Model {
	t.Helper()
	dir := t.TempDir()
	store, err := projectstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.StartSession("test-model", "openai-compatible")
	if err != nil {
		t.Fatal(err)
	}
	m := New("test-model", "openai-compatible")
	m.SetProjectSession(store, session)
	return m
}

func TestWatchSlashStartsAndStopsPersistentSchedule(t *testing.T) {
	m := newWatchTestModel(t)
	m.composer.SetValue("/watch 10m inspect monitor and repair safe issues")
	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if !m.watch.Active || m.watch.Interval != "10m0s" || m.watch.Prompt != "inspect monitor and repair safe issues" {
		t.Fatalf("watch not configured: %+v", m.watch)
	}
	if cmd == nil {
		t.Fatal("watch start did not arm timer")
	}
	if _, err := os.Stat(m.watchPath()); err != nil {
		t.Fatalf("watch state not persisted: %v", err)
	}

	m.composer.SetValue("/watch stop")
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if m.watch.Active {
		t.Fatal("watch remained active after stop")
	}
}

func TestWatchTickQueuesSameSessionInstructionWhenIdle(t *testing.T) {
	m := newWatchTestModel(t)
	m.watch = watchSchedule{Version: watchVersion, Active: true, Interval: time.Minute.String(), Prompt: "inspect now", NextRunAt: time.Now(), generation: 7}
	_, cmd := m.Update(watchTickMsg{generation: 7})
	if len(m.queuedInputs) != 1 || m.queuedInputs[0] != "inspect now" {
		t.Fatalf("watch queue=%#v", m.queuedInputs)
	}
	if cmd == nil {
		t.Fatal("watch tick did not return submit/timer command")
	}
	if m.watch.LastRunAt.IsZero() || !m.watch.NextRunAt.After(m.watch.LastRunAt) {
		t.Fatalf("watch timestamps not advanced: %+v", m.watch)
	}
}

func TestWatchTickSkipsWhileBusyOrUserHasDraft(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Model)
	}{
		{name: "streaming", setup: func(m *Model) { m.streaming = true }},
		{name: "draft", setup: func(m *Model) { m.composer.SetValue("do not overwrite me") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newWatchTestModel(t)
			m.watch = watchSchedule{Version: watchVersion, Active: true, Interval: time.Minute.String(), Prompt: "inspect now", NextRunAt: time.Now(), generation: 2}
			tc.setup(m)
			m.Update(watchTickMsg{generation: 2})
			if len(m.queuedInputs) != 0 {
				t.Fatalf("busy watch should skip, queue=%#v", m.queuedInputs)
			}
			if !m.watch.LastRunAt.IsZero() {
				t.Fatalf("skipped tick recorded as run: %s", m.watch.LastRunAt)
			}
			if tc.name == "draft" && !strings.Contains(m.composer.Value(), "do not overwrite") {
				t.Fatalf("draft changed: %q", m.composer.Value())
			}
		})
	}
}

func TestWatchScheduleRestoresForSession(t *testing.T) {
	m := newWatchTestModel(t)
	m.watch = watchSchedule{Version: watchVersion, Active: true, Interval: "5m0s", Prompt: "health check", NextRunAt: time.Now().Add(5 * time.Minute), generation: 3}
	if err := m.persistWatchSchedule(); err != nil {
		t.Fatal(err)
	}

	reloaded := New("test-model", "openai-compatible")
	session, err := m.sessionStore.LoadSession(m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.SetProjectSession(m.sessionStore, session)
	if !reloaded.watch.Active || reloaded.watch.Interval != "5m0s" || reloaded.watch.Prompt != "health check" {
		t.Fatalf("watch not restored: %+v", reloaded.watch)
	}
}

func TestWatchSyncLoadsExternalToolChanges(t *testing.T) {
	m := newWatchTestModel(t)
	m.watch = watchSchedule{Version: watchVersion, Active: true, Interval: "10m0s", Prompt: "scan", NextRunAt: time.Now().Add(10 * time.Minute)}
	if err := m.persistWatchSchedule(); err != nil {
		t.Fatal(err)
	}
	if cmd := m.syncWatchSchedule(); cmd != nil {
		t.Fatal("unchanged state should not rearm watch")
	}
	newSchedule := watchSchedule{Version: watchVersion, Active: true, Interval: "5m0s", Prompt: "new scan", NextRunAt: time.Now().Add(5 * time.Minute)}
	data, err := json.Marshal(newSchedule)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.watchPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if cmd := m.syncWatchSchedule(); cmd == nil || m.watch.Prompt != "new scan" {
		t.Fatalf("external change not synchronized: %+v", m.watch)
	}
	newSchedule.Active = false
	data, _ = json.Marshal(newSchedule)
	if err := os.WriteFile(m.watchPath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	if cmd := m.syncWatchSchedule(); cmd != nil || m.watch.Active {
		t.Fatalf("external stop not synchronized: %+v", m.watch)
	}
}

func TestWatchRunLifecyclePersistsCompletionAndFailure(t *testing.T) {
	m := newWatchTestModel(t)
	m.watch = watchSchedule{Version: watchVersion, Active: true, Interval: "5m0s", Prompt: "inspect", NextRunAt: time.Now(), generation: 3}
	m.Update(watchTickMsg{generation: 3})
	if m.watch.RunStatus != "queued" || !m.watchQueued {
		t.Fatalf("queued: %+v", m.watch)
	}
	m.Update(queuedSubmitMsg{})
	if m.watch.RunStatus != "running" || !m.watchRunPending {
		t.Fatalf("running: %+v", m.watch)
	}
	m.watchRunStatus("completed", "")
	if m.watch.RunStatus != "completed" || m.watch.CompletedAt.IsZero() {
		t.Fatalf("completed: %+v", m.watch)
	}
	var saved watchSchedule
	data, err := os.ReadFile(m.watchPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &saved); err != nil || saved.RunStatus != "completed" {
		t.Fatalf("persisted: %+v %v", saved, err)
	}
	m.watchRunPending = true
	m.watchRunStatus("failed", "timeout")
	if m.watch.RunStatus != "failed" || m.watch.RunError != "timeout" {
		t.Fatalf("failed: %+v", m.watch)
	}
}

func TestWatchBusyTickRecordsSkippedWithoutOverwritingRunning(t *testing.T) {
	m := newWatchTestModel(t)
	m.watch = watchSchedule{Version: watchVersion, Active: true, Interval: "5m0s", Prompt: "inspect", NextRunAt: time.Now(), generation: 2, RunStatus: "running"}
	m.streaming = true
	m.watchRunPending = true
	m.Update(watchTickMsg{generation: 2})
	if m.watch.SkippedCount != 1 || m.watch.RunStatus != "running" {
		t.Fatalf("wrong running status: %+v", m.watch)
	}
}

func TestWatchRestoreMarksIncompleteRunInterrupted(t *testing.T) {
	m := newWatchTestModel(t)
	m.watch = watchSchedule{Version: watchVersion, Active: true, Interval: "5m0s", Prompt: "inspect", NextRunAt: time.Now().Add(time.Minute), RunStatus: "running"}
	if err := m.persistWatchSchedule(); err != nil {
		t.Fatal(err)
	}
	session, err := m.sessionStore.LoadSession(m.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := New("test-model", "openai-compatible")
	reloaded.SetProjectSession(m.sessionStore, session)
	if reloaded.watch.RunStatus != "interrupted" || reloaded.watch.CompletedAt.IsZero() {
		t.Fatalf("incomplete run not marked interrupted: %+v", reloaded.watch)
	}
}
