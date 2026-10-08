package builtin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func canonicalRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", fmt.Errorf("working directory is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("stat working directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory is not a directory")
	}
	return real, nil
}

func resolveWithinRoot(root, target string) (string, error) {
	rootReal, err := canonicalRoot(root)
	if err != nil {
		return "", err
	}
	target = strings.TrimSpace(target)
	if target == "" {
		target = "."
	}
	if strings.IndexByte(target, 0) >= 0 {
		return "", fmt.Errorf("path contains NUL byte")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(rootReal, target)
	}
	abs, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if !isWithin(rootReal, abs) {
		return "", fmt.Errorf("path %q is outside working directory", target)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if !isWithin(rootReal, real) {
		return "", fmt.Errorf("path %q resolves outside working directory", target)
	}
	return real, nil
}

func isWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func relativeDisplay(root, target string) string {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." {
		return filepath.Base(target)
	}
	return filepath.ToSlash(rel)
}

func patternEscapesRoot(pattern string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" || strings.IndexByte(pattern, 0) >= 0 || strings.HasPrefix(pattern, "/") {
		return true
	}
	for _, part := range strings.Split(pattern, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}
