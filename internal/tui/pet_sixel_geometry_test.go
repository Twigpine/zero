package tui

import (
	"testing"

	"github.com/Gitlawb/zero/internal/terminalpet"
)

// The erase rectangle has to describe what was PAINTED, not the reserved area.
//
// Measured on Windows Terminal 1.24, which answers CSI 16 t with a 20x10 cell.
// A companion is rendered at preferredHeight (75px), so it covers
// ceil(75/20) = 4 rows, while petImageRows is 5. The erase used the constant and
// blanked a fifth row of live interface every time the companion moved, which is
// the broken border and the eaten characters reported on Windows.
//
// Sixel is the only protocol that erases by writing over cells, so it is the
// only one that needs this; the Kitty case below pins that it is left alone.
func TestSixelEraseFootprintMatchesWhatWasPainted(t *testing.T) {
	m := model{
		petRenderer:        terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolSixel}),
		petCellPixelWidth:  10,
		petCellPixelHeight: 20,
	}
	columns, rows := m.petImageCells(nil, terminalpet.Idle, 0, 75)
	if rows != 4 {
		t.Errorf("rows = %d, want 4 for a 75px sprite in a 20px cell; erasing %d would blank live interface", rows, petImageRows)
	}
	if rows > petImageRows || columns > petImageColumns {
		t.Errorf("footprint %dx%d exceeds the reserved %dx%d area", columns, rows, petImageColumns, petImageRows)
	}
}

// Kitty passes Columns/Rows to the terminal as the placement REQUEST and the
// terminal owns the region, so the constants are correct there and must not be
// recomputed.
func TestKittyKeepsTheRequestedPlacementSize(t *testing.T) {
	m := model{
		petRenderer:        terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty}),
		petCellPixelWidth:  10,
		petCellPixelHeight: 20,
	}
	columns, rows := m.petImageCells(nil, terminalpet.Idle, 0, 75)
	if columns != petImageColumns || rows != petImageRows {
		t.Errorf("Kitty placement = %dx%d, want the requested %dx%d", columns, rows, petImageColumns, petImageRows)
	}
}

// Without metrics there is nothing to compute from, so it must fall back to the
// constants rather than to zero, which would erase nothing and leave the
// companion smeared across the screen.
func TestFootprintFallsBackToTheReservedAreaWithoutMetrics(t *testing.T) {
	m := model{petRenderer: terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolSixel})}
	columns, rows := m.petImageCells(nil, terminalpet.Idle, 0, 75)
	if columns != petImageColumns || rows != petImageRows {
		t.Errorf("fallback = %dx%d, want the reserved %dx%d", columns, rows, petImageColumns, petImageRows)
	}
}

// The request has to be sent for a sixel terminal, or the metrics never arrive
// and every computation above silently uses the fallback. This is the actual
// defect: the reply was never asked for, not unsupported.
func TestCellMetricsAreRequestedForEveryImageProtocol(t *testing.T) {
	for _, protocol := range []terminalpet.ImageProtocol{
		terminalpet.ImageProtocolSixel,
		terminalpet.ImageProtocolKitty,
		terminalpet.ImageProtocolKittyLocalFile,
	} {
		m := model{petRenderer: terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: protocol})}
		if !m.petCellMetricsWanted() {
			t.Errorf("protocol %v does not ask the terminal for its cell size, so the reply never arrives", protocol)
		}
	}
}
