package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/arborlogic/rurushu-go/tool"
)

const (
	defaultReadLimit = 200
	maxReadLimit     = 500
)

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type ReadTool struct{}

func (*ReadTool) ID() string { return "read" }

func (*ReadTool) Description() string {
	return "Read a text file inside the working directory with line numbers. Supports offset and limit for pagination."
}

func (*ReadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":   map[string]any{"type": "string", "description": "File path relative to the working directory, or an absolute path inside it."},
			"offset": map[string]any{"type": "integer", "description": "1-indexed starting line (default 1)."},
			"limit":  map[string]any{"type": "integer", "description": "Maximum lines to return (default 200, max 500)."},
		},
		"required": []string{"path"},
	}
}

func (*ReadTool) Execute(ctx context.Context, raw json.RawMessage, execCtx *tool.ExecutionContext) (*tool.Result, error) {
	var args readArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolError("Read error", "invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return toolError("Read error", "path is required"), nil
	}
	root := ""
	if execCtx != nil {
		root = execCtx.CWD
	}
	path, err := resolveWithinRoot(root, args.Path)
	if err != nil {
		return toolError("Read error", "%v", err), nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return toolError("Read error", "%v", err), nil
	}
	if !info.Mode().IsRegular() {
		return toolError("Read error", "%q is not a regular file", args.Path), nil
	}

	f, err := os.Open(path)
	if err != nil {
		return toolError("Read error", "%v", err), nil
	}
	defer f.Close()

	offset := args.Offset
	if offset < 1 {
		offset = 1
	}
	limit := args.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	} else if limit > maxReadLimit {
		limit = maxReadLimit
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lines := make([]string, 0, limit)
	lineNumber := 0
	hasMore := false
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		lineNumber++
		if lineNumber < offset {
			continue
		}
		if len(lines) >= limit {
			hasMore = true
			break
		}
		lines = append(lines, fmt.Sprintf("%5d│ %s", lineNumber, scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		return toolError("Read error", "%v", err), nil
	}
	if len(lines) == 0 {
		output := "(empty file)"
		if offset > 1 {
			output = fmt.Sprintf("(file has fewer than %d lines)", offset)
		}
		return &tool.Result{Title: "Read " + args.Path, Output: output}, nil
	}
	output := strings.Join(lines, "\n")
	if hasMore {
		output += fmt.Sprintf("\n\n[Showing lines %d-%d; use offset=%d to continue]", offset, offset+len(lines)-1, offset+len(lines))
	}
	return &tool.Result{Title: fmt.Sprintf("Read %s (%d lines)", args.Path, len(lines)), Output: output}, nil
}
