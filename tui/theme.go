package tui

import "charm.land/lipgloss/v2"

var (
	statusNameStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#E6B450"))

	userMarkerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#D3869B"))

	agentMarkerStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#7DAEA3"))

	idleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#D8DEE9"))

	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6B7280"))
)
