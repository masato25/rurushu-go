package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/masato25/rurushu-go/permission"
	"github.com/masato25/rurushu-go/tool"
	"github.com/masato25/rurushu-go/tui"
)

type terminalSendTool struct{ service *tui.TerminalService }

func (*terminalSendTool) ID() string { return "terminal_send" }
func (*terminalSendTool) Description() string {
	return "Send one shell command into the single F4 interactive terminal (also works while F4 is hidden). The command is submitted asynchronously; successful submission is NOT proof of execution, completion or exit status. Requires user permission. Use for explicitly requested interactive/local programs."
}
func (*terminalSendTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []string{"command"}, "additionalProperties": false}
}
func (t *terminalSendTool) Execute(_ context.Context, raw json.RawMessage, execCtx *tool.ExecutionContext) (*tool.Result, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return &tool.Result{Title: "Terminal rejected", Output: "invalid command arguments", IsError: true}, nil
	}
	cmd := strings.TrimSpace(args.Command)
	if cmd == "" || len(cmd) > 8192 || strings.ContainsAny(cmd, "\r\n\x00\x1b") {
		return &tool.Result{Title: "Terminal rejected", Output: "command must be one line of at most 8192 bytes", IsError: true}, nil
	}
	if t.service == nil || execCtx == nil || execCtx.AskPermission == nil {
		return &tool.Result{Title: "Terminal denied", Output: "terminal or permission handler unavailable", IsError: true}, nil
	}
	if !execCtx.AskPermission(permission.Request{Tool: "terminal_send", Pattern: cmd, Description: "Send command to F4 terminal: " + cmd}) {
		return &tool.Result{Title: "Terminal denied", Output: "permission denied", IsError: true}, nil
	}
	if err := t.service.SendCommand(cmd); err != nil {
		return &tool.Result{Title: "Terminal error", Output: err.Error(), IsError: true}, nil
	}
	return &tool.Result{Title: "Terminal command submitted", Output: fmt.Sprintf("Submitted to the single F4 shell: %s\nThe command may still be running; open F4 to observe its output. Exit status is not verified.", cmd), Metadata: map[string]any{"submitted": true, "completed": false}}, nil
}
