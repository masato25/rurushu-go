package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/masato25/rurushu-go/tool"
)

type watchTool struct {
	stateDir string
	mode     string
}

type watchToolArgs struct {
	Interval string `json:"interval"`
	Prompt   string `json:"prompt"`
}

type watchToolState struct {
	Version   int       `json:"version"`
	Active    bool      `json:"active"`
	Interval  string    `json:"interval"`
	Prompt    string    `json:"prompt"`
	NextRunAt time.Time `json:"next_run_at"`
	LastRunAt time.Time `json:"last_run_at,omitempty"`
}

var watchSessionID = regexp.MustCompile(`^ses_[a-zA-Z0-9_-]+$`)

func (w *watchTool) ID() string { return "watch_" + w.mode }

func (w *watchTool) Description() string {
	switch w.mode {
	case "create":
		return "Create or replace a recurring single-chat task. Use this when the user asks in natural language to schedule an inspection or maintenance task every N minutes. The task runs in the same session while the Rurushu TUI is open. Do not claim creation until this tool succeeds."
	case "stop":
		return "Stop the active recurring watch for the current single-chat session."
	default:
		return "Get the current single-chat recurring watch interval, instruction, next run and active status."
	}
}

func (w *watchTool) Parameters() map[string]any {
	properties := map[string]any{}
	required := []string{}
	if w.mode == "create" {
		properties = map[string]any{
			"interval": map[string]any{"type": "string", "description": "Go duration, e.g. 10m or 1h"},
			"prompt":   map[string]any{"type": "string", "description": "Complete instruction to execute at every interval"},
		}
		required = []string{"interval", "prompt"}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func (w *watchTool) Execute(_ context.Context, raw json.RawMessage, execCtx *tool.ExecutionContext) (*tool.Result, error) {
	if execCtx == nil || !watchSessionID.MatchString(execCtx.Metadata["session_id"]) {
		return &tool.Result{Title: "Watch unavailable", Output: "An active local project session is required", IsError: true}, nil
	}
	path := filepath.Join(w.stateDir, "watch-"+execCtx.Metadata["session_id"]+".json")
	var state watchToolState
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("decode watch state: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if w.mode == "status" {
		data, _ := json.Marshal(state)
		return &tool.Result{Title: "Watch status", Output: string(data)}, nil
	}
	if w.mode == "stop" {
		state.Active = false
	} else {
		var args watchToolArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
		interval, err := time.ParseDuration(strings.TrimSpace(args.Interval))
		if err != nil || interval < time.Second || interval > 24*time.Hour {
			return &tool.Result{Title: "Invalid interval", Output: "interval must be from 1s to 24h, for example 10m", IsError: true}, nil
		}
		if strings.TrimSpace(args.Prompt) == "" || len(args.Prompt) > 12000 {
			return &tool.Result{Title: "Invalid prompt", Output: "prompt must contain 1–12000 characters", IsError: true}, nil
		}
		state = watchToolState{Version: 1, Active: true, Interval: interval.String(), Prompt: strings.TrimSpace(args.Prompt), NextRunAt: time.Now().Add(interval)}
	}
	state.Version = 1
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(w.stateDir, ".watch-tool-*.tmp")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	return &tool.Result{Title: "Watch " + w.mode, Output: fmt.Sprintf("watch %s succeeded for session %s; active=%t; interval=%s; next_run_at=%s. The Rurushu TUI must remain running.", w.mode, execCtx.Metadata["session_id"], state.Active, state.Interval, state.NextRunAt.Format(time.RFC3339))}, nil
}
