package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/arborlogic/rurushu-go/tool"
	"github.com/bmatcuk/doublestar/v4"
)

const (
	defaultGlobLimit = 100
	maxGlobLimit     = 200
)

var ignoredDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	".next": true, "_next": true, ".turbo": true, ".cache": true, "coverage": true, "out": true,
	".claude": true, ".mimocode": true, ".openzerocode": true,
	".venv": true, "venv": true, ".tox": true, ".pytest_cache": true,
}

type globArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	MaxResults int    `json:"maxResults,omitempty"`
}

type GlobTool struct{}

func (*GlobTool) ID() string { return "glob" }

func (*GlobTool) Description() string {
	return "Find files inside the working directory using glob patterns such as **/*.go. Skips common dependency/build directories."
}

func (*GlobTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":    map[string]any{"type": "string", "description": "Glob pattern relative to the search path, e.g. **/*.go."},
			"path":       map[string]any{"type": "string", "description": "Base directory inside the working directory (default .)."},
			"maxResults": map[string]any{"type": "integer", "description": "Maximum file paths to return (default 100, max 200)."},
		},
		"required": []string{"pattern"},
	}
}

func (*GlobTool) Execute(ctx context.Context, raw json.RawMessage, execCtx *tool.ExecutionContext) (*tool.Result, error) {
	var args globArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolError("Glob error", "invalid arguments: %v", err), nil
	}
	pattern := filepath.ToSlash(strings.TrimSpace(args.Pattern))
	if patternEscapesRoot(pattern) {
		return toolError("Glob error", "pattern is required and must stay inside the working directory"), nil
	}
	root := ""
	if execCtx != nil {
		root = execCtx.CWD
	}
	rootReal, err := canonicalRoot(root)
	if err != nil {
		return toolError("Glob error", "%v", err), nil
	}
	baseArg := strings.TrimSpace(args.Path)
	if baseArg == "" {
		baseArg = "."
	}
	base, err := resolveWithinRoot(rootReal, baseArg)
	if err != nil {
		return toolError("Glob error", "%v", err), nil
	}
	info, err := os.Stat(base)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return toolError("Glob error", "%v", err), nil
	}
	if _, err := doublestar.Match(pattern, "probe"); err != nil {
		return toolError("Glob error", "invalid pattern: %v", err), nil
	}
	limit := args.MaxResults
	if limit <= 0 {
		limit = defaultGlobLimit
	} else if limit > maxGlobLimit {
		limit = maxGlobLimit
	}

	var matches []string
	hasMore := false
	err = filepath.WalkDir(base, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if fullPath == base {
			return nil
		}

		rel, relErr := filepath.Rel(base, fullPath)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if ignoredDirectory(d.Name()) {
				return fs.SkipDir
			}
			matched, matchErr := doublestar.Match(pattern, rel)
			if matchErr != nil {
				return matchErr
			}
			if !matched {
				return nil
			}
			if len(matches) >= limit {
				hasMore = true
				return fs.SkipAll
			}
			matches = append(matches, rel+"/")
			return nil
		}
		for _, part := range strings.Split(rel, "/") {
			if ignoredDirectory(part) {
				return nil
			}
		}
		if ignoredDiscoveryFile(d.Name()) {
			return nil
		}
		matched, matchErr := doublestar.Match(pattern, rel)
		if matchErr != nil {
			return matchErr
		}
		if !matched {
			return nil
		}
		real, evalErr := filepath.EvalSymlinks(fullPath)
		if evalErr != nil || !isWithin(rootReal, real) {
			return nil
		}
		fileInfo, statErr := os.Stat(real)
		if statErr != nil || !fileInfo.Mode().IsRegular() {
			return nil
		}
		if len(matches) >= limit {
			hasMore = true
			return fs.SkipAll
		}
		matches = append(matches, relativeDisplay(rootReal, real))
		return nil
	})
	if err != nil && err != fs.SkipAll {
		return toolError("Glob error", "%v", err), nil
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return &tool.Result{Title: "Glob: " + args.Pattern, Output: "(no matching files found)"}, nil
	}
	output := strings.Join(matches, "\n")
	if hasMore {
		output += fmt.Sprintf("\n\n[Showing first %d matches]", limit)
	}
	return &tool.Result{Title: fmt.Sprintf("Glob: %s (%d files)", args.Pattern, len(matches)), Output: output}, nil
}
