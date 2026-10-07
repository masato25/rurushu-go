package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/arborlogic/rurushu-go/permission"
	"github.com/arborlogic/rurushu-go/provider"
	"github.com/arborlogic/rurushu-go/tool"
)

type ContextSource interface {
	Context(ctx context.Context, cwd string, maxBytes int) ([]string, error)
}

type ContextFunc func(context.Context, string, int) ([]string, error)

func (f ContextFunc) Context(ctx context.Context, cwd string, maxBytes int) ([]string, error) {
	return f(ctx, cwd, maxBytes)
}

type Config struct {
	CWD                string
	SystemPrompt       string
	AppendSystemPrompt string
	PromptFiles        []string
	Context            ContextSource
	ContextMaxBytes    int
	Tools              *tool.Registry
	ToolMaxSteps       int
	MaxContextTokens   int
	AutoCompactPercent float64
	Permission         permission.Handler
}

type Client struct {
	provider           provider.Provider
	systemPrompt       string
	cwd                string
	context            ContextSource
	contextMax         int
	tools              *tool.Registry
	toolMaxSteps       int
	maxContextTokens   int
	autoCompactPercent float64
	permission         permission.Handler
}

func New(prov provider.Provider, cfg Config) (*Client, error) {
	if prov == nil {
		return nil, fmt.Errorf("harness provider is required")
	}
	cwd := strings.TrimSpace(cfg.CWD)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}

	parts := make([]string, 0, len(cfg.PromptFiles)+2)
	if value := strings.TrimSpace(cfg.SystemPrompt); value != "" {
		parts = append(parts, value)
	}
	for _, path := range cfg.PromptFiles {
		content, err := readPromptFile(path, cwd)
		if err != nil {
			return nil, err
		}
		if content != "" {
			parts = append(parts, content)
		}
	}
	if value := strings.TrimSpace(cfg.AppendSystemPrompt); value != "" {
		parts = append(parts, value)
	}
	systemPrompt := strings.ReplaceAll(strings.Join(parts, "\n\n"), "{{cwd}}", cwd)

	contextMax := cfg.ContextMaxBytes
	if contextMax <= 0 {
		contextMax = 8192
	}
	toolMaxSteps := cfg.ToolMaxSteps
	if toolMaxSteps <= 0 {
		toolMaxSteps = 8
	}
	maxContextTokens := cfg.MaxContextTokens
	if maxContextTokens <= 0 {
		maxContextTokens = 24576
	}
	autoCompactPercent := cfg.AutoCompactPercent
	if autoCompactPercent < 0 {
		autoCompactPercent = 0
	} else if autoCompactPercent == 0 {
		autoCompactPercent = 80
	} else if autoCompactPercent <= 1 {
		autoCompactPercent *= 100
	}
	if autoCompactPercent > 100 {
		autoCompactPercent = 100
	}
	perm := cfg.Permission
	if cfg.Tools != nil && perm == nil {
		perm = permission.DenyAll{}
	}

	return &Client{
		provider: prov, systemPrompt: systemPrompt, cwd: cwd, context: cfg.Context,
		contextMax: contextMax, tools: cfg.Tools, toolMaxSteps: toolMaxSteps,
		maxContextTokens: maxContextTokens, autoCompactPercent: autoCompactPercent, permission: perm,
	}, nil
}

func (c *Client) Stream(ctx context.Context, req provider.CompletionRequest) (<-chan provider.StreamEvent, error) {
	enriched, err := c.Enrich(ctx, req)
	if err != nil {
		return nil, err
	}
	if c.tools == nil || len(enriched.Tools) == 0 {
		return c.provider.Stream(ctx, enriched)
	}
	return c.streamWithTools(ctx, enriched), nil
}

func (c *Client) Enrich(ctx context.Context, req provider.CompletionRequest) (provider.CompletionRequest, error) {
	enriched := req
	enriched.Messages = append([]provider.Message(nil), req.Messages...)
	enriched.Tools = append([]map[string]any(nil), req.Tools...)
	system := strings.TrimSpace(c.systemPrompt)
	if c.context != nil {
		entries, err := c.context.Context(ctx, c.cwd, c.contextMax)
		if err != nil {
			return provider.CompletionRequest{}, err
		}
		if len(entries) > 0 {
			var b strings.Builder
			b.WriteString("# Context")
			for _, entry := range entries {
				if value := strings.TrimSpace(entry); value != "" {
					b.WriteString("\n- ")
					b.WriteString(value)
				}
			}
			if b.Len() > len("# Context") {
				if system != "" {
					system += "\n\n"
				}
				system += b.String()
			}
		}
	}
	if system != "" {
		enriched.Messages = append([]provider.Message{{Role: provider.RoleSystem, Content: system}}, enriched.Messages...)
	}
	if c.tools != nil {
		enriched.Tools = c.tools.Schemas()
	}
	return enriched, nil
}

func readPromptFile(path, cwd string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("prompt file path cannot be empty")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read prompt file %q: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}
