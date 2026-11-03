package tui

import (
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
)

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

func markdownColor(value string) *string { return &value }
func markdownSpacing(value uint) *uint   { return &value }

func rurushuMarkdownStyle() ansi.StyleConfig {
	cfg := styles.DarkStyleConfig
	cfg.Document.Margin = markdownSpacing(0)
	cfg.Document.BlockPrefix = ""
	cfg.Document.BlockSuffix = ""
	cfg.Document.Color = markdownColor("#D5D9E0")
	cfg.Heading.Color = markdownColor("#E6B450")
	cfg.H1.Prefix, cfg.H1.Suffix = "", ""
	cfg.H1.BackgroundColor = nil
	cfg.H1.Color = markdownColor("#E6B450")
	cfg.H2.Prefix, cfg.H3.Prefix = "", ""
	cfg.Strong.Color = markdownColor("#E5E9F0")
	cfg.Code.Color = markdownColor("#A7D7C5")
	cfg.Code.BackgroundColor = markdownColor("#27313A")
	cfg.CodeBlock.Margin = markdownSpacing(0)
	cfg.CodeBlock.Color = markdownColor("#C5D1DC")
	cfg.Item.Color = markdownColor("#7DAEA3")
	cfg.Link.Color = markdownColor("#7DAEA3")
	cfg.LinkText.Color = markdownColor("#A7D7C5")
	return cfg
}
