package tui

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

const (
	workingStatusText        = "Working"
	workingDriveGridWidth    = 3
	workingDriveGridHeight   = 3
	workingDriveStep         = 90 * time.Millisecond
	workingDriveCycle        = 1200 * time.Millisecond
	workingDriveBrightWindow = 80 * time.Millisecond
	workingDriveSoftWindow   = 200 * time.Millisecond
	workingDriveDimWindow    = 430 * time.Millisecond
)

var workingDriveDelays = [...]time.Duration{
	workingDriveStep, 2 * workingDriveStep, 3 * workingDriveStep,
	0, workingDriveStep, 2 * workingDriveStep,
	workingDriveStep, 2 * workingDriveStep, 3 * workingDriveStep,
}

type workingDriveLevel uint8

const (
	workingDriveIdle workingDriveLevel = iota
	workingDriveDim
	workingDriveSoft
	workingDriveBright
)

// workingStatusIndicator renders a compact moving 3×3 chevron. Braille keeps
// the grid in one transcript row while preserving its actual changing shape.
func (m model) workingStatusIndicator() string {
	if m.reducedMotion {
		return ""
	}

	levels := workingDriveLevels(m.spinnerPhase)
	var out strings.Builder
	out.Grow(32)
	for group := 0; group < 2; group++ {
		bits := 0
		level := workingDriveIdle
		for row := 0; row < workingDriveGridHeight; row++ {
			for localColumn := 0; localColumn < 2; localColumn++ {
				column := group*2 + localColumn
				if column >= workingDriveGridWidth {
					continue
				}
				cell := levels[row*workingDriveGridWidth+column]
				if cell == workingDriveIdle {
					continue
				}
				bits |= workingDriveBrailleBit(localColumn, row)
				if cell > level {
					level = cell
				}
			}
		}
		out.WriteString(workingDriveStyle(level).Render(string(rune(0x2800 + bits))))
	}
	return out.String()
}

// workingStatusLabel continues the same wave after the grid. The first letter
// starts immediately after the final grid column, making one animation travel
// cleanly from the loader into the active label.
func (m model) workingStatusLabel() string {
	if m.reducedMotion {
		return zeroTheme.ink.Render(workingStatusText)
	}

	var out strings.Builder
	out.Grow(len(workingStatusText) * 12)
	start := 4 * workingDriveStep
	for index, char := range []rune(workingStatusText) {
		delay := start + time.Duration(index)*workingDriveStep
		out.WriteString(workingDriveStyle(workingDriveLevelAt(m.spinnerPhase, delay)).Render(string(char)))
	}
	return out.String()
}

func workingDriveLevels(phase int) [workingDriveGridWidth * workingDriveGridHeight]workingDriveLevel {
	var levels [workingDriveGridWidth * workingDriveGridHeight]workingDriveLevel
	for index, delay := range workingDriveDelays {
		levels[index] = workingDriveLevelAt(phase, delay)
	}
	return levels
}

func workingDriveBrailleBit(column, row int) int {
	if column == 0 {
		switch row {
		case 0:
			return 1
		case 1:
			return 2
		default:
			return 4
		}
	}
	switch row {
	case 0:
		return 8
	case 1:
		return 16
	default:
		return 32
	}
}

// workingDriveLevelAt computes one shared pulse track. A new grid front starts
// while the final label character still fades, so there is no dead blink between
// loops. The same timing function drives both the 3×3 grid and Working.
func workingDriveLevelAt(phase int, delay time.Duration) workingDriveLevel {
	if phase < 0 {
		phase = 0
	}
	age := time.Duration(phase)*activeAnimationFrameInterval - delay
	age %= workingDriveCycle
	if age < 0 {
		age += workingDriveCycle
	}
	switch {
	case age < workingDriveBrightWindow:
		return workingDriveBright
	case age < workingDriveSoftWindow:
		return workingDriveSoft
	case age < workingDriveDimWindow:
		return workingDriveDim
	default:
		return workingDriveIdle
	}
}

// workingDriveStyle gives the grid and label exactly the same theme-aware
// intensity ramp. The result is legible in ANSI, 256-color, and true-color
// terminals without painting a background behind either surface.
func workingDriveStyle(level workingDriveLevel) lipgloss.Style {
	switch level {
	case workingDriveBright:
		return zeroTheme.accent
	case workingDriveSoft:
		return zeroTheme.ink
	case workingDriveDim:
		return zeroTheme.muted
	default:
		return zeroTheme.faint
	}
}
