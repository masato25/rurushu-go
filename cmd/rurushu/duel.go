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
	rurushuconfig "github.com/masato25/rurushu-go/config"
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
	PendingOwner    []string    `json:"pending_owner,omitempty"`
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

func loadDuelStateLoose(path string) (duelState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return duelState{}, err
	}
	var state duelState
	if err := json.Unmarshal(data, &state); err != nil {
		return duelState{}, fmt.Errorf("decode duel state: %w", err)
	}
	return state, nil
}

func reconcileDuelStateForLeft(store *projectstate.Store, path, requestedGoal, leftSessionID string, leftCfg, rightCfg duelRoleConfig) (duelState, bool, error) {
	leftSessionID = strings.TrimSpace(leftSessionID)
	if leftSessionID == "" {
		return duelState{}, false, errors.New("LEFT session is required")
	}
	left, err := store.LoadSession(leftSessionID)
	if err != nil {
		return duelState{}, false, err
	}
	leftCfg = normalizeDuelRoleConfig(leftCfg)
	rightCfg = normalizeDuelRoleConfig(rightCfg)
	_ = left

	state := duelState{}
	reused := false
	if _, statErr := os.Stat(path); statErr == nil {
		state, err = loadDuelStateLoose(path)
		if err != nil {
			return duelState{}, false, err
		}
		reused = true
	} else if !os.IsNotExist(statErr) {
		return duelState{}, false, statErr
	}

	goal := strings.TrimSpace(requestedGoal)
	if goal == "" {
		goal = strings.TrimSpace(state.Goal)
	}
	if goal == "" {
		return duelState{}, reused, errors.New("duel goal is required")
	}

	state.Goal = goal
	state.LeftID = leftSessionID
	state.LeftProvider = leftCfg.Provider
	state.LeftModel = leftCfg.Model

	rightValid := strings.TrimSpace(state.RightID) != "" && state.RightID != leftSessionID
	if rightValid {
		right, loadErr := store.LoadSession(state.RightID)
		if loadErr != nil {
			rightValid = false
		} else {
			rightSessionCfg := normalizeDuelRoleConfig(duelRoleConfig{Provider: right.Provider, Model: right.Model})
			if rightSessionCfg.Provider != rightCfg.Provider || rightSessionCfg.Model != rightCfg.Model {
				rightValid = false
			}
		}
	}
	if !rightValid {
		right, createErr := store.StartSession(rightCfg.Model, rightCfg.Provider)
		if createErr != nil {
			return duelState{}, reused, createErr
		}
		state.RightID = right.ID
		state.RightProvider = rightCfg.Provider
		state.RightModel = rightCfg.Model
	} else {
		state.RightProvider = rightCfg.Provider
		state.RightModel = rightCfg.Model
	}
	state.Status = "running"
	state.LastError = ""
	if err := saveDuelState(path, state); err != nil {
		return duelState{}, reused, err
	}
	return state, reused, nil
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
	return createDuelStateWithLeft(store, path, goal, "", configs...)
}

func createDuelStateWithLeft(store *projectstate.Store, path, goal, leftSessionID string, configs ...duelRoleConfig) (duelState, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return duelState{}, errors.New("duel goal is required")
	}
	leftCfg, rightCfg := duelRoleConfigs(configs)
	if leftCfg.Model == "" || rightCfg.Model == "" {
		return duelState{}, errors.New("left and right model are required")
	}
	var left projectstate.Session
	var err error
	if strings.TrimSpace(leftSessionID) != "" {
		left, err = store.LoadSession(strings.TrimSpace(leftSessionID))
	} else {
		left, err = store.StartSession(leftCfg.Model, leftCfg.Provider)
	}
	if err != nil {
		return duelState{}, err
	}
	leftCfg = normalizeDuelRoleConfig(leftCfg)
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

func newDuelProvider(cwd, name, baseURL, apiKey, ollamaURL string) (provider.Provider, error) {
	switch normalizeDuelRoleConfig(duelRoleConfig{Provider: name}).Provider {
	case "cos":
		p := provider.NewCOSProvider(cwd)
		p.Stderr = nil
		return p, nil
	case "openai-compatible":
		return provider.NewOpenAICompatibleProvider(strings.TrimSpace(baseURL), strings.TrimSpace(apiKey)), nil
	case "ollama":
		return provider.NewOllamaProvider(ollamaURL), nil
	default:
		return nil, fmt.Errorf("unsupported duel provider %q", name)
	}
}

func runDuel(args []string) error {
	fs := flag.NewFlagSet("rurushu duel", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwdDefault, _ := os.Getwd()
	saved, err := rurushuconfig.LoadForDir(cwdDefault)
	if err != nil {
		return err
	}
	cwd := fs.String("cwd", cwdDefault, "project directory")
	goal := fs.String("goal", "", "task goal for both agents")
	leftProviderName := fs.String("left-provider", firstNonEmpty(os.Getenv("RURUSHU_LEFT_PROVIDER"), os.Getenv("RURUSHU_PROVIDER"), saved.Duel.Left.Provider, "cos"), "LEFT provider: cos, openai-compatible, or ollama")
	leftModelName := fs.String("left-model", firstNonEmpty(os.Getenv("RURUSHU_LEFT_MODEL"), os.Getenv("RURUSHU_MODEL"), saved.Duel.Left.Model), "LEFT model (COS defaults to chatgpt-web)")
	leftSessionID := fs.String("left-session", "", "reuse an existing project session as duel LEFT")
	rightProviderName := fs.String("right-provider", firstNonEmpty(os.Getenv("RURUSHU_RIGHT_PROVIDER"), os.Getenv("RURUSHU_PROVIDER"), saved.Duel.Right.Provider, "cos"), "RIGHT provider: cos, openai-compatible, or ollama")
	rightModelName := fs.String("right-model", firstNonEmpty(os.Getenv("RURUSHU_RIGHT_MODEL"), os.Getenv("RURUSHU_MODEL"), saved.Duel.Right.Model), "RIGHT model (COS defaults to chatgpt-web)")
	baseURL := fs.String("base-url", firstNonEmpty(os.Getenv("RURUSHU_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), saved.BaseURL, "https://api.openai.com/v1"), "OpenAI-compatible API base URL used by duel roles")
	apiKey := fs.String("api-key", firstNonEmpty(os.Getenv("RURUSHU_API_KEY"), os.Getenv("OPENAI_API_KEY")), "OpenAI-compatible API key used by duel roles")
	ollamaURL := fs.String("ollama-url", firstNonEmpty(os.Getenv("RURUSHU_OLLAMA_URL"), saved.Duel.OllamaURL, "http://127.0.0.1:11434"), "Ollama server URL (without /v1 is also accepted)")
	resume := fs.Bool("resume", false, "resume existing LEFT/RIGHT sessions")
	continueDuel := fs.Bool("continue", false, "continue the current project's existing duel (alias for --resume)")
	newDuel := fs.Bool("new", false, "start a new duel with a fresh LEFT/RIGHT session pair")
	runFor := fs.Duration("run-for", 0, "run a recurring duel for this duration (for example 10m)")
	pollEvery := fs.Duration("poll-every", 30*time.Second, "recurring duel interval (for example 30s)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), `Usage:
  rurushu duel --new --goal "<goal>" [--run-for DURATION --poll-every DURATION]
  rurushu duel --continue
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

Ollama RIGHT example:
  rurushu duel --new --goal "<goal>" \
    --right-provider ollama --right-model qwen3:8b \
    --ollama-url http://192.168.2.109:11434

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
	if *continueDuel {
		if *newDuel {
			return errors.New("--continue cannot be combined with --new")
		}
		*resume = true
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
	preferredLeftID := strings.TrimSpace(*leftSessionID)
	if preferredLeftID != "" {
		_, loadErr := store.LoadSession(preferredLeftID)
		if loadErr != nil {
			return fmt.Errorf("load duel LEFT session %s: %w", preferredLeftID, loadErr)
		}
	}
	if leftCfg.Model == "" || rightCfg.Model == "" {
		return errors.New("--left-model and --right-model are required for non-COS providers")
	}
	if requestedGoal == "" {
		requestedGoal = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	waitingForGoal := false
	var state duelState
	var reused bool
	if preferredLeftID != "" {
		if requestedGoal == "" {
			if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				waitingForGoal = true
			} else if statErr != nil {
				return statErr
			}
		}
		if !waitingForGoal {
			state, reused, err = reconcileDuelStateForLeft(store, path, requestedGoal, preferredLeftID, leftCfg, rightCfg)
			if err != nil {
				return err
			}
		}
	} else {
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
	leftProvider, err := newDuelProvider(abs, activeLeftCfg.Provider, *baseURL, *apiKey, *ollamaURL)
	if err != nil {
		return err
	}
	rightProvider, err := newDuelProvider(abs, activeRightCfg.Provider, *baseURL, *apiKey, *ollamaURL)
	if err != nil {
		return err
	}
	if activeLeftCfg.Provider == "cos" {
		if lister, ok := leftProvider.(provider.ModelLister); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			models, listErr := lister.ListModels(ctx)
			cancel()
			if listErr != nil {
				return fmt.Errorf("resolve LEFT COS model: %w", listErr)
			}
			resolved := chooseAvailableModel(activeLeftCfg.Model, models, "gpt-5-6")
			if resolved == "" {
				return errors.New("LEFT COS provider offers no usable model")
			}
			activeLeftCfg.Model = resolved
			if !waitingForGoal {
				leftSession, loadErr := store.LoadSession(state.LeftID)
				if loadErr != nil {
					return loadErr
				}
				if _, saveErr := store.SaveSession(state.LeftID, resolved, activeLeftCfg.Provider, leftSession.Messages); saveErr != nil {
					return saveErr
				}
				state.LeftProvider = activeLeftCfg.Provider
				state.LeftModel = resolved
				if saveErr := saveDuelState(path, state); saveErr != nil {
					return saveErr
				}
			}
		}
	}
	if activeRightCfg.Provider == "cos" {
		if lister, ok := rightProvider.(provider.ModelLister); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			models, listErr := lister.ListModels(ctx)
			cancel()
			if listErr != nil {
				return fmt.Errorf("resolve RIGHT COS model: %w", listErr)
			}
			resolved := chooseAvailableModel(activeRightCfg.Model, models, "gpt-5-6")
			if resolved == "" {
				return errors.New("RIGHT COS provider offers no usable model")
			}
			activeRightCfg.Model = resolved
			if !waitingForGoal {
				rightSession, loadErr := store.LoadSession(state.RightID)
				if loadErr != nil {
					return loadErr
				}
				if _, saveErr := store.SaveSession(state.RightID, resolved, activeRightCfg.Provider, rightSession.Messages); saveErr != nil {
					return saveErr
				}
				state.RightProvider = activeRightCfg.Provider
				state.RightModel = resolved
				if saveErr := saveDuelState(path, state); saveErr != nil {
					return saveErr
				}
			}
		}
	}
	terminalService := tuiPkg.NewTerminalService(abs)
	defer terminalService.Close()
	var stateMu sync.Mutex
	var runMu sync.Mutex
	var runCancel context.CancelFunc
	var runDone chan struct{}
	var runGeneration uint64
	instructions := make(chan string, 128)
	stateMu.Lock()
	for _, note := range state.PendingOwner {
		if strings.TrimSpace(note) != "" {
			instructions <- note
		}
	}
	stateMu.Unlock()
	updates := make(chan duelUpdate, 128)
	executeRun := func(ctx context.Context, generation uint64) {
		stateMu.Lock()
		runGoal, runLeftID, runRightID, runSummary := state.Goal, state.LeftID, state.RightID, recurringInitialSummary(state)
		stateMu.Unlock()
		leftSession, loadErr := store.LoadSession(runLeftID)
		if loadErr != nil {
			updates <- duelUpdate{done: true, err: fmt.Errorf("load LEFT session history: %w", loadErr)}
			return
		}
		runLeftModel, runRightModel := activeLeftCfg.Model, activeRightCfg.Model
		leftHistory := append([]provider.Message(nil), leftSession.Messages...)
		baseOptions := duel.Options{Goal: runGoal, LeftID: runLeftID, RightID: runRightID, LeftModel: runLeftModel, RightModel: runRightModel, LeftStreamer: leftProvider, RightStreamer: rightProvider, LeftHistory: leftHistory, LeftRemoteHistory: activeLeftCfg.Provider == "cos", InitialSummary: runSummary, Instructions: instructions, RetryDelay: time.Minute, GoalFunc: func() string {
			stateMu.Lock()
			defer stateMu.Unlock()
			return state.Goal
		}, OnLeftExchange: func(round int, ownerNotes []string, prompt, response string) error {
			persistedUser := prompt
			if len(ownerNotes) > 0 {
				persistedUser = strings.Join(ownerNotes, "\n")
			}
			leftHistory = append(leftHistory,
				provider.Message{Role: provider.RoleUser, Content: persistedUser},
				provider.Message{Role: provider.RoleAssistant, Content: response},
			)
			if _, saveErr := store.SaveSession(runLeftID, runLeftModel, activeLeftCfg.Provider, leftHistory); saveErr != nil {
				return saveErr
			}
			if len(ownerNotes) > 0 {
				stateMu.Lock()
				if len(state.PendingOwner) >= len(ownerNotes) {
					matches := true
					for i := range ownerNotes {
						if state.PendingOwner[i] != ownerNotes[i] {
							matches = false
							break
						}
					}
					if matches {
						state.PendingOwner = append([]string(nil), state.PendingOwner[len(ownerNotes):]...)
					}
				}
				saveErr := saveDuelState(path, state)
				stateMu.Unlock()
				if saveErr != nil {
					return saveErr
				}
			}
			return nil
		}, OnRoleStart: func(role string, round int) error {
			stateMu.Lock()
			shownRound := state.Rounds + 1
			stateMu.Unlock()
			select {
			case updates <- duelUpdate{role: role, round: shownRound, active: true}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
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
		runMu.Lock()
		if runGeneration == generation {
			runCancel = nil
		}
		runMu.Unlock()
		updates <- duelUpdate{done: true, err: err, result: result}
	}
	run := func() {
		runMu.Lock()
		if runCancel != nil {
			runMu.Unlock()
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		runGeneration++
		generation := runGeneration
		done := make(chan struct{})
		runCancel = cancel
		runDone = done
		runMu.Unlock()
		go func() {
			executeRun(ctx, generation)
			close(done)
			runMu.Lock()
			if runGeneration == generation {
				runDone = nil
			}
			runMu.Unlock()
		}()
	}
	cancelCurrentRun := func() {
		runMu.Lock()
		cancel := runCancel
		runMu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	waitCurrentRun := func() {
		runMu.Lock()
		done := runDone
		runMu.Unlock()
		if done != nil {
			<-done
		}
	}
	cancelAndPersist := func() {
		cancelCurrentRun()
		stateMu.Lock()
		defer stateMu.Unlock()
		if state.Status == "running" {
			state.Status = "interrupted"
			_ = saveDuelState(path, state)
		}
	}
	newDuelSessions := func() (string, string, error) {
		stateMu.Lock()
		defer stateMu.Unlock()
		leftCfg := normalizeDuelRoleConfig(duelRoleConfig{Provider: state.LeftProvider, Model: state.LeftModel})
		rightCfg := normalizeDuelRoleConfig(duelRoleConfig{Provider: state.RightProvider, Model: state.RightModel})
		leftSession, createErr := store.StartSession(leftCfg.Model, leftCfg.Provider)
		if createErr != nil {
			return "", "", createErr
		}
		rightSession, createErr := store.StartSession(rightCfg.Model, rightCfg.Provider)
		if createErr != nil {
			return "", "", createErr
		}
		state.LeftID = leftSession.ID
		state.RightID = rightSession.ID
		state.Status = "running"
		state.LastError = ""
		if saveErr := saveDuelState(path, state); saveErr != nil {
			return "", "", saveErr
		}
		return leftSession.ID, rightSession.ID, nil
	}
	ui := newDuelUI(instructions, updates, run, cancelAndPersist, state.LeftID, state.RightID)
	ui.setOwnerQueueHandler(func(note string) error {
		stateMu.Lock()
		defer stateMu.Unlock()
		state.PendingOwner = append(state.PendingOwner, strings.TrimSpace(note))
		return saveDuelState(path, state)
	})
	if !waitingForGoal {
		leftSession, loadErr := store.LoadSession(state.LeftID)
		if loadErr != nil {
			return fmt.Errorf("load LEFT session for duel UI: %w", loadErr)
		}
		ui.setInitialLeftHistory(leftSession.Messages)
	}
	aux := tuiPkg.NewAuxiliaryWorkspace(abs, terminalService)
	aux.SetContextInfo(
		fmt.Sprintf("LEFT  · %s / %s", activeLeftCfg.Provider, activeLeftCfg.Model),
		fmt.Sprintf("RIGHT · %s / %s", activeRightCfg.Provider, activeRightCfg.Model),
	)
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
	ui.setNewSessionHandler(newDuelSessions)
	ui.setStopLoopHandler(cancelCurrentRun)
	if waitingForGoal {
		ui.setInitialGoalHandler(func(goal string) error {
			created, createErr := createDuelStateWithLeft(store, path, goal, preferredLeftID, leftCfg, rightCfg)
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
	cancelCurrentRun()
	waitCurrentRun()
	if err != nil {
		return err
	}
	if ui.switchMode == "single" {
		stateMu.Lock()
		leftID, leftProvider, leftModel := state.LeftID, state.LeftProvider, state.LeftModel
		stateMu.Unlock()
		return &modeSwitchError{Mode: "single", CWD: abs, SessionID: leftID, Provider: leftProvider, Model: leftModel}
	}
	return nil
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
