package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/arborlogic/rurushu-go/jobs"
	"github.com/arborlogic/rurushu-go/permission"
	"github.com/arborlogic/rurushu-go/tool"
)

const maxBashOutput = 64 * 1024

type BashTool struct{ Jobs *jobs.Manager }

type bashArgs struct {
	Command        string `json:"command"`
	Background     bool   `json:"background,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

func (*BashTool) ID() string { return "bash" }
func (*BashTool) Description() string {
	return "Execute a shell command in the project working directory. Use background=true for long-running servers, watchers, or daemons; those become managed Rurushu jobs that survive TUI exit."
}
func (*BashTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":        map[string]any{"type": "string", "description": "Shell command to execute."},
			"background":     map[string]any{"type": "boolean", "description": "Run as a managed persistent job."},
			"timeoutSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 1800, "description": "Foreground timeout; defaults to 120 seconds."},
		},
		"required":             []string{"command"},
		"additionalProperties": false,
	}
}

func (b *BashTool) Execute(ctx context.Context, raw json.RawMessage, execCtx *tool.ExecutionContext) (*tool.Result, error) {
	var args bashArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolError("Bash error", "invalid arguments: %v", err), nil
	}
	args.Command = strings.TrimSpace(args.Command)
	if args.Command == "" {
		return toolError("Bash error", "command is required"), nil
	}
	if execCtx == nil || execCtx.AskPermission == nil {
		return toolError("Bash denied", "permission handler is required"), nil
	}
	description := "Execute shell command"
	if args.Background {
		description = "Start persistent managed job"
	}
	if !execCtx.AskPermission(permission.Request{Tool: "bash", Pattern: args.Command, Description: description + ": " + args.Command}) {
		return toolError("Bash denied", "permission denied"), nil
	}

	if args.Background {
		if b == nil || b.Jobs == nil {
			return toolError("Bash error", "managed job service is unavailable"), nil
		}
		job, err := b.Jobs.Start(args.Command)
		if err != nil {
			return toolError("Bash error", "%v", err), nil
		}
		return &tool.Result{
			Title:    fmt.Sprintf("Started job #%d", job.ID),
			Output:   fmt.Sprintf("Started managed job #%d\nstatus: %s\npid: %d\ncommand: %s\nUse job_output or /jobs %d to inspect logs.", job.ID, job.Status, job.RunnerPID, job.Command, job.ID),
			Metadata: map[string]any{"job_id": job.ID, "background": true},
		}, nil
	}

	timeout := args.TimeoutSeconds
	if timeout <= 0 {
		timeout = 120
	}
	if timeout > 1800 {
		timeout = 1800
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := foregroundShellCommand(runCtx, args.Command)
	if execCtx != nil && strings.TrimSpace(execCtx.CWD) != "" {
		cmd.Dir = execCtx.CWD
	}
	var output cappedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	text := strings.TrimSpace(output.String())
	if output.truncated {
		text += "\n\n[output truncated at 64 KiB]"
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return toolError("Bash timeout", "command exceeded %d seconds%s", timeout, suffixOutput(text)), nil
	}
	if err != nil {
		exitCode := -1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		return toolError("Bash failed", "exit code %d%s", exitCode, suffixOutput(text)), nil
	}
	if text == "" {
		text = "(no output)"
	}
	return &tool.Result{Title: "Bash", Output: text}, nil
}

func foregroundShellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd.exe", "/C", command)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-lc", command)
}

type cappedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := maxBashOutput - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buf.Write(p)
	return original, nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

func suffixOutput(output string) string {
	if strings.TrimSpace(output) == "" {
		return ""
	}
	return "\n\n" + output
}

type JobListTool struct{ Jobs *jobs.Manager }

func (*JobListTool) ID() string { return "job_list" }
func (*JobListTool) Description() string {
	return "List Rurushu-managed background jobs for the current project, including jobs restored from previous TUI sessions."
}
func (*JobListTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (t *JobListTool) Execute(context.Context, json.RawMessage, *tool.ExecutionContext) (*tool.Result, error) {
	if t == nil || t.Jobs == nil {
		return toolError("Jobs error", "managed job service is unavailable"), nil
	}
	all, err := t.Jobs.List()
	if err != nil {
		return toolError("Jobs error", "%v", err), nil
	}
	if len(all) == 0 {
		return &tool.Result{Title: "Managed jobs", Output: "No managed jobs."}, nil
	}
	var out strings.Builder
	for i, job := range all {
		if i > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "#%d  %s  %s  pid=%d", job.ID, job.Status, job.Command, job.RunnerPID)
	}
	return &tool.Result{Title: "Managed jobs", Output: out.String()}, nil
}

type JobOutputTool struct{ Jobs *jobs.Manager }

type jobOutputArgs struct {
	ID       int `json:"id"`
	MaxBytes int `json:"maxBytes,omitempty"`
}

func (*JobOutputTool) ID() string { return "job_output" }
func (*JobOutputTool) Description() string {
	return "Read recent output from a Rurushu-managed background job."
}
func (*JobOutputTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":       map[string]any{"type": "integer", "minimum": 1},
			"maxBytes": map[string]any{"type": "integer", "minimum": 1, "maximum": 262144},
		},
		"required": []string{"id"}, "additionalProperties": false,
	}
}
func (t *JobOutputTool) Execute(_ context.Context, raw json.RawMessage, _ *tool.ExecutionContext) (*tool.Result, error) {
	var args jobOutputArgs
	if err := json.Unmarshal(raw, &args); err != nil || args.ID <= 0 {
		return toolError("Job output error", "valid job id is required"), nil
	}
	output, err := t.Jobs.Tail(args.ID, args.MaxBytes)
	if err != nil {
		return toolError("Job output error", "%v", err), nil
	}
	if output == "" {
		output = "(no output yet)"
	}
	return &tool.Result{Title: "Job #" + strconv.Itoa(args.ID), Output: output}, nil
}

type JobStopTool struct{ Jobs *jobs.Manager }

type jobStopArgs struct {
	ID int `json:"id"`
}

func (*JobStopTool) ID() string          { return "job_stop" }
func (*JobStopTool) Description() string { return "Stop a Rurushu-managed background job." }
func (*JobStopTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "integer", "minimum": 1}}, "required": []string{"id"}, "additionalProperties": false}
}
func (t *JobStopTool) Execute(_ context.Context, raw json.RawMessage, execCtx *tool.ExecutionContext) (*tool.Result, error) {
	var args jobStopArgs
	if err := json.Unmarshal(raw, &args); err != nil || args.ID <= 0 {
		return toolError("Job stop error", "valid job id is required"), nil
	}
	if execCtx == nil || execCtx.AskPermission == nil {
		return toolError("Job stop denied", "permission handler is required"), nil
	}
	if !execCtx.AskPermission(permission.Request{Tool: "job_stop", Pattern: strconv.Itoa(args.ID), Description: fmt.Sprintf("Stop managed job #%d", args.ID)}) {
		return toolError("Job stop denied", "permission denied"), nil
	}
	job, err := t.Jobs.Stop(args.ID)
	if err != nil {
		return toolError("Job stop error", "%v", err), nil
	}
	return &tool.Result{Title: fmt.Sprintf("Stopped job #%d", args.ID), Output: fmt.Sprintf("Stopped managed job #%d: %s", args.ID, job.Command)}, nil
}
