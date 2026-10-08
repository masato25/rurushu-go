package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/masato25/rurushu-go/permission"
)

const maxExternalToolOutput = 64 * 1024

// ExternalSpec describes one orchestrator-provided subprocess tool. The
// orchestrator owns the command and schema; Rurushu only exposes the bounded
// function call to the model and forwards JSON arguments on stdin.
type ExternalSpec struct {
	ID            string            `json:"id"`
	Description   string            `json:"description"`
	Command       string            `json:"command"`
	Args          []string          `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	InputSchema   map[string]any    `json:"input_schema"`
	ReadOnly      bool              `json:"read_only,omitempty"`
	Preauthorized bool              `json:"preauthorized,omitempty"`
}

type External struct {
	spec ExternalSpec
}

func NewExternal(spec ExternalSpec) (*External, error) {
	spec.ID = strings.TrimSpace(spec.ID)
	spec.Description = strings.TrimSpace(spec.Description)
	spec.Command = strings.TrimSpace(spec.Command)
	if spec.ID == "" {
		return nil, fmt.Errorf("external tool id is required")
	}
	if spec.Description == "" {
		return nil, fmt.Errorf("external tool %q description is required", spec.ID)
	}
	if spec.Command == "" {
		return nil, fmt.Errorf("external tool %q command is required", spec.ID)
	}
	if spec.InputSchema == nil {
		spec.InputSchema = map[string]any{"type": "object"}
	}
	return &External{spec: spec}, nil
}

func (t *External) ID() string { return t.spec.ID }

func (t *External) Description() string { return t.spec.Description }

func (t *External) Parameters() map[string]any { return t.spec.InputSchema }

func (t *External) Execute(ctx context.Context, args json.RawMessage, execCtx *ExecutionContext) (*Result, error) {
	if !t.spec.ReadOnly && !t.spec.Preauthorized && execCtx != nil && execCtx.AskPermission != nil {
		if !execCtx.AskPermission(permission.Request{
			Tool: t.spec.ID, Pattern: t.spec.Command, Description: "Run external tool: " + t.spec.ID,
		}) {
			return &Result{Title: t.spec.ID, Output: "permission denied", IsError: true}, nil
		}
	}

	cmd := exec.CommandContext(ctx, t.spec.Command, t.spec.Args...)
	if execCtx != nil && strings.TrimSpace(execCtx.CWD) != "" {
		cmd.Dir = execCtx.CWD
	}
	cmd.Env = os.Environ()
	for key, value := range t.spec.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdin = bytes.NewReader(args)
	var output cappedExternalBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	result := &Result{Title: t.spec.ID, Output: strings.TrimSpace(output.String())}
	if runErr != nil {
		result.IsError = true
		if result.Output == "" {
			result.Output = runErr.Error()
		}
	}
	return result, nil
}

type cappedExternalBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedExternalBuffer) Write(p []byte) (int, error) {
	if b.truncated {
		return len(p), nil
	}
	remaining := maxExternalToolOutput - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *cappedExternalBuffer) String() string {
	value := b.buf.String()
	if b.truncated {
		value += "\n[output truncated]"
	}
	return value
}
