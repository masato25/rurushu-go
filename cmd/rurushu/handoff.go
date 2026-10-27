package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/masato25/rurushu-go/projectstate"
	"github.com/masato25/rurushu-go/provider"
)

// runHandoff works for ordinary single-agent sessions as well as role
// sessions. COS owns the transaction; Rurushu only requests it.
func runHandoff(args []string) error {
	fs := flag.NewFlagSet("rurushu handoff", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd, _ := os.Getwd()
	root := fs.String("cwd", cwd, "Rurushu project directory")
	sessionID := fs.String("session", "last", "local Rurushu session id or last")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	store, err := projectstate.Open(abs)
	if err != nil {
		return err
	}
	var session projectstate.Session
	if strings.TrimSpace(*sessionID) == "last" {
		session, err = store.LastSession()
	} else {
		session, err = store.LoadSession(strings.TrimSpace(*sessionID))
	}
	if err != nil {
		return err
	}
	if session.Provider != "cos" {
		return fmt.Errorf("session %s uses provider %q, not COS", session.ID, session.Provider)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := provider.NewCOSProvider(abs).CompactResume(ctx, session.ID); err != nil {
		return err
	}
	fmt.Printf("COS handoff requested for %s; this is not completion proof. Continue with --resume %s after COS finishes Compact & Resume.\n", session.ID, session.ID)
	return nil
}
