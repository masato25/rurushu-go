package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	rurushuconfig "github.com/arborlogic/rurushu-go/config"
	"github.com/arborlogic/rurushu-go/harness"
	"github.com/arborlogic/rurushu-go/permission"
	"github.com/arborlogic/rurushu-go/provider"
	"github.com/arborlogic/rurushu-go/tui"
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
	if len(args) > 0 && args[0] == "setup" {
		return runSetup(args[1:])
	}

	saved, err := rurushuconfig.Load()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("rurushu", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	baseURL := fs.String("base-url", firstNonEmpty(os.Getenv("RURUSHU_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), saved.BaseURL, "https://api.openai.com/v1"), "OpenAI-compatible API base URL")
	apiKey := fs.String("api-key", firstNonEmpty(os.Getenv("RURUSHU_API_KEY"), os.Getenv("OPENAI_API_KEY"), saved.APIKey), "API key (prefer env vars)")
	modelName := fs.String("model", firstNonEmpty(os.Getenv("RURUSHU_MODEL"), os.Getenv("OPENAI_MODEL"), saved.Model), "model name")
	cwdDefault, _ := os.Getwd()
	cwd := fs.String("cwd", cwdDefault, "working directory exposed to the harness")
	systemPrompt := fs.String("system-prompt", firstNonEmpty(saved.SystemPrompt, rurushuconfig.DefaultSystemPrompt), "system prompt")
	maxSteps := fs.Int("max-steps", saved.MaxSteps, "maximum model/tool turns per request")
	maxContext := fs.Int("max-context-tokens", saved.MaxContextTokens, "approximate context budget before compaction")
	compactAt := fs.Float64("compact-at", saved.CompactAt, "context compaction threshold percent")
	listModels := fs.Bool("list-models", false, "list models and exit")
	var promptFiles stringList
	fs.Var(&promptFiles, "prompt-file", "append a prompt file; repeatable")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *maxSteps <= 0 || *maxContext <= 0 || *compactAt < 0 || *compactAt > 100 {
		return errors.New("max-steps/max-context-tokens must be positive and compact-at must be between 0 and 100")
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

	prov := provider.NewOpenAICompatibleProvider(strings.TrimSpace(*baseURL), strings.TrimSpace(*apiKey))
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

	model := strings.TrimSpace(*modelName)
	if model == "" {
		return errors.New("model is required; use --model, RURUSHU_MODEL, or OPENAI_MODEL (use --list-models to inspect the endpoint)")
	}

	permissionUI := permission.NewTUIHandler()
	client, err := harness.New(prov, harness.Config{
		CWD:                absCWD,
		SystemPrompt:       *systemPrompt,
		PromptFiles:        promptFiles,
		ToolMaxSteps:       *maxSteps,
		MaxContextTokens:   *maxContext,
		AutoCompactPercent: *compactAt,
		Permission:         permissionUI,
	})
	if err != nil {
		return err
	}

	uiModel := tui.NewWithStreamer(model, prov.ID(), client)
	uiModel.SetPermissionRequests(permissionUI.Requests())
	if len(fs.Args()) > 0 {
		uiModel.SetInitialInput(strings.Join(fs.Args(), " "))
	}
	_, err = tea.NewProgram(uiModel).Run()
	return err
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
