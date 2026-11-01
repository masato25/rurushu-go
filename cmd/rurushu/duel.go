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
	tuiPkg "github.com/masato25/rurushu-go/tui"
)

type duelState struct {
	Goal            string      `json:"goal"`
	LeftID          string      `json:"left_session"`
	RightID         string      `json:"right_session"`
	LeftProvider    string      `json:"left_provider,omitempty"`
	LeftModel       string      `json:"left_model,omitempty"`
	RightProvider   string      `json:"right_provider,omitempty"`
	RightModel      string      `json:"right_model,omitempty"`
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
	LastError       string      `json:"last_error,omitempty"`
}

type duelRoleConfig struct {
	Provider string
	Model    string
}

func normalizeDuelRoleConfig(cfg duelRoleConfig) duelRoleConfig {
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.Provider == "" {
		cfg.Provider = "cos"
	}
	if cfg.Provider == "chat-on-steroids" {
		cfg.Provider = "cos"
	}
	if cfg.Provider == "openai" {
		cfg.Provider = "openai-compatible"
	}
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.Model == "" && cfg.Provider == "cos" {
		cfg.Model = "chatgpt-web"
	}
	return cfg
}

func duelRoleConfigs(configs []duelRoleConfig) (duelRoleConfig, duelRoleConfig) {
	left := duelRoleConfig{Provider: "cos", Model: "chatgpt-web"}
	right := left
	if len(configs) > 0 {
		left = normalizeDuelRoleConfig(configs[0])
	}
	if len(configs) > 1 {
		right = normalizeDuelRoleConfig(configs[1])
	}
	return left, right
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
		state.LastError = ""
		return
	}
	if errors.Is(runErr, context.Canceled) {
		state.Status = "interrupted"
		state.LastError = ""
		return
	}
	state.Status = "error"
	state.LastError = runErr.Error()
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
	if strings.TrimSpace(state.LastObservation) != "" {
		return state.LastObservation
	}
	return state.Summary
}

func applyDuelTurnState(state *duelState, role string, turn duel.Turn) int {
	shownRound := state.Rounds + 1
	if role == "left" {
		state.Rounds++
		state.Summary = turn.Summary
		return state.Rounds
	}
	if role == "right" {
		state.LastAction = turn.Action
		state.LastObservation = strings.TrimSpace(turn.Summary)
		return state.Rounds
	}
	return shownRound
}

func resolveDuelState(store *projectstate.Store, path, requestedGoal string, resume, newDuel bool, configs ...duelRoleConfig) (duelState, bool, error) {
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
	leftCfg, rightCfg := duelRoleConfigs(configs)
	if leftCfg.Model == "" || rightCfg.Model == "" {
		return duelState{}, false, errors.New("left and right model are required")
	}
	left, err := store.StartSession(leftCfg.Model, leftCfg.Provider)
	if err != nil {
		return duelState{}, false, err
	}
	right, err := store.StartSession(rightCfg.Model, rightCfg.Provider)
	if err != nil {
		return duelState{}, false, err
	}
	state := duelState{Goal: requestedGoal, LeftID: left.ID, RightID: right.ID, LeftProvider: leftCfg.Provider, LeftModel: leftCfg.Model, RightProvider: rightCfg.Provider, RightModel: rightCfg.Model, Status: "running"}
	if err = saveDuelState(path, state); err != nil {
		return duelState{}, false, err
	}
	return state, false, nil
}

func createDuelState(store *projectstate.Store, path, goal string, configs ...duelRoleConfig) (duelState, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return duelState{}, errors.New("duel goal is required")
	}
	leftCfg, rightCfg := duelRoleConfigs(configs)
	if leftCfg.Model == "" || rightCfg.Model == "" {
		return duelState{}, errors.New("left and right model are required")
	}
	left, err := store.StartSession(leftCfg.Model, leftCfg.Provider)
	if err != nil {
		return duelState{}, err
	}
	right, err := store.StartSession(rightCfg.Model, rightCfg.Provider)
	if err != nil {
		return duelState{}, err
	}
	state := duelState{Goal: goal, LeftID: left.ID, RightID: right.ID, LeftProvider: leftCfg.Provider, LeftModel: leftCfg.Model, RightProvider: rightCfg.Provider, RightModel: rightCfg.Model, Status: "running"}
	if err := saveDuelState(path, state); err != nil {
		return duelState{}, err
	}
	return state, nil
}

func newDuelProvider(cwd, name, baseURL, apiKey string) (provider.Provider, error) {
	switch normalizeDuelRoleConfig(duelRoleConfig{Provider: name}).Provider {
	case "cos":
		p := provider.NewCOSProvider(cwd)
		p.Stderr = nil
		return p, nil
	case "openai-compatible":
		return provider.NewOpenAICompatibleProvider(strings.TrimSpace(baseURL), strings.TrimSpace(apiKey)), nil
	default:
		return nil, fmt.Errorf("unsupported duel provider %q", name)
	}
}

func runDuel(args []string) error {
	fs := flag.NewFlagSet("rurushu duel", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwdDefault, _ := os.Getwd()
	cwd := fs.String("cwd", cwdDefault, "project directory")
	goal := fs.String("goal", "", "task goal for both agents")
	leftProviderName := fs.String("left-provider", firstNonEmpty(os.Getenv("RURUSHU_LEFT_PROVIDER"), os.Getenv("RURUSHU_PROVIDER"), "cos"), "LEFT provider: cos or openai-compatible")
	leftModelName := fs.String("left-model", firstNonEmpty(os.Getenv("RURUSHU_LEFT_MODEL"), os.Getenv("RURUSHU_MODEL")), "LEFT model (COS defaults to chatgpt-web)")
	rightProviderName := fs.String("right-provider", firstNonEmpty(os.Getenv("RURUSHU_RIGHT_PROVIDER"), os.Getenv("RURUSHU_PROVIDER"), "cos"), "RIGHT provider: cos or openai-compatible")
	rightModelName := fs.String("right-model", firstNonEmpty(os.Getenv("RURUSHU_RIGHT_MODEL"), os.Getenv("RURUSHU_MODEL")), "RIGHT model (COS defaults to chatgpt-web)")
	baseURL := fs.String("base-url", firstNonEmpty(os.Getenv("RURUSHU_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), "https://api.openai.com/v1"), "OpenAI-compatible API base URL used by duel roles")
	apiKey := fs.String("api-key", firstNonEmpty(os.Getenv("RURUSHU_API_KEY"), os.Getenv("OPENAI_API_KEY")), "OpenAI-compatible API key used by duel roles")
	resume := fs.Bool("resume", false, "resume existing LEFT/RIGHT sessions")
	newDuel := fs.Bool("new", false, "start a new duel with a fresh LEFT/RIGHT session pair")
	runFor := fs.Duration("run-for", 0, "run a recurring duel for this duration (for example 10m)")
	pollEvery := fs.Duration("poll-every", 30*time.Second, "recurring duel interval (for example 30s)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `Usage:
  rurushu duel --new --goal "<goal>" [--run-for DURATION --poll-every DURATION]
  rurushu duel --resume

Recurring duel:
  --run-for is the total lifetime of the recurring run.
  --poll-every is how often the next LEFT work round starts.

Conversation model:
  LEFT is the primary worker and runs first, like a normal single chat.
  RIGHT receives LEFT's final response and replies with at most 100 characters.
  That short RIGHT reply is fed back to LEFT for the next work round.
  LEFT and RIGHT may use different providers and models.
  RIGHT can prefix a reply with DONE: when the task is ready for owner review.

Example:
  rurushu duel --new \
    --goal "持續觀測 preflight 與 executor；沒有 READY_TO_SEND 時等待，有 net-positive READY 時啟動 executor，直到確認鏈上獲利或需要主人介入" \
    --run-for 30m \
    --poll-every 1m

Resume:
  Press Ctrl+C to interrupt safely. From the same workspace, run:
    rurushu duel --resume

  Resume without schedule flags keeps the original deadline and next poll.
  With --run-for, extend the deadline to at least now + duration (never shorten).
  With --poll-every, replace the interval and start the next poll now.

Process lifetime:
  This MVP is not a daemon or system service. The rurushu process must remain
  alive while polling; closing the terminal/process stops execution until you
  explicitly run --resume.

Options:`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	pollEverySet, runForSet := false, false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "poll-every" {
			pollEverySet = true
		}
		if f.Name == "run-for" {
			runForSet = true
		}
	})
	if *runFor < 0 || *pollEvery <= 0 {
		return errors.New("--run-for cannot be negative and --poll-every must be positive")
	}
	if *runFor == 0 && pollEverySet && !*resume {
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
	leftCfg := normalizeDuelRoleConfig(duelRoleConfig{Provider: *leftProviderName, Model: *leftModelName})
	rightCfg := normalizeDuelRoleConfig(duelRoleConfig{Provider: *rightProviderName, Model: *rightModelName})
	if leftCfg.Model == "" || rightCfg.Model == "" {
		return errors.New("--left-model and --right-model are required for non-COS providers")
	}
	if requestedGoal == "" {
		requestedGoal = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	waitingForGoal := false
	var state duelState
	var reused bool
	if requestedGoal == "" && !*resume {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			waitingForGoal = true
		} else if statErr != nil {
			return statErr
		}
	}
	if !waitingForGoal {
		state, reused, err = resolveDuelState(store, path, requestedGoal, *resume, *newDuel, leftCfg, rightCfg)
		if err != nil {
			return err
		}
	}
	now := time.Now()
	if !waitingForGoal && reused && state.Mode == "recurring" {
		if err := updateRecurringSchedule(&state, now, *runFor, runForSet, *pollEvery, pollEverySet); err != nil {
			return err
		}
		if runForSet || pollEverySet {
			if err = saveDuelState(path, state); err != nil {
				return err
			}
		}
	} else if !waitingForGoal && *runFor > 0 {
		state.Mode = "recurring"
		state.PollEvery = pollEvery.String()
		state.StartedAt = now
		state.Deadline = now.Add(*runFor)
		state.NextPollAt = now
		if err = saveDuelState(path, state); err != nil {
			return err
		}
	}
	if !waitingForGoal && reused {
		state.Status = "running"
		state.LastError = ""
		if err = saveDuelState(path, state); err != nil {
			return err
		}
	}
	if (state.LeftProvider == "cos" || state.RightProvider == "cos" || waitingForGoal) && strings.TrimSpace(os.Getenv("COS_SESSION_ID")) != "" {
		return errors.New("unset COS_SESSION_ID for duel: each role requires its own remote session")
	}
	activeLeftCfg, activeRightCfg := leftCfg, rightCfg
	if !waitingForGoal {
		activeLeftCfg = normalizeDuelRoleConfig(duelRoleConfig{Provider: state.LeftProvider, Model: state.LeftModel})
		activeRightCfg = normalizeDuelRoleConfig(duelRoleConfig{Provider: state.RightProvider, Model: state.RightModel})
	}
	leftProvider, err := newDuelProvider(abs, activeLeftCfg.Provider, *baseURL, *apiKey)
	if err != nil {
		return err
	}
	rightProvider, err := newDuelProvider(abs, activeRightCfg.Provider, *baseURL, *apiKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	terminalService := tuiPkg.NewTerminalService(abs)
	defer terminalService.Close()
	var stateMu sync.Mutex
	instructions := make(chan string, 128)
	updates := make(chan duelUpdate, 128)
	run := func() {
		stateMu.Lock()
		runGoal, runLeftID, runRightID, runSummary := state.Goal, state.LeftID, state.RightID, recurringInitialSummary(state)
		stateMu.Unlock()
		runLeftModel, runRightModel := activeLeftCfg.Model, activeRightCfg.Model
		baseOptions := duel.Options{Goal: runGoal, LeftID: runLeftID, RightID: runRightID, LeftModel: runLeftModel, RightModel: runRightModel, LeftStreamer: leftProvider, RightStreamer: rightProvider, InitialSummary: runSummary, Instructions: instructions, RetryDelay: time.Minute, GoalFunc: func() string {
			stateMu.Lock()
			defer stateMu.Unlock()
			return state.Goal
		}, OnRetry: func(role string, round int, delay time.Duration, retryErr error) error {
			select {
			case updates <- duelUpdate{role: role, round: round, err: retryErr, retry: true, retryAfter: delay}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, OnTurn: func(role string, round int, turn duel.Turn) error {
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
				result, err = duel.RunRecurring(ctx, nil, duel.RecurringOptions{
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
			result, err = duel.Run(ctx, nil, baseOptions)
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
	newRoleSession := func(role string) (string, error) {
		stateMu.Lock()
		defer stateMu.Unlock()
		cfg := duelRoleConfig{Provider: state.LeftProvider, Model: state.LeftModel}
		if role == "right" {
			cfg = duelRoleConfig{Provider: state.RightProvider, Model: state.RightModel}
		}
		cfg = normalizeDuelRoleConfig(cfg)
		session, createErr := store.StartSession(cfg.Model, cfg.Provider)
		if createErr != nil {
			return "", createErr
		}
		switch role {
		case "left":
			state.LeftID = session.ID
		case "right":
			state.RightID = session.ID
		default:
			return "", fmt.Errorf("unknown duel role %q", role)
		}
		state.Status = "running"
		state.LastError = ""
		if saveErr := saveDuelState(path, state); saveErr != nil {
			return "", saveErr
		}
		return session.ID, nil
	}
	ui := newDuelUI(instructions, updates, run, cancelAndPersist, state.LeftID, state.RightID)
	aux := tuiPkg.NewAuxiliaryWorkspace(abs, terminalService)
	updateGoal := func(goal string) error {
		goal = strings.TrimSpace(goal)
		if goal == "" {
			return errors.New("duel goal is required")
		}
		stateMu.Lock()
		defer stateMu.Unlock()
		state.Goal = goal
		return saveDuelState(path, state)
	}
	if !waitingForGoal {
		aux.SetGoalEditor(state.Goal, updateGoal)
	}
	ui.setAuxiliaryWorkspace(aux)
	ui.setNewSessionHandler(newRoleSession)
	if waitingForGoal {
		ui.setInitialGoalHandler(func(goal string) error {
			created, createErr := createDuelState(store, path, goal, leftCfg, rightCfg)
			if createErr != nil {
				return createErr
			}
			if *runFor > 0 {
				created.Mode = "recurring"
				created.PollEvery = pollEvery.String()
				created.StartedAt = time.Now()
				created.Deadline = created.StartedAt.Add(*runFor)
				created.NextPollAt = created.StartedAt
				if saveErr := saveDuelState(path, created); saveErr != nil {
					return saveErr
				}
			}
			stateMu.Lock()
			state = created
			stateMu.Unlock()
			ui.leftID, ui.rightID = created.LeftID, created.RightID
			aux.SetGoalEditor(created.Goal, updateGoal)
			return nil
		})
	}
	_, err = tea.NewProgram(ui).Run()
	return err
}

func updateRecurringSchedule(state *duelState, now time.Time, runFor time.Duration, runForSet bool, pollEvery time.Duration, pollEverySet bool) error {
	if runForSet {
		if runFor <= 0 {
			return errors.New("--run-for must be positive when overriding a recurring duel deadline")
		}
		if deadline := now.Add(runFor); deadline.After(state.Deadline) {
			state.Deadline = deadline
		}
	}
	if pollEverySet {
		if pollEvery <= 0 {
			return errors.New("--poll-every must be positive")
		}
		state.PollEvery = pollEvery.String()
		state.NextPollAt = now
	}
	return nil
}
