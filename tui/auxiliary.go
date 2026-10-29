package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	rurushuconfig "github.com/masato25/rurushu-go/config"
	"github.com/trentkm/spanreed"
)

// AuxiliaryWorkspace exposes the same F2-F5 utility panels used by the main
// chat TUI so alternate frontends (such as duel mode) can reuse them.
type AuxiliaryWorkspace struct {
	cwd             string
	width, height   int
	setup           *SetupModel
	review          *reviewModel
	browser         *fileBrowser
	terminal        *terminalWorkspace
	terminalService *TerminalService
	errText         string
}

func NewAuxiliaryWorkspace(cwd string, service *TerminalService) *AuxiliaryWorkspace {
	if service == nil {
		service = NewTerminalService(cwd)
	}
	return &AuxiliaryWorkspace{cwd: cwd, terminalService: service}
}

func (a *AuxiliaryWorkspace) Active() bool {
	return a != nil && (a.setup != nil || a.review != nil || a.browser != nil || a.terminal != nil)
}

func (a *AuxiliaryWorkspace) Update(msg tea.Msg) (bool, tea.Cmd) {
	if a == nil {
		return false, nil
	}
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = v.Width, v.Height
		if a.setup != nil {
			a.setup.Update(v)
		}
		if a.terminal != nil {
			return true, a.terminal.resize(a.width, a.height)
		}
		return a.Active(), nil
	case spanreed.FrameMsg:
		if a.terminal != nil {
			return true, a.terminal.update(v)
		}
	case reviewFilesMsg:
		if a.review == nil || v.generation != a.review.generation {
			return a.Active(), nil
		}
		a.review.loading = false
		if v.err != nil {
			a.review.err = v.err.Error()
			return true, nil
		}
		a.review.files = v.files
		a.review.commits = v.commits
		a.review.commitError = v.commitError
		a.review.selected = min(a.review.selected, max(0, len(v.files)-1))
		return true, a.review.loadDiff()
	case reviewDiffMsg:
		if a.review == nil || v.generation != a.review.generation {
			return a.Active(), nil
		}
		a.review.loading = false
		if v.err != nil {
			a.review.err = v.err.Error()
			return true, nil
		}
		a.review.diff = nil
		if v.diff != "" {
			a.review.diff = strings.Split(strings.TrimSuffix(v.diff, "\n"), "\n")
		}
		return true, nil
	case tea.MouseWheelMsg:
		if a.browser != nil {
			a.browser.wheel(v.Button)
			return true, nil
		}
		if a.terminal != nil {
			return true, a.terminal.update(v)
		}
		if a.review != nil {
			return true, a.review.wheel(v.Button)
		}
		if a.setup != nil {
			_, cmd := a.setup.Update(v)
			return true, cmd
		}
	case tea.KeyPressMsg:
		key := v.String()
		if a.browser != nil {
			if key == "f5" || (key == "esc" && a.browser.focusFiles) {
				a.browser = nil
				return true, nil
			}
			a.browser.key(key)
			return true, nil
		}
		if a.terminal != nil {
			if key == "f4" || (!a.terminal.focused && key == "esc") {
				a.terminal.terminal.Blur()
				a.terminal.focused = false
				a.terminal = nil
				return true, nil
			}
			return true, a.terminal.update(v)
		}
		if a.review != nil {
			if key == "f3" || key == "esc" {
				a.review = nil
				return true, nil
			}
			return true, a.review.key(key)
		}
		if a.setup != nil {
			if key == "f2" || key == "esc" {
				a.setup = nil
				return true, nil
			}
			if a.setup.saved && key == "enter" {
				a.setup = nil
				return true, nil
			}
			_, cmd := a.setup.Update(v)
			return true, cmd
		}

		switch key {
		case "f2":
			cfg, err := rurushuconfig.Load()
			if err != nil {
				a.errText = err.Error()
				return true, nil
			}
			a.setup = NewSetupModel(cfg)
			a.setup.embedded = true
			a.setup.Update(tea.WindowSizeMsg{Width: a.width, Height: a.height})
			return true, a.setup.Init()
		case "f3":
			a.review = newReviewModel()
			if a.cwd != "" {
				a.review.cwd = a.cwd
			}
			return true, a.review.refresh()
		case "f4":
			terminal, cmd, err := a.terminalService.attach(a.width, a.height)
			if err != nil {
				a.errText = err.Error()
				return true, nil
			}
			a.terminal = terminal
			resize := a.terminal.resize(a.width, a.height)
			return true, tea.Batch(cmd, resize)
		case "f5":
			a.browser = newFileBrowser(a.cwd)
			return true, nil
		}
	}
	return false, nil
}

func (a *AuxiliaryWorkspace) View() (tea.View, bool) {
	if a == nil {
		return tea.View{}, false
	}
	if a.browser != nil {
		return a.browser.view(a.width, a.height), true
	}
	if a.terminal != nil {
		return a.terminal.view(a.width, a.height), true
	}
	if a.review != nil {
		return a.review.view(a.width, a.height), true
	}
	if a.setup != nil {
		return a.setup.View(), true
	}
	return tea.View{}, false
}
