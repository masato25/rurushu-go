package tui

type rect struct {
	X      int
	Y      int
	Width  int
	Height int
}

type frameLayout struct {
	Root              rect
	Workspace         rect
	Status            rect
	Transcript        rect
	Composer          rect
	GapHeight         int
	ShowStatus        bool
	MaxComposerHeight int
}

func computeFrameLayout(width, height, composerHeight int) frameLayout {
	width = max(1, width)
	height = max(1, height)

	gutter := 3
	if width < 80 {
		gutter = 2
	}
	if width < 48 {
		gutter = 1
	}
	if width < 24 {
		gutter = 0
	}

	// Rurushu is a terminal workbench, not a centered reading surface. Keep a
	// small stable left gutter and let the workspace consume most of the
	// terminal width. Very wide terminals retain some natural space on the
	// right rather than pushing the entire workbench toward the middle.
	workspaceWidth := width - gutter
	if workspaceWidth > 120 {
		workspaceWidth = 120
	}
	workspaceWidth = max(1, workspaceWidth)
	workspaceX := min(gutter, max(0, width-workspaceWidth))

	statusHeight := 0
	if height >= 5 {
		statusHeight = 1
	}
	gapHeight := 0
	if height >= 10 {
		gapHeight = 1
	}

	// Keep at least one transcript row whenever the terminal has enough room.
	reservedTranscript := 0
	if height-statusHeight-gapHeight >= 2 {
		reservedTranscript = 1
	}
	maxComposerHeight := height - statusHeight - gapHeight - reservedTranscript
	maxComposerHeight = min(4, max(1, maxComposerHeight))
	composerHeight = min(maxComposerHeight, max(1, composerHeight))

	transcriptHeight := height - statusHeight - gapHeight - composerHeight
	transcriptHeight = max(0, transcriptHeight)
	transcriptY := statusHeight + gapHeight
	composerY := transcriptY + transcriptHeight

	return frameLayout{
		Root: rect{Width: width, Height: height},
		Workspace: rect{
			X:      workspaceX,
			Y:      0,
			Width:  workspaceWidth,
			Height: height,
		},
		Status: rect{
			X:      workspaceX,
			Y:      0,
			Width:  workspaceWidth,
			Height: statusHeight,
		},
		Transcript: rect{
			X:      workspaceX,
			Y:      transcriptY,
			Width:  workspaceWidth,
			Height: transcriptHeight,
		},
		Composer: rect{
			X:      workspaceX,
			Y:      composerY,
			Width:  workspaceWidth,
			Height: composerHeight,
		},
		GapHeight:         gapHeight,
		ShowStatus:        statusHeight > 0,
		MaxComposerHeight: maxComposerHeight,
	}
}
