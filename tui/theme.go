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

	markdownHeadingStyle = lipgloss.NewStyle().Bold(true).
				Foreground(lipgloss.Color("#E6B450"))
	markdownStrongStyle = lipgloss.NewStyle().Bold(true).
				Foreground(lipgloss.Color("#E5E9F0"))
	markdownCodeStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#A7D7C5")).
				Background(lipgloss.Color("#27313A"))
	markdownBlockStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#C5D1DC"))
	markdownBulletStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#7DAEA3"))
)
