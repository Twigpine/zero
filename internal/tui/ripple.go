package tui

import (
	"image/color"
	"math"
	"strings"

	"charm.land/lipgloss/v2"
)

func rippleLevel(distance, travel, waveLen int) int {
	if travel <= 0 || waveLen <= 0 {
		return 0
	}
	q := distance % waveLen
	if q < 0 {
		q += waveLen
	}
	ratio := 0.5 + 0.5*math.Cos(2*math.Pi*float64(q)/float64(waveLen))
	return int(float64(travel) * ratio)
}

// rippleText uses the existing spinner phase, so the posture colour wave adds
// no timer of its own.
func rippleText(text string, palette []lipgloss.Style, phase, waveLen int) string {
	if len(palette) == 0 || text == "" {
		return text
	}
	travel := len(palette) - 1
	if travel == 0 {
		return palette[0].Render(text)
	}
	var b strings.Builder
	for i, r := range text {
		level := rippleLevel(i+phase, travel, waveLen)
		if level < 0 {
			level = 0
		}
		if level > travel {
			level = travel
		}
		b.WriteString(palette[level].Render(string(r)))
	}
	return b.String()
}

// ripplePalette derives its ramp from the active theme; no fixed colour bypasses
// theme selection.
func ripplePalette() []lipgloss.Style {
	bright := zeroTheme.accent.GetForeground()
	dim := zeroTheme.faint.GetForeground()
	if bright == nil || dim == nil {
		return []lipgloss.Style{zeroTheme.faint, zeroTheme.muted, zeroTheme.accent}
	}
	blend := lipgloss.Blend1D(5, dim, bright)
	out := make([]lipgloss.Style, len(blend))
	for i, c := range blend {
		r, g, b, a := c.RGBA()
		out[i] = lipgloss.NewStyle().Foreground(color.RGBA{
			R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8),
		})
	}
	return out
}
