package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const initialCOSConfig = "{\n  \"project\": \"\",\n  \"projectUrl\": \"\"\n}\n"

// Init only creates a local configuration template. The user supplies the
// ChatGPT Project URL manually before using the COS provider.
func runInit(args []string) error {
	fs := flag.NewFlagSet("rurushu --init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	dir := fs.String("cwd", cwd, "project directory where .rurushugo/cos.json will be created")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", root)
	}
	configDir := filepath.Join(root, ".rurushugo")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	path := filepath.Join(configDir, "cos.json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		fmt.Printf("Configuration already exists; unchanged: %s\n", path)
		return nil
	}
	if err != nil {
		return fmt.Errorf("create COS config: %w", err)
	}
	if _, err := f.WriteString(initialCOSConfig); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("Created %s\nEdit project and projectUrl before using --provider cos.\n", path)
	return nil
}
