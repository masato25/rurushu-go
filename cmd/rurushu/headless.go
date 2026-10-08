package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	rurushuconfig "github.com/arborlogic/rurushu-go/config"
	"github.com/arborlogic/rurushu-go/harness"
	"github.com/arborlogic/rurushu-go/jobs"
	"github.com/arborlogic/rurushu-go/permission"
	"github.com/arborlogic/rurushu-go/projectstate"
	"github.com/arborlogic/rurushu-go/provider"
	"github.com/arborlogic/rurushu-go/tool"
	"github.com/arborlogic/rurushu-go/tool/builtin"
)

const headlessProtocolVersion = 2
const headlessMinimumProtocolVersion = 1
const maxHeadlessRequestBytes = 2 << 20

type headlessRequest struct {
	Version          int                 `json:"version"`
	ExecutionID      string              `json:"execution_id,omitempty"`
	Task             string              `json:"task"`
	CWD              string              `json:"cwd,omitempty"`
	Model            string              `json:"model,omitempty"`
	SystemPrompt     string              `json:"system_prompt,omitempty"`
	PromptFiles      []string            `json:"prompt_files,omitempty"`
	MaxSteps         int                 `json:"max_steps,omitempty"`
	MaxContextTokens int                 `json:"max_context_tokens,omitempty"`
	CompactAt        float64             `json:"compact_at,omitempty"`
	ToolProfile      string              `json:"tool_profile,omitempty"`
	PermissionMode   string              `json:"permission_mode,omitempty"`
	ExternalTools    []tool.ExternalSpec `json:"external_tools,omitempty"`
}

type headlessUsage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

type headlessResponse struct {
	Version     int           `json:"version"`
	ExecutionID string        `json:"execution_id,omitempty"`
	Status      string        `json:"status"`
	Text        string        `json:"text,omitempty"`
	Reasoning   string        `json:"reasoning,omitempty"`
	Usage       headlessUsage `json:"usage,omitempty"`
	ToolCalls   int           `json:"tool_calls,omitempty"`
	Compactions int           `json:"compactions,omitempty"`
	ErrorCode   string        `json:"error_code,omitempty"`
	RetryAfter  int64         `json:"retry_after_ms,omitempty"`
	Error       string        `json:"error,omitempty"`
}

func headlessRunRequested(args []string) bool {
	if len(args) > 0 {
		return strings.HasPrefix(args[0], "-")
	}
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice == 0
}

func runHeadlessCLI(args []string) error {
	fs := flag.NewFlagSet("rurushu run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	inputPath := fs.String("input", "-", "JSON request path, or - for stdin")
	outputPath := fs.String("output", "-", "JSON response path, or - for stdout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("rurushu run does not accept positional arguments")
	}

	in, closeInput, err := headlessInput(*inputPath)
	if err != nil {
		return err
	}
	defer closeInput()
	out, closeOutput, err := headlessOutput(*outputPath)
	if err != nil {
		return err
	}
	defer closeOutput()

	req, decodeErr := decodeHeadlessRequest(in)
	if decodeErr != nil {
		resp := headlessResponse{Version: headlessProtocolVersion, Status: "error", ErrorCode: "invalid_request", Error: decodeErr.Error()}
		if err := json.NewEncoder(out).Encode(resp); err != nil {
			return fmt.Errorf("encode headless response: %w", err)
		}
		return decodeErr
	}
	resp, runErr := executeHeadless(context.Background(), req)
	if err := json.NewEncoder(out).Encode(resp); err != nil {
		return fmt.Errorf("encode headless response: %w", err)
	}
	return runErr
}

func decodeHeadlessRequest(in io.Reader) (headlessRequest, error) {
	data, err := io.ReadAll(io.LimitReader(in, maxHeadlessRequestBytes+1))
	if err != nil {
		return headlessRequest{}, fmt.Errorf("read headless request: %w", err)
	}
	if len(data) > maxHeadlessRequestBytes {
		return headlessRequest{}, fmt.Errorf("headless request exceeds %d bytes", maxHeadlessRequestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var req headlessRequest
	if err := decoder.Decode(&req); err != nil {
		return headlessRequest{}, fmt.Errorf("decode headless request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return headlessRequest{}, fmt.Errorf("decode headless request: trailing JSON value")
		}
		return headlessRequest{}, fmt.Errorf("decode headless request: trailing data: %w", err)
	}
	return req, nil
}

func executeHeadless(ctx context.Context, req headlessRequest) (headlessResponse, error) {
	fail := func(err error) (headlessResponse, error) {
		resp := headlessFailure(err)
		if req.Version >= headlessMinimumProtocolVersion && req.Version <= headlessProtocolVersion {
			resp.Version = req.Version
		}
		resp.ExecutionID = req.ExecutionID
		return resp, err
	}
	failInvalid := func(err error) (headlessResponse, error) {
		resp := headlessFailure(err)
		if req.Version >= headlessMinimumProtocolVersion && req.Version <= headlessProtocolVersion {
			resp.Version = req.Version
		}
		resp.ExecutionID = req.ExecutionID
		resp.ErrorCode = "invalid_request"
		resp.RetryAfter = 0
		return resp, err
	}
	if req.Version < headlessMinimumProtocolVersion || req.Version > headlessProtocolVersion {
		return failInvalid(fmt.Errorf("unsupported request version %d", req.Version))
	}
	if req.Version == 1 && len(req.ExternalTools) > 0 {
		return failInvalid(fmt.Errorf("external_tools requires request version 2"))
	}
	if strings.TrimSpace(req.Task) == "" {
		return failInvalid(fmt.Errorf("task is required"))
	}

	saved, err := rurushuconfig.Load()
	if err != nil {
		return fail(err)
	}
	cwd := strings.TrimSpace(req.CWD)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return fail(fmt.Errorf("resolve cwd: %w", err))
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return failInvalid(fmt.Errorf("cwd %q: %w", cwd, err))
	}

	model := firstNonEmpty(req.Model, os.Getenv("RURUSHU_MODEL"), os.Getenv("OPENAI_MODEL"), saved.Model)
	if model == "" {
		return failInvalid(fmt.Errorf("model is required"))
	}
	baseURL := firstNonEmpty(os.Getenv("RURUSHU_BASE_URL"), os.Getenv("OPENAI_BASE_URL"), saved.BaseURL, "https://api.openai.com/v1")
	apiKey := firstNonEmpty(os.Getenv("RURUSHU_API_KEY"), os.Getenv("OPENAI_API_KEY"), saved.APIKey)
	systemPrompt := firstNonEmpty(req.SystemPrompt, saved.SystemPrompt, rurushuconfig.DefaultSystemPrompt)
	maxSteps := req.MaxSteps
	if maxSteps <= 0 {
		maxSteps = saved.MaxSteps
	}
	maxContext := req.MaxContextTokens
	if maxContext <= 0 {
		maxContext = saved.MaxContextTokens
	}
	compactAt := req.CompactAt
	if compactAt <= 0 {
		compactAt = saved.CompactAt
	}
	if maxSteps <= 0 || maxContext <= 0 || compactAt < 0 || compactAt > 100 {
		return failInvalid(fmt.Errorf("max_steps/max_context_tokens must be positive and compact_at must be between 0 and 100"))
	}

	toolProfile := strings.ToLower(strings.TrimSpace(req.ToolProfile))
	if toolProfile == "" {
		toolProfile = "readonly"
	}
	if toolProfile != "none" && toolProfile != "readonly" && toolProfile != "execution" {
		return failInvalid(fmt.Errorf("unsupported tool_profile %q", req.ToolProfile))
	}
	permissionMode := strings.ToLower(strings.TrimSpace(req.PermissionMode))
	if permissionMode == "" {
		permissionMode = "deny"
	}
	if permissionMode != "deny" && permissionMode != "allow" {
		return failInvalid(fmt.Errorf("unsupported permission_mode %q", req.PermissionMode))
	}
	if toolProfile == "execution" && permissionMode != "allow" {
		return failInvalid(fmt.Errorf("tool_profile %q requires permission_mode %q", "execution", "allow"))
	}

	registry := tool.NewRegistry()
	switch toolProfile {
	case "none":
	case "readonly":
		builtin.RegisterReadOnly(registry)
	case "execution":
		builtin.RegisterReadOnly(registry)
		projectStore, openErr := projectstate.Open(cwd)
		if openErr != nil {
			return fail(fmt.Errorf("initialize project state: %w", openErr))
		}
		manager, openErr := jobs.Open(cwd, projectStore.Dir, "")
		if openErr != nil {
			return fail(fmt.Errorf("initialize managed jobs: %w", openErr))
		}
		if _, openErr = manager.List(); openErr != nil {
			return fail(fmt.Errorf("reconcile managed jobs: %w", openErr))
		}
		builtin.RegisterExecution(registry, manager)
	}
	for _, spec := range req.ExternalTools {
		if _, exists := registry.Get(strings.TrimSpace(spec.ID)); exists {
			return failInvalid(fmt.Errorf("external tool %q conflicts with an existing tool", spec.ID))
		}
		externalTool, toolErr := tool.NewExternal(spec)
		if toolErr != nil {
			return failInvalid(toolErr)
		}
		registry.Register(externalTool)
	}

	var perm permission.Handler = permission.DenyAll{}
	switch permissionMode {
	case "deny":
	case "allow":
		perm = permission.AllowAll{}
	}

	prov := provider.NewOpenAICompatibleProvider(baseURL, apiKey)
	client, err := harness.New(prov, harness.Config{
		CWD:                cwd,
		SystemPrompt:       systemPrompt,
		PromptFiles:        req.PromptFiles,
		Tools:              registry,
		ToolMaxSteps:       maxSteps,
		MaxContextTokens:   maxContext,
		AutoCompactPercent: compactAt,
		Permission:         perm,
	})
	if err != nil {
		return fail(err)
	}
	result, err := client.Run(ctx, provider.CompletionRequest{
		Model:    model,
		Messages: []provider.Message{{Role: provider.RoleUser, Content: strings.TrimSpace(req.Task)}},
	})
	if err != nil {
		return fail(err)
	}
	return headlessResponse{
		Version:     req.Version,
		ExecutionID: req.ExecutionID,
		Status:      "completed",
		Text:        result.Text,
		Reasoning:   result.Reasoning,
		Usage: headlessUsage{
			PromptTokens: result.Usage.PromptTokens, CompletionTokens: result.Usage.CompletionTokens, TotalTokens: result.Usage.TotalTokens,
		},
		ToolCalls: result.ToolCalls, Compactions: result.Compactions,
	}, nil
}

func headlessFailure(err error) headlessResponse {
	code := "execution_failed"
	var retryAfter int64
	switch {
	case errors.Is(err, harness.ErrMaxSteps):
		code = "budget_exhausted"
	case errors.Is(err, context.Canceled):
		code = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "transient"
	default:
		if httpErr, limited := provider.IsRateLimitError(err); limited {
			code = "rate_limited"
			if httpErr.RetryAfter > 0 {
				retryAfter = httpErr.RetryAfter.Milliseconds()
			}
		} else {
			var httpErr *provider.HTTPError
			if errors.As(err, &httpErr) && httpErr != nil && (httpErr.StatusCode == http.StatusRequestTimeout || httpErr.StatusCode == http.StatusTooEarly || httpErr.StatusCode >= http.StatusInternalServerError) {
				code = "transient"
				if httpErr.RetryAfter > 0 {
					retryAfter = httpErr.RetryAfter.Milliseconds()
				}
			}
		}
	}
	return headlessResponse{
		Version: headlessProtocolVersion, Status: "error", ErrorCode: code, RetryAfter: retryAfter, Error: err.Error(),
	}
}

func headlessInput(path string) (io.Reader, func(), error) {
	if strings.TrimSpace(path) == "" || path == "-" {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open headless input: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}

func headlessOutput(path string) (io.Writer, func(), error) {
	if strings.TrimSpace(path) == "" || path == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open headless output: %w", err)
	}
	return f, func() { _ = f.Close() }, nil
}
