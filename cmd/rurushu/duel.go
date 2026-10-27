package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/bubbletea/v2"
	"github.com/masato25/rurushu-go/duel"
	"github.com/masato25/rurushu-go/projectstate"
	"github.com/masato25/rurushu-go/provider"
)

type duelState struct {
	Goal            string      `json:"goal"`
	LeftID          string      `json:"left_session"`
	RightID         string      `json:"right_session"`
	Mode            string      `json:"mode,omitempty"`
	PollEvery       string      `json:"poll_every,omitempty"`
	StartedAt       time.Time   `json:"started_at,omitempty"`
	Deadline        time.Time   `json:"deadline,omitempty"`
	NextPollAt      time.Time   `json:"next_poll_at,omitempty"`
	LastAction      string      `json:"last_action,omitempty"`
	LastObservation string      `json:"last_observation,omitempty"`
	Rounds          int         `json:"rounds"`
	RoundTimes      []time.Time `json:"round_times,omitempty"`
	Status          string      `json:"status"`
	Summary         string      `json:"summary"`
}

type duelLockMeta struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	CWD       string    `json:"cwd"`
	Token     string    `json:"token"`
}

type duelLock struct {
	dir   string
	token string
}

func newDuelLockToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", buf[:]), nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EPERM)
}

func acquireDuelLock(stateDir, cwd string) (*duelLock, error) {
	dir := filepath.Join(stateDir, "duel-active.lock")
	for attempt := 0; attempt < 2; attempt++ {
		if err := os.Mkdir(dir, 0o700); err == nil {
			token, err := newDuelLockToken()
			if err != nil {
				_ = os.Remove(dir)
				return nil, err
			}
			meta := duelLockMeta{PID: os.Getpid(), StartedAt: time.Now(), CWD: cwd, Token: token}
			data, err := json.MarshalIndent(meta, "", "  ")
			if err != nil {
				_ = os.Remove(dir)
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, "owner.json"), append(data, '\n'), 0o600); err != nil {
				_ = os.RemoveAll(dir)
				return nil, fmt.Errorf("write duel lock: %w", err)
			}
			return &duelLock{dir: dir, token: token}, nil
		} else if !os.IsExist(err) {
			return nil, fmt.Errorf("acquire duel lock: %w", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "owner.json"))
		if err != nil {
			return nil, fmt.Errorf("duel is already active for this workspace (lock %s is initializing or unreadable)", dir)
		}
		var meta duelLockMeta
		if err := json.Unmarshal(data, &meta); err != nil || meta.PID <= 0 || strings.TrimSpace(meta.Token) == "" {
			return nil, fmt.Errorf("duel is already active for this workspace (invalid lock metadata at %s)", dir)
		}
		if processAlive(meta.PID) {
			return nil, fmt.Errorf("duel is already active for this workspace (pid %d, started %s, cwd %s)", meta.PID, meta.StartedAt.Format(time.RFC3339), meta.CWD)
		}
		staleDir := dir + ".stale-" + strconv.Itoa(meta.PID)
		_ = os.RemoveAll(staleDir)
		if err := os.Rename(dir, staleDir); err != nil {
			continue
		}
		_ = os.RemoveAll(staleDir)
	}
	return nil, errors.New("unable to acquire duel lock after reclaiming stale owner")
}

func (l *duelLock) Release() error {
	if l == nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(l.dir, "owner.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var meta duelLockMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return err
	}
	if meta.Token != l.token {
		return errors.New("duel lock ownership changed; refusing to release another instance's lock")
	}
	return os.RemoveAll(l.dir)
}

func applyDuelResult(state *duelState, result duel.Result, runErr error) {
	if strings.TrimSpace(result.Summary) != "" {
		state.Summary = result.Summary
	}
	if runErr == nil {
		state.Status = result.Status
		return
	}
	if errors.Is(runErr, context.Canceled) {
		state.Status = "interrupted"
		return
	}
	state.Status = "error"
}

func loadDuelState(path string) (duelState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return duelState{}, fmt.Errorf("resume duel: %w", err)
	}
	var state duelState
	if err = json.Unmarshal(data, &state); err != nil {
		return duelState{}, fmt.Errorf("decode duel state: %w", err)
	}
	if state.LeftID == "" || state.RightID == "" || state.Goal == "" || state.LeftID == state.RightID {
		return duelState{}, errors.New("invalid saved duel state")
	}
	return state, nil
}

func saveDuelState(path string, state duelState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".duel-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func ensureRoundTime(state *duelState, round int, now time.Time) time.Time {
	if round <= 0 {
		return time.Time{}
	}
	for len(state.RoundTimes) < round {
		state.RoundTimes = append(state.RoundTimes, time.Time{})
	}
	if state.RoundTimes[round-1].IsZero() {
		state.RoundTimes[round-1] = now
	}
	return state.RoundTimes[round-1]
}

func recurringInitialSummary(state duelState) string {
	if state.Mode == "recurring" && strings.TrimSpace(state.LastObservation) != "" {
		return fmt.Sprintf("Last recurring action=%s. Observation: %s", state.LastAction, state.LastObservation)
	}
	return state.Summary
}

func applyDuelTurnState(state *duelState, role string, turn duel.Turn) int {
	shownRound := state.Rounds + 1
	if state.Mode == "recurring" && role == "right" {
		state.Rounds++
		state.LastAction = turn.Action
		state.LastObservation = strings.TrimSpace(turn.Summary + " Evidence: " + turn.Evidence)
		state.Summary = turn.Summary
		return state.Rounds
	}
	if state.Mode != "recurring" && role == "left" {
		state.Rounds++
		state.Summary = turn.Summary
		return state.Rounds
	}
	if state.Mode == "recurring" && role == "left" {
		state.Summary = turn.Summary
		return state.Rounds
	}
	return shownRound
}

func resolveDuelState(store *projectstate.Store, path, requestedGoal string, resume, newDuel bool) (duelState, bool, error) {
	if resume && newDuel {
		return duelState{}, false, errors.New("--resume and --new cannot be combined")
	}
	requestedGoal = strings.TrimSpace(requestedGoal)
	loadExisting := func() (duelState, error) {
		state, err := loadDuelState(path)
		if err != nil {
			return duelState{}, err
		}
		if _, err = store.LoadSession(state.LeftID); err != nil {
			return duelState{}, err
		}
		if _, err = store.LoadSession(state.RightID); err != nil {
			return duelState{}, err
		}
		return state, nil
	}
	if resume {
		state, err := loadExisting()
		return state, true, err
	}
	if !newDuel {
		if _, err := os.Stat(path); err == nil {
			state, err := loadExisting()
			if err != nil {
				return duelState{}, false, err
			}
			if requestedGoal != "" && requestedGoal != state.Goal {
				return duelState{}, false, fmt.Errorf("an existing duel is already bound to goal %q; use --resume to continue it or --new to start a new duel", state.Goal)
			}
			return state, true, nil
		} else if !os.IsNotExist(err) {
			return duelState{}, false, err
		}
	}
	if requestedGoal == "" {
		return duelState{}, false, errors.New("duel requires --goal or positional goal when starting a new duel")
	}
	left, err := store.StartSession("chatgpt-web", "cos")
	if err != nil {
		return duelState{}, false, err
	}
	right, err := store.StartSession("chatgpt-web", "cos")
	if err != nil {
		return duelState{}, false, err
	}
	state := duelState{Goal: requestedGoal, LeftID: left.ID, RightID: right.ID, Status: "running"}
	if err = saveDuelState(path, state); err != nil {
		return duelState{}, false, err
	}
	return state, false, nil
}

func runDuel(args []string) error {
	fs := flag.NewFlagSet("rurushu duel", flag.ContinueOnError)
	cwdDefault, _ := os.Getwd()
	cwd := fs.String("cwd", cwdDefault, "project directory")
	goal := fs.String("goal", "", "task goal for both agents")
	resume := fs.Bool("resume", false, "resume existing left/right COS sessions")
	newDuel := fs.Bool("new", false, "start a new duel with a fresh LEFT/RIGHT session pair")
	runFor := fs.Duration("run-for", 0, "run a recurring duel for this duration (for example 10m)")
	pollEvery := fs.Duration("poll-every", 30*time.Second, "recurring duel interval (for example 30s)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	pollEverySet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "poll-every" {
			pollEverySet = true
		}
	})
	if *runFor < 0 || *pollEvery <= 0 {
		return errors.New("--run-for cannot be negative and --poll-every must be positive")
	}
	if *runFor == 0 && pollEverySet {
		return errors.New("--poll-every requires --run-for when creating a recurring duel")
	}
	abs, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}
	store, err := projectstate.Open(abs)
	if err != nil {
		return err
	}
	lock, err := acquireDuelLock(store.Dir, abs)
	if err != nil {
		return err
	}
	defer lock.Release()
	path := filepath.Join(store.Dir, "duel-state.json")
	requestedGoal := strings.TrimSpace(*goal)
	if requestedGoal == "" {
		requestedGoal = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	state, reused, err := resolveDuelState(store, path, requestedGoal, *resume, *newDuel)
	if err != nil {
		return err
	}
	now := time.Now()
	if reused && state.Mode == "recurring" {
		if *runFor > 0 {
			return errors.New("recurring duel already has a persisted deadline; resume without --run-for so the original deadline is preserved")
		}
		if pollEverySet {
			return errors.New("recurring duel already has a persisted poll interval; resume without --poll-every")
		}
	} else if *runFor > 0 {
		state.Mode = "recurring"
		state.PollEvery = pollEvery.String()
		state.StartedAt = now
		state.Deadline = now.Add(*runFor)
		state.NextPollAt = now
		if err = saveDuelState(path, state); err != nil {
			return err
		}
	}
	if reused {
		state.Status = "running"
		if err = saveDuelState(path, state); err != nil {
			return err
		}
	}
	if strings.TrimSpace(os.Getenv("COS_SESSION_ID")) != "" {
		return errors.New("unset COS_SESSION_ID for duel: each role requires its own remote session")
	}
	p := provider.NewCOSProvider(abs)
	// Bubble Tea owns the terminal; COS diagnostics must not write to stderr.
	p.Stderr = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stateMu sync.Mutex
	instructions := make(chan string, 128)
	updates := make(chan duelUpdate, 128)
	run := func() {
		stateMu.Lock()
		runGoal, runLeftID, runRightID, runSummary := state.Goal, state.LeftID, state.RightID, recurringInitialSummary(state)
		stateMu.Unlock()
		baseOptions := duel.Options{Goal: runGoal, LeftID: runLeftID, RightID: runRightID, InitialSummary: runSummary, Instructions: instructions, OnTurn: func(role string, round int, turn duel.Turn) error {
			stateMu.Lock()
			defer stateMu.Unlock()
			shownRound := applyDuelTurnState(&state, role, turn)
			roundTime := ensureRoundTime(&state, shownRound, time.Now())
			if err := saveDuelState(path, state); err != nil {
				return err
			}
			select {
			case updates <- duelUpdate{role: role, round: shownRound, roundTime: roundTime, turn: turn}:
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		}}
		var result duel.Result
		var err error
		stateMu.Lock()
		mode, pollEveryText, deadline, nextPollAt := state.Mode, state.PollEvery, state.Deadline, state.NextPollAt
		stateMu.Unlock()
		if mode == "recurring" {
			interval, parseErr := time.ParseDuration(pollEveryText)
			if parseErr != nil || interval <= 0 {
				if parseErr == nil {
					parseErr = errors.New("interval must be positive")
				}
				err = fmt.Errorf("invalid persisted recurring poll interval %q: %w", pollEveryText, parseErr)
			} else {
				result, err = duel.RunRecurring(ctx, p, duel.RecurringOptions{
					Options:    baseOptions,
					PollEvery:  interval,
					Deadline:   deadline,
					NextPollAt: nextPollAt,
					OnSchedule: func(next time.Time) error {
						stateMu.Lock()
						defer stateMu.Unlock()
						state.NextPollAt = next
						return saveDuelState(path, state)
					},
				})
			}
		} else {
			result, err = duel.Run(ctx, p, baseOptions)
		}
		stateMu.Lock()
		applyDuelResult(&state, result, err)
		if saveErr := saveDuelState(path, state); saveErr != nil && err == nil {
			err = saveErr
		}
		stateMu.Unlock()
		select {
		case updates <- duelUpdate{done: true, err: err, result: result}:
		case <-ctx.Done():
		}
	}
	cancelAndPersist := func() {
		cancel()
		stateMu.Lock()
		defer stateMu.Unlock()
		if state.Status == "running" {
			state.Status = "interrupted"
			_ = saveDuelState(path, state)
		}
	}
	ui := newDuelUI(instructions, updates, run, cancelAndPersist, state.LeftID, state.RightID)
	_, err = tea.NewProgram(ui).Run()
	return err
}
