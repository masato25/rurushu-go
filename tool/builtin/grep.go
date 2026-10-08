package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/arborlogic/rurushu-go/tool"
	"github.com/bmatcuk/doublestar/v4"
)

const (
	defaultGrepLimit = 50
	maxGrepLimit     = 100
)

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty"`
	Include         string `json:"include,omitempty"`
	CaseInsensitive bool   `json:"caseInsensitive,omitempty"`
	IsRegex         bool   `json:"isRegex,omitempty"`
	MaxResults      int    `json:"maxResults,omitempty"`
}

type GrepTool struct{}

func (*GrepTool) ID() string { return "grep" }

func (*GrepTool) Description() string {
	return "Search text or regular expressions in files inside the working directory. Skips binary files and common dependency/build directories."
}

func (*GrepTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern":         map[string]any{"type": "string", "description": "Text or regular expression to search for."},
			"path":            map[string]any{"type": "string", "description": "File or directory inside the working directory (default .)."},
			"include":         map[string]any{"type": "string", "description": "Optional glob filter such as *.go or **/*.ts."},
			"caseInsensitive": map[string]any{"type": "boolean", "description": "Use case-insensitive matching."},
			"isRegex":         map[string]any{"type": "boolean", "description": "Treat pattern as a regular expression."},
			"maxResults":      map[string]any{"type": "integer", "description": "Maximum matching lines to return (default 50, max 100)."},
		},
		"required": []string{"pattern"},
	}
}

func (*GrepTool) Execute(ctx context.Context, raw json.RawMessage, execCtx *tool.ExecutionContext) (*tool.Result, error) {
	var args grepArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolError("Grep error", "invalid arguments: %v", err), nil
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return toolError("Grep error", "pattern is required"), nil
	}
	if args.Include != "" && patternEscapesRoot(args.Include) {
		return toolError("Grep error", "include pattern must stay inside the working directory"), nil
	}
	root := ""
	if execCtx != nil {
		root = execCtx.CWD
	}
	rootReal, err := canonicalRoot(root)
	if err != nil {
		return toolError("Grep error", "%v", err), nil
	}
	targetArg := strings.TrimSpace(args.Path)
	if targetArg == "" {
		targetArg = "."
	}
	target, err := resolveWithinRoot(rootReal, targetArg)
	if err != nil {
		return toolError("Grep error", "%v", err), nil
	}
	re, err := compileSearch(args)
	if err != nil {
		return toolError("Grep error", "invalid regex: %v", err), nil
	}
	limit := args.MaxResults
	if limit <= 0 {
		limit = defaultGrepLimit
	} else if limit > maxGrepLimit {
		limit = maxGrepLimit
	}
	results := make([]string, 0, limit)
	hasMore := false

	searchFile := func(path string) error {
		if len(results) >= limit {
			hasMore = true
			return fs.SkipAll
		}
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		if args.Include != "" {
			rel := filepath.ToSlash(relativeDisplay(rootReal, path))
			include := filepath.ToSlash(args.Include)
			matched, matchErr := doublestar.Match(include, rel)
			if matchErr != nil {
				return matchErr
			}
			if !matched {
				matched, matchErr = doublestar.Match(include, filepath.Base(path))
				if matchErr != nil || !matched {
					return matchErr
				}
			}
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		defer f.Close()
		if binaryFile(f) {
			return nil
		}
		_, _ = f.Seek(0, io.SeekStart)
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 512*1024)
		lineNo := 0
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			lineNo++
			line := scanner.Text()
			if !re.MatchString(line) {
				continue
			}
			if len(line) > 300 {
				line = line[:300] + "…"
			}
			results = append(results, fmt.Sprintf("%s:%d: %s", relativeDisplay(rootReal, path), lineNo, line))
			if len(results) >= limit {
				hasMore = true
				return fs.SkipAll
			}
		}
		return scanner.Err()
	}

	info, err := os.Stat(target)
	if err != nil {
		return toolError("Grep error", "%v", err), nil
	}
	if !info.IsDir() {
		err = searchFile(target)
	} else {
		err = filepath.WalkDir(target, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			if d.IsDir() {
				if path != target && ignoredDirectory(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			if ignoredDiscoveryFile(d.Name()) {
				return nil
			}
			real, evalErr := filepath.EvalSymlinks(path)
			if evalErr != nil || !isWithin(rootReal, real) {
				return nil
			}
			return searchFile(real)
		})
	}
	if err != nil && err != fs.SkipAll {
		return toolError("Grep error", "%v", err), nil
	}
	if len(results) == 0 {
		return &tool.Result{Title: "Grep: " + args.Pattern, Output: "(no matches found)"}, nil
	}
	output := strings.Join(results, "\n")
	if hasMore {
		output += fmt.Sprintf("\n\n[Showing first %d matches]", limit)
	}
	return &tool.Result{Title: fmt.Sprintf("Grep: %s (%d matches)", args.Pattern, len(results)), Output: output}, nil
}

func compileSearch(args grepArgs) (*regexp.Regexp, error) {
	pattern := args.Pattern
	if !args.IsRegex {
		pattern = regexp.QuoteMeta(pattern)
	}
	if args.CaseInsensitive {
		pattern = "(?i)" + pattern
	}
	return regexp.Compile(pattern)
}

func binaryFile(r io.Reader) bool {
	buf := make([]byte, 512)
	n, err := r.Read(buf)
	return (err == nil || err == io.EOF) && bytes.IndexByte(buf[:n], 0) >= 0
}
