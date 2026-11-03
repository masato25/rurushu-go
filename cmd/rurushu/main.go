package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	rurushuconfig "github.com/masato25/rurushu-go/config"
	"github.com/masato25/rurushu-go/harness"
	"github.com/masato25/rurushu-go/jobs"
	"github.com/masato25/rurushu-go/permission"
	"github.com/masato25/rurushu-go/projectstate"
	"github.com/masato25/rurushu-go/provider"
	"github.com/masato25/rurushu-go/tool"
	"github.com/masato25/rurushu-go/tool/builtin"
	"github.com/masato25/rurushu-go/tui"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("value cannot be empty")
	}
	*s = append(*s, value)
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rurushu:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	current := append([]string(nil), args...)
	for {
		var err error
		if len(current) > 0 && current[0] == "duel" {
			err = runDuel(current[1:])
		} else {
			err = runSingle(current)
		}
		var switchErr *modeSwitchError
		if !errors.As(err, &switchErr) {
			return err
		}
		switch switchErr.Mode {
		case "single":
			current = []string{"--cwd", switchErr.CWD, "--resume", switchErr.SessionID, "--provider", switchErr.Provider, "--model", switchErr.Model}
		case "duel":
			current = []string{"duel", "--cwd", switchErr.CWD, "--left-session", switchErr.SessionID, "--left-provider", switchErr.Provider, "--left-model", switchErr.Model}
		default:
			return err
		}
	}
}

type modeSwitchError struct {
	Mode      string
	CWD       string
	SessionID string
	Provider  string
	Model     string
}

func (e *modeSwitchError) Error() string { return "switch mode to " + e.Mode }

func runSingle(args []string) error {
	if len(args) > 0 && args[0] == "--init" {
		return runInit(args[1:])
	}
	if len(args) > 0 && args[0] == "handoff" {
		return runHandoff(args[1:])
	}
	if len(args) > 0 && args[0] == "__job-runner" {
		return runJobRunner(args[1:])
	}
	if len(args) > 0 && args[0] == "run" && headlessRunRequested(args[1:]) {
		return runHeadlessCLI(args[1:])
	}
	if len(args) > 0 && args[0] == "setup" {
		return runSetup(args[1:])
	}

	cwdDefault, _ := os.Getwd()
	saved, err := rurushuconfig.LoadForDir(cwdDefault)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("rurushu", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	providerName := fs.String("provider", firstNonEmpty(os.Getenv("RURUSHU_PROVIDER"), saved.Single.Provider, "openai-compatible"), "provider: openai-compatible or cos")
	baseURL := fs.String("base-url", firstNonEmpty(os.Getenv("RURUSHU_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), saved.BaseURL, "https://api.openai.com/v1"), "OpenAI-compatible API base URL")
	apiKey := fs.String("api-key", firstNonEmpty(os.Getenv("RURUSHU_API_KEY"), os.Getenv("OPENAI_API_KEY"), saved.APIKey), "API key (prefer env vars)")
	modelName := fs.String("model", firstNonEmpty(os.Getenv("RURUSHU_MODEL"), os.Getenv("OPENAI_MODEL"), saved.Single.Model, saved.Model), "model name")
	cwd := fs.String("cwd", cwdDefault, "working directory exposed to the harness")
	systemPrompt := fs.String("system-prompt", firstNonEmpty(saved.SystemPrompt, rurushuconfig.DefaultSystemPrompt), "system prompt")
	maxSteps := fs.Int("max-steps", saved.MaxSteps, "maximum model/tool turns per request")
	maxContext := fs.Int("max-context-tokens", saved.MaxContextTokens, "approximate context budget before compaction")
	compactAt := fs.Float64("compact-at", saved.CompactAt, "context compaction threshold percent")
	activity := fs.String("activity", firstNonEmpty(os.Getenv("RURUSHU_ACTIVITY"), "normal"), "activity display: normal, verbose, or debug")
	listModels := fs.Bool("list-models", false, "list models and exit")
	autoApprove := fs.Bool("auto-approve", saved.AutoApprove, "automatically approve all tool operations (unsafe; trusted tasks only)")
	resumeSession := fs.String("resume", "", "resume project session by id or 'last'")
	continueLast := fs.Bool("c", false, "continue the last project session (same as --resume last)")
	forceContinueLast := fs.Bool("kc", false, "force continue the last session despite existing owner (unsafe)")
	pprofAddr := fs.String("pprof", "", "enable local Go profiling on 127.0.0.1:PORT (development only)")
	var promptFiles stringList
	fs.Var(&promptFiles, "prompt-file", "append a prompt file; repeatable")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *continueLast && strings.TrimSpace(*resumeSession) != "" {
		return errors.New("-c cannot be combined with --resume")
	}
	if *forceContinueLast && *continueLast {
		return errors.New("-kc cannot be combined with -c")
	}
	if *pprofAddr != "" {
		if err := startLocalPprof(*pprofAddr); err != nil {
			return err
		}
	}
	if *maxSteps <= 0 || *maxContext <= 0 || *compactAt < 0 || *compactAt > 100 {
		return errors.New("max-steps/max-context-tokens must be positive and compact-at must be between 0 and 100")
	}
	activityMode, err := tui.ParseActivityMode(*activity)
	if err != nil {
		return err
	}
	absCWD, err := filepath.Abs(strings.TrimSpace(*cwd))
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	if info, err := os.Stat(absCWD); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return fmt.Errorf("cwd %q: %w", absCWD, err)
	}

	var prov interface {
		provider.Provider
		provider.ModelLister
		ID() string
	}
	switch strings.ToLower(strings.TrimSpace(*providerName)) {
	case "openai-compatible", "openai":
		prov = provider.NewOpenAICompatibleProvider(strings.TrimSpace(*baseURL), strings.TrimSpace(*apiKey))
	case "cos", "chat-on-steroids":
		cosProvider := provider.NewCOSProvider(absCWD)
		// The interactive TUI owns the terminal. Writing COS progress messages
		// directly to stderr leaves those lines behind outside Bubble Tea's
		// redraw lifecycle, so keep provider diagnostics silent here. Headless
		// mode still uses the provider's default stderr logger.
		cosProvider.Stderr = nil
		prov = cosProvider
	default:
		return fmt.Errorf("unsupported provider %q", *providerName)
	}
	if *listModels {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models, err := prov.ListModels(ctx)
		if err != nil {
			return err
		}
		for _, model := range models {
			fmt.Println(model)
		}
		return nil
	}

	modelExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "model" {
			modelExplicit = true
		}
	})
	model := resolveSingleModel(prov.ID(), *modelName, modelExplicit, os.Getenv("RURUSHU_MODEL"), os.Getenv("OPENAI_MODEL"), saved.Single.Model)
	if prov.ID() == "cos" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		models, listErr := prov.ListModels(ctx)
		cancel()
		if listErr != nil {
			return fmt.Errorf("resolve COS model: %w", listErr)
		}
		resolved := chooseAvailableModel(model, models, "gpt-5-6")
		if resolved == "" {
			return errors.New("Chat On Steroids offers no usable model")
		}
		if resolved != model && strings.TrimSpace(model) != "" {
			fmt.Fprintf(os.Stderr, "rurushu: COS model %q unavailable; using %q\n", model, resolved)
		}
		model = resolved
	}
	if model == "" && prov.ID() == "cos" {
		model = "chatgpt-web"
	}
	if model == "" {
		return errors.New("model is required; use --model, RURUSHU_MODEL, or OPENAI_MODEL (use --list-models to inspect the endpoint)")
	}
	projectStore, err := projectstate.Open(absCWD)
	if err != nil {
		return fmt.Errorf("initialize project state: %w", err)
	}
	projectStore.SetForceSessionOwnership(*forceContinueLast)
	jobManager, err := jobs.Open(absCWD, projectStore.Dir, "")
	if err != nil {
		return fmt.Errorf("initialize managed jobs: %w", err)
	}
	if _, err := jobManager.List(); err != nil {
		return fmt.Errorf("reconcile managed jobs: %w", err)
	}
	var session projectstate.Session
	resumeID := strings.TrimSpace(*resumeSession)
	if *continueLast || (*forceContinueLast && resumeID == "") {
		resumeID = "last"
	}
	if resumeID == "" {
		session, err = projectStore.StartSession(model, prov.ID())
	} else if resumeID == "last" {
		session, err = projectStore.LastSession()
		if err == nil {
			session, err = projectStore.ActivateSession(session.ID)
		}
	} else {
		session, err = projectStore.ActivateSession(resumeID)
	}
	if err != nil {
		return fmt.Errorf("initialize project session: %w", err)
	}
	if err := projectStore.HoldSession(session.ID); err != nil {
		return err
	}
	if *forceContinueLast {
		fmt.Fprintln(os.Stderr, "rurushu: WARNING: -kc bypasses session ownership; concurrent clients may overwrite history")
	}
	defer projectStore.ReleaseSession()

	permissionHandler, permissionRequests := newPermissionHandler(*autoApprove)
	registry := tool.NewRegistry()
	builtin.RegisterReadOnly(registry)
	builtin.RegisterExecution(registry, jobManager)
	for _, mode := range []string{"create", "status", "stop"} {
		registry.Register(&watchTool{stateDir: projectStore.Dir, mode: mode})
	}
	terminalService := tui.NewTerminalService(absCWD)
	defer terminalService.Close()
	registry.Register(&terminalSendTool{service: terminalService})
	client, err := harness.New(prov, harness.Config{
		CWD:                absCWD,
		SystemPrompt:       *systemPrompt,
		PromptFiles:        promptFiles,
		Tools:              registry,
		ToolMaxSteps:       *maxSteps,
		MaxContextTokens:   *maxContext,
		AutoCompactPercent: *compactAt,
		Permission:         permissionHandler,
	})
	if err != nil {
		return err
	}

	uiModel := tui.NewWithStreamer(model, prov.ID(), client)
	uiModel.SetActivityMode(activityMode)
	uiModel.SetPermissionRequests(permissionRequests)
	uiModel.SetJobManager(jobManager)
	uiModel.SetTerminalService(terminalService)
	uiModel.SetProjectSession(projectStore, session)
	if len(fs.Args()) > 0 {
		uiModel.SetInitialInput(strings.Join(fs.Args(), " "))
	}
	_, err = tea.NewProgram(uiModel).Run()
	if err != nil {
		return err
	}
	if uiModel.ReloadRequested() {
		return rebuildAndReload(args, uiModel.SessionID())
	}
	if uiModel.ModeSwitchRequested() == "duel" {
		return &modeSwitchError{Mode: "duel", CWD: absCWD, SessionID: uiModel.SessionID(), Provider: prov.ID(), Model: model}
	}
	return nil
}

func startLocalPprof(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || (host != "127.0.0.1" && host != "localhost" && host != "[::1]" && host != "::1") {
		return fmt.Errorf("pprof address must be a loopback host and port (example: 127.0.0.1:6060)")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("start local pprof: %w", err)
	}
	go func() { _ = http.Serve(listener, http.DefaultServeMux) }()
	return nil
}

func reloadArgs(args []string, sessionID string) []string {
	result := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c", "--resume", "-resume":
			if args[i] != "-c" && i+1 < len(args) {
				i++
			}
		case "--resume=last", "-resume=last":
		default:
			if !strings.HasPrefix(args[i], "--resume=") && !strings.HasPrefix(args[i], "-resume=") {
				result = append(result, args[i])
			}
		}
	}
	return append(result, "--resume", sessionID)
}

func rebuildAndReload(args []string, sessionID string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Dir(filepath.Dir(executable))
	buildScript := filepath.Join(root, "build.sh")
	if _, err := os.Stat(buildScript); err != nil {
		return fmt.Errorf("reload requires source checkout at %s: %w", root, err)
	}
	fmt.Fprintln(os.Stderr, "rurushu: building new version before resuming", sessionID)
	build := exec.Command("bash", buildScript, "--skip-tests")
	build.Dir = root
	build.Stdin, build.Stdout, build.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("reload build failed (session retained, rerun with --resume %s): %w", sessionID, err)
	}
	// Replace the current process rather than nesting a new Rurushu child.
	// A child with Run() leaves every previous reload waiting as a parent,
	// accumulating idle processes and permitting concurrent session owners.
	bin := filepath.Join(root, "bin", "rurushu")
	return syscall.Exec(bin, append([]string{bin}, reloadArgs(args, sessionID)...), os.Environ())
}

func newPermissionHandler(autoApprove bool) (permission.Handler, <-chan *permission.Prompt) {
	if autoApprove {
		return permission.AllowAll{}, nil
	}
	handler := permission.NewTUIHandler()
	return handler, handler.Requests()
}

func runJobRunner(args []string) error {
	fs := flag.NewFlagSet("rurushu __job-runner", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", "", "project .rurushu directory")
	jobID := fs.Int("job", 0, "managed job id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jobID <= 0 || strings.TrimSpace(*stateDir) == "" {
		return errors.New("job runner requires --state-dir and --job")
	}
	absState, err := filepath.Abs(strings.TrimSpace(*stateDir))
	if err != nil {
		return err
	}
	projectRoot := filepath.Dir(absState)
	manager, err := jobs.Open(projectRoot, absState, "")
	if err != nil {
		return err
	}
	return manager.RunJob(context.Background(), *jobID)
}

func runSetup(args []string) error {
	fs := flag.NewFlagSet("rurushu setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("setup does not accept positional arguments")
	}
	cfg, err := rurushuconfig.Load()
	if err != nil {
		return err
	}
	model := tui.NewSetupModel(cfg)
	_, err = tea.NewProgram(model).Run()
	return err
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func resolveSingleModel(providerID, parsedModel string, explicit bool, rurushuEnv, openAIEnv, singleModel string) string {
	if providerID == "cos" && !explicit && strings.TrimSpace(rurushuEnv) == "" && strings.TrimSpace(openAIEnv) == "" && strings.TrimSpace(singleModel) == "" {
		return "chatgpt-web"
	}
	return strings.TrimSpace(parsedModel)
}

func chooseAvailableModel(requested string, available []string, preferred string) string {
	requested = strings.TrimSpace(requested)
	preferred = strings.TrimSpace(preferred)
	for _, model := range available {
		if strings.TrimSpace(model) == requested && requested != "" {
			return requested
		}
	}
	for _, model := range available {
		if strings.TrimSpace(model) == preferred && preferred != "" {
			return preferred
		}
	}
	for _, model := range available {
		if model = strings.TrimSpace(model); model != "" {
			return model
		}
	}
	return ""
}
