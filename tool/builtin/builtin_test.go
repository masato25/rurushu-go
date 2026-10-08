package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/arborlogic/rurushu-go/tool"
)

func TestReadGlobGrep(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "pkg", "core", "a.go"), "package core\nfunc Alpha() string { return \"magic-keyword\" }\n")
	mustWrite(t, filepath.Join(root, "pkg", "core", "b.txt"), "hello\n")
	mustWrite(t, filepath.Join(root, ".git", "hidden.go"), "magic-keyword\n")
	mustWrite(t, filepath.Join(root, ".next", "generated.go"), "magic-keyword\n")
	mustWrite(t, filepath.Join(root, ".next-dev", "generated.go"), "magic-keyword\n")
	mustWrite(t, filepath.Join(root, ".env.local"), "SECRET=do-not-discover\n")
	execCtx := &tool.ExecutionContext{CWD: root}

	readResult := execute(t, &ReadTool{}, execCtx, map[string]any{"path": "pkg/core/a.go", "offset": 2, "limit": 1})
	if readResult.IsError || !strings.Contains(readResult.Output, "magic-keyword") || !strings.Contains(readResult.Output, "2│") {
		t.Fatalf("unexpected read result: %+v", readResult)
	}

	globResult := execute(t, &GlobTool{}, execCtx, map[string]any{"pattern": "**/*.go"})
	if globResult.IsError || !strings.Contains(globResult.Output, "pkg/core/a.go") || strings.Contains(globResult.Output, ".git") || strings.Contains(globResult.Output, ".next") || strings.Contains(globResult.Output, ".env.local") {
		t.Fatalf("unexpected glob result: %+v", globResult)
	}

	grepResult := execute(t, &GrepTool{}, execCtx, map[string]any{"pattern": "magic-keyword", "include": "**/*.go"})
	if grepResult.IsError || !strings.Contains(grepResult.Output, "pkg/core/a.go:2:") || strings.Contains(grepResult.Output, ".git") || strings.Contains(grepResult.Output, ".next") || strings.Contains(grepResult.Output, ".env.local") {
		t.Fatalf("unexpected grep result: %+v", grepResult)
	}
}

func TestGlobStarOnlyListsImmediateEntries(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "README.md"), "hello\n")
	mustWrite(t, filepath.Join(root, "src", "main.go"), "package main\n")

	result := execute(t, &GlobTool{}, &tool.ExecutionContext{CWD: root}, map[string]any{"pattern": "*"})
	if result.IsError {
		t.Fatalf("glob failed: %+v", result)
	}
	if !strings.Contains(result.Output, "README.md") {
		t.Fatalf("root file missing: %q", result.Output)
	}
	if !strings.Contains(result.Output, "src/") {
		t.Fatalf("root directory missing: %q", result.Output)
	}
	if strings.Contains(result.Output, "src/main.go") {
		t.Fatalf("star pattern unexpectedly recursed: %q", result.Output)
	}
}

func TestReadOnlyToolsRejectTraversal(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside-secret.txt")
	mustWrite(t, outside, "secret\n")
	t.Cleanup(func() { _ = os.Remove(outside) })
	execCtx := &tool.ExecutionContext{CWD: root}

	readResult := execute(t, &ReadTool{}, execCtx, map[string]any{"path": "../outside-secret.txt"})
	if !readResult.IsError || !strings.Contains(readResult.Output, "outside working directory") {
		t.Fatalf("read traversal was not rejected: %+v", readResult)
	}
	globResult := execute(t, &GlobTool{}, execCtx, map[string]any{"pattern": "../*.txt"})
	if !globResult.IsError {
		t.Fatalf("glob traversal was not rejected: %+v", globResult)
	}
	grepResult := execute(t, &GrepTool{}, execCtx, map[string]any{"pattern": "secret", "path": "../outside-secret.txt"})
	if !grepResult.IsError {
		t.Fatalf("grep traversal was not rejected: %+v", grepResult)
	}
}

func TestReadOnlyToolsRejectEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside-target.txt")
	mustWrite(t, outside, "secret\n")
	t.Cleanup(func() { _ = os.Remove(outside) })
	if err := os.Symlink(outside, filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	execCtx := &tool.ExecutionContext{CWD: root}
	result := execute(t, &ReadTool{}, execCtx, map[string]any{"path": "escape.txt"})
	if !result.IsError || !strings.Contains(result.Output, "resolves outside working directory") {
		t.Fatalf("escaping symlink was not rejected: %+v", result)
	}
}

func TestReadRejectsNonRegularFile(t *testing.T) {
	root := t.TempDir()
	execCtx := &tool.ExecutionContext{CWD: root}
	result := execute(t, &ReadTool{}, execCtx, map[string]any{"path": "."})
	if !result.IsError || !strings.Contains(result.Output, "not a regular file") {
		t.Fatalf("directory read was not rejected: %+v", result)
	}
}

func TestRegisterReadOnly(t *testing.T) {
	registry := tool.NewRegistry()
	RegisterReadOnly(registry)
	for _, id := range []string{"glob", "grep", "read"} {
		if _, ok := registry.Get(id); !ok {
			t.Fatalf("tool %q not registered", id)
		}
	}
}

func execute(t *testing.T, builtin tool.Tool, execCtx *tool.ExecutionContext, args map[string]any) *tool.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := builtin.Execute(context.Background(), raw, execCtx)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyToolsRejectAbsolutePrefixSibling(t *testing.T) {
	container := t.TempDir()
	root := filepath.Join(container, "repo")
	sibling := filepath.Join(container, "repo-evil")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(sibling, "secret.txt")
	mustWrite(t, secret, "secret\n")

	result := execute(t, &ReadTool{}, &tool.ExecutionContext{CWD: root}, map[string]any{"path": secret})
	if !result.IsError || !strings.Contains(result.Output, "outside working directory") {
		t.Fatalf("absolute prefix sibling was not rejected: %+v", result)
	}
}

func TestGlobRejectsInvalidPattern(t *testing.T) {
	root := t.TempDir()
	result := execute(t, &GlobTool{}, &tool.ExecutionContext{CWD: root}, map[string]any{"pattern": "["})
	if !result.IsError || !strings.Contains(result.Output, "invalid pattern") {
		t.Fatalf("invalid glob pattern was not rejected: %+v", result)
	}
}
