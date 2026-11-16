package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/Twigpine/zero/internal/terminalpet"
	"github.com/Twigpine/zero/internal/terminalpet/petart"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

func TestParsePetsCommandAndAlias(t *testing.T) {
	for _, input := range []string{"/pets", "/pet boba"} {
		parsed := parseCommand(input)
		if parsed.kind != commandPets {
			t.Fatalf("parseCommand(%q).kind = %v, want commandPets", input, parsed.kind)
		}
	}
}

func TestPetPickerOverlayIncludesPreview(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 100
	m.picker = &commandPicker{kind: pickerPet, title: "Choose a companion", items: []pickerItem{{Label: "Boba", Value: "boba"}}, selected: 0}
	m.petEntries["boba"] = terminalpet.Entry{Slug: "boba", DisplayName: "Boba", SubmittedBy: "tester"}
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	for y := range 12 {
		for x := range 12 {
			frame.SetNRGBA(x, y, color.NRGBA{R: 120, G: 80, B: 200, A: 255})
		}
	}
	animation, err := terminalpet.ThumbnailAnimation(frame)
	if err != nil {
		t.Fatal(err)
	}
	m.petPreview = animation
	plain := plainRender(t, m.petPickerOverlay(m.width))
	if !strings.Contains(plain, "Boba · by tester") || !strings.Contains(plain, "Enter select") {
		t.Fatalf("pet overlay lacks preview detail or controls: %q", plain)
	}
}

func TestPetPickerPasteSanitizesMultilineQuery(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.picker = &commandPicker{kind: pickerPet}

	updated, _ := m.routePaste("Luffy\nGear\t5")
	next := updated.(model)
	if got := next.picker.query; got != "LuffyGear    5" {
		t.Fatalf("picker paste query = %q, want sanitized single line", got)
	}
}

func TestPetPickerRowsShowOnlyNamesAndFooterUsesCatalogMetadata(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 100
	m.picker = &commandPicker{kind: pickerPet, title: "Choose a companion", items: []pickerItem{{Label: "Boba", Value: "boba"}}, selected: 0}
	m.petEntries["boba"] = terminalpet.Entry{Slug: "boba", DisplayName: "Boba", Kind: "creature", SubmittedBy: "tester"}

	plain := plainRender(t, m.petPickerOverlay(m.width))
	if !strings.Contains(plain, "Boba · creature · by tester") {
		t.Fatalf("pet footer should show name, kind, and creator:\n%s", plain)
	}
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "❯ Boba") && (strings.Contains(line, "creature") || strings.Contains(line, "tester")) {
			t.Fatalf("pet list row should contain only the name: %q", line)
		}
	}
}

func TestPetCatalogUsesClearLocalAndRemoteGroupLabels(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.picker = &commandPicker{kind: pickerPet}
	updated, _ := m.applyPetCatalog(petCatalogLoadedMsg{entries: []terminalpet.Entry{
		{Slug: "local", DisplayName: "Local", Local: true},
		{Slug: "remote", DisplayName: "Remote"},
	}})
	next := updated.(model)
	want := []pickerItem{
		{Label: "No companion", Value: terminalpet.DisabledID, Meta: "off"},
		{Group: "Installed", Label: "Local", Value: "local", Local: true},
		{Group: "Discover", Label: "Remote", Value: "remote", Remote: true},
	}
	if !reflect.DeepEqual(next.picker.items, want) {
		t.Fatalf("pet picker items = %#v, want %#v", next.picker.items, want)
	}
}

func TestPetsPickerShowsLocalChoicesWhileDiscoverLoads(t *testing.T) {
	root := t.TempDir()
	installedDir := filepath.Join(root, "pets", "installed", "kratos")
	if err := os.MkdirAll(installedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := `{"slug":"kratos","displayName":"Kratos Greek","kind":"character","submittedBy":"tester","spriteVersionNumber":1}`
	if err := os.WriteFile(filepath.Join(installedDir, "source.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{})
	m.width = 100
	m.petClient = terminalpet.NewClient(root)
	m.petID = "kratos"
	updated, cmd := m.handlePetsCommand("")
	next := updated.(model)
	if cmd == nil {
		t.Fatal("opening the pet picker should continue loading Discover entries")
	}
	if next.picker == nil || !next.picker.loading {
		t.Fatalf("pet picker = %#v, want local rows plus loading state", next.picker)
	}
	want := []pickerItem{{Label: "No companion", Value: terminalpet.DisabledID, Meta: "off"}}
	for _, entry := range terminalpet.BuiltinEntries() {
		want = append(want, pickerItem{Group: "Built in", Label: entry.Label(), Value: entry.Slug, Local: true})
	}
	want = append(want, pickerItem{Group: "Installed", Label: "Kratos Greek", Value: "kratos", Local: true})
	if !reflect.DeepEqual(next.picker.items, want) {
		t.Fatalf("initial pet picker items = %#v, want %#v", next.picker.items, want)
	}
	if current, ok := next.picker.current(); !ok || current.Value != "kratos" {
		t.Fatalf("initial pet picker selection = %#v, %v; want installed current pet", current, ok)
	}
	plain := plainRender(t, next.petPickerOverlay(next.width))
	for _, wantText := range []string{"Installed", "Kratos Greek", "Discover", "Fetching companions"} {
		if !strings.Contains(plain, wantText) {
			t.Fatalf("loading pet picker missing %q:\n%s", wantText, plain)
		}
	}
}

func TestPetCatalogPreservesQueryTypedWhileLoading(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.picker = &commandPicker{kind: pickerPet, loading: true, query: "boba"}
	updated, _ := m.applyPetCatalog(petCatalogLoadedMsg{entries: []terminalpet.Entry{
		{Slug: "kratos", DisplayName: "Kratos", Local: true},
		{Slug: "boba", DisplayName: "Boba"},
	}})
	next := updated.(model)
	if next.picker.query != "boba" || len(next.picker.items) != 1 || next.picker.items[0].Value != "boba" {
		t.Fatalf("loaded picker did not preserve query: %#v", next.picker)
	}
}

func TestPetPickerRowsTruncateLongNames(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 100
	m.picker = &commandPicker{kind: pickerPet, title: "Choose a companion", items: []pickerItem{{
		Label: "A very long companion name that keeps going far beyond the available picker row width and must be truncated safely",
		Value: "long-pet",
	}}, selected: 0}
	m.petEntries["long-pet"] = terminalpet.Entry{Slug: "long-pet", DisplayName: "Long pet", Kind: "character", SubmittedBy: "tester"}

	plain := plainRender(t, m.petPickerOverlay(m.width))
	var row string
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "A very long companion") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatalf("pet row missing:\n%s", plain)
	}
	if !strings.Contains(row, "…") || strings.Contains(row, "truncated safely") {
		t.Fatalf("pet row should truncate its long name: %q", row)
	}
}

func TestPetPickerViewSchedulesTerminalImageInsteadOfANSIArt(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width, m.height = 110, 34
	m.picker = &commandPicker{kind: pickerPet, title: "Choose a companion", items: []pickerItem{{Label: "Boba", Value: "boba"}}, selected: 0}
	m.petEntries["boba"] = terminalpet.Entry{Slug: "boba", DisplayName: "Boba", SubmittedBy: "tester"}
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	for y := range 12 {
		for x := range 12 {
			frame.SetNRGBA(x, y, color.NRGBA{R: 120, G: 80, B: 200, A: 255})
		}
	}
	m.petPreview, _ = terminalpet.ThumbnailAnimation(frame)
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})

	view := m.View()
	if strings.Contains(plainRender(t, view.Content), "▀") {
		t.Fatalf("pet picker still contains ANSI image cells: %q", plainRender(t, view.Content))
	}
	var output bytes.Buffer
	if err := m.petRenderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "_Ga=T,t=d,f=100,c=9,r=5") {
		t.Fatalf("pet picker did not schedule a Kitty image: %q", got)
	}
}

func TestPetPickerPreviewUsesRightPaneWhenWide(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width, m.height = 110, 34
	m.picker = &commandPicker{kind: pickerPet, title: "Choose a companion", items: []pickerItem{
		{Group: "Discover", Label: "Aion", Value: "aion"},
		{Group: "Discover", Label: "AirRing", Value: "airring"},
		{Group: "Discover", Label: "Akane", Value: "akane"},
	}, selected: 1}
	m.petEntries["airring"] = terminalpet.Entry{Slug: "airring", DisplayName: "AirRing", SubmittedBy: "tester"}
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	m.petPreview, _ = terminalpet.ThumbnailAnimation(frame)

	content := m.petPickerOverlay(m.width)
	x, y, ok := petPickerImagePosition(content, petImageColumns, petImageRows)
	if !ok {
		t.Fatalf("wide picker should provide preview image geometry:\n%s", plainRender(t, content))
	}
	if x < m.width*2/3 {
		t.Fatalf("wide picker preview x=%d, want a right-side pane in width %d", x, m.width)
	}
	detailRow := -1
	for index, line := range viewLines(content) {
		if strings.Contains(ansi.Strip(line), "AirRing · by tester") {
			detailRow = index
			break
		}
	}
	if detailRow < 0 || y+petImageRows > detailRow {
		t.Fatalf("preview rows [%d,%d) should sit beside the list above detail row %d:\n%s", y, y+petImageRows, detailRow, plainRender(t, content))
	}
	previewRow := ""
	for _, line := range viewLines(content) {
		plain := ansi.Strip(line)
		if strings.Contains(plain, "Preview") {
			previewRow = plain
			break
		}
	}
	if previewRow == "" {
		t.Fatalf("wide picker should label its preview pane:\n%s", plainRender(t, content))
	}
	if !strings.Contains(previewRow, "│") {
		t.Fatalf("wide picker should separate the list and preview panes: %q", previewRow)
	}
	if strings.Contains(previewRow, "…") {
		t.Fatalf("preview heading row should not truncate the list pane: %q", previewRow)
	}
}

func TestPetPickerPreviewUsesRightPaneWhenTitleAndFooterAreClipped(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width, m.height = 110, 34
	m.picker = &commandPicker{kind: pickerPet, title: "Choose a companion", items: []pickerItem{
		{Group: "Installed", Label: "Dasheng", Value: "dasheng"},
		{Group: "Installed", Label: "Ddo-zvzo", Value: "ddo-zvzo"},
		{Group: "Installed", Label: "Doraemon", Value: "doraemon"},
		{Group: "Installed", Label: "Kratos Greek", Value: "kratos-greek"},
		{Group: "Installed", Label: "Luffy", Value: "luffy"},
		{Group: "Installed", Label: "Luffy Gear 5", Value: "luffy-gear-5"},
		{Group: "Installed", Label: "Sasuke Uchiha", Value: "sasuke-uchiha"},
	}, selected: 1}
	m.petEntries["kratos-greek"] = terminalpet.Entry{Slug: "kratos-greek", DisplayName: "Kratos Greek", SubmittedBy: "tester"}
	m.petPreview, _ = terminalpet.ThumbnailAnimation(image.NewNRGBA(image.Rect(0, 0, 12, 12)))
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})

	lines := viewLines(m.petPickerOverlay(m.width))
	search, footer := -1, -1
	for index, line := range lines {
		if strings.Contains(ansi.Strip(line), "search >") {
			search = index
		}
		if strings.Contains(ansi.Strip(line), "↑/↓ preview") {
			footer = index
			break
		}
	}
	if search < 0 || footer < 0 {
		t.Fatalf("picker search or footer missing from test fixture")
	}
	clipped := append([]string{strings.Repeat("─", m.width)}, lines[search:footer]...)
	content := strings.Join(clipped, "\n")

	x, _, ok := petPickerImagePosition(content, petImageColumns, petImageRows)
	if !ok {
		t.Fatalf("clipped picker should retain right-pane preview geometry:\n%s", plainRender(t, content))
	}
	searchRunes := []rune(ansi.Strip(lines[search]))
	rightBorder := -1
	for index := len(searchRunes) - 1; index >= 0; index-- {
		if searchRunes[index] == '│' {
			rightBorder = index
			break
		}
	}
	if rightBorder < 0 || x+petImageColumns > rightBorder {
		t.Fatalf("clipped picker preview [%d,%d) escapes modal right border %d:\n%s", x, x+petImageColumns, rightBorder, plainRender(t, content))
	}
	if draw := m.petImageDraw(content); draw == nil {
		t.Fatal("clipped picker should still schedule its preview image")
	}
}

func TestPetPickerHidesPreviewPaneForNoCompanion(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width, m.height = 110, 34
	m.picker = &commandPicker{kind: pickerPet, title: "Choose a companion", items: []pickerItem{
		{Label: "No companion", Value: terminalpet.DisabledID},
		{Group: "Discover", Label: "Boba", Value: "boba"},
	}, selected: 0}

	plain := plainRender(t, m.petPickerOverlay(m.width))
	if strings.Contains(plain, "Preview") {
		t.Fatalf("no-companion selection should not show an empty preview pane:\n%s", plain)
	}
}

func TestPetPickerMouseWheelSchedulesSelectedPreview(t *testing.T) {
	m := mouseTestModel()
	m.picker = &commandPicker{kind: pickerPet, title: "Choose a companion", items: []pickerItem{
		{Label: "Alpha", Value: "alpha"},
		{Label: "Beta", Value: "beta"},
	}, selected: 0}
	m.petEntries["alpha"] = terminalpet.Entry{Slug: "alpha"}
	m.petEntries["beta"] = terminalpet.Entry{Slug: "beta"}
	m.petPreviewSlug = "alpha"
	m.petPreviewLoading = false

	updated, cmd := m.Update(testMouseWheel(tea.MouseWheelDown, 1, 1))
	next := updated.(model)
	if next.picker.selected != 1 {
		t.Fatalf("wheel selected index = %d, want 1", next.picker.selected)
	}
	if next.petPreviewSlug != "beta" || !next.petPreviewLoading {
		t.Fatalf("wheel preview = %q loading=%v, want beta loading", next.petPreviewSlug, next.petPreviewLoading)
	}
	if cmd == nil {
		t.Fatal("wheel selection should schedule the selected pet preview")
	}
}

func TestPetPickerPasteFiltersAndSchedulesSelectedPreview(t *testing.T) {
	m := mouseTestModel()
	m.picker = &commandPicker{kind: pickerPet, title: "Choose a companion", items: []pickerItem{
		{Label: "Kratos Greek", Value: "kratos-greek"},
		{Label: "Luffy Gear 5", Value: "luffy-gear-5"},
	}, allItems: []pickerItem{
		{Label: "Kratos Greek", Value: "kratos-greek"},
		{Label: "Luffy Gear 5", Value: "luffy-gear-5"},
	}}
	m.petEntries["kratos-greek"] = terminalpet.Entry{Slug: "kratos-greek"}
	m.petEntries["luffy-gear-5"] = terminalpet.Entry{Slug: "luffy-gear-5"}

	updated, cmd := m.Update(tea.PasteMsg{Content: "luffy"})
	next := updated.(model)
	if next.picker.query != "luffy" {
		t.Fatalf("pasted pet query = %q, want luffy", next.picker.query)
	}
	if len(next.picker.items) != 1 || next.picker.items[0].Value != "luffy-gear-5" {
		t.Fatalf("pasted pet results = %#v, want only Luffy Gear 5", next.picker.items)
	}
	if next.petPreviewSlug != "luffy-gear-5" || cmd == nil {
		t.Fatalf("pasted pet preview = %q cmd=%v, want luffy-gear-5 and refresh", next.petPreviewSlug, cmd)
	}
}

func TestPetPickerPreviewStacksBelowListWhenNarrow(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width, m.height = 50, 28
	m.picker = &commandPicker{kind: pickerPet, title: "Choose a companion", items: []pickerItem{{Label: "Boba", Value: "boba"}}, selected: 0}
	m.petEntries["boba"] = terminalpet.Entry{Slug: "boba", DisplayName: "Boba"}
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	m.petPreview, _ = terminalpet.ThumbnailAnimation(frame)

	content := m.petPickerOverlay(m.width)
	x, y, ok := petPickerImagePosition(content, petImageColumns, petImageRows)
	if !ok {
		t.Fatalf("narrow picker should provide stacked preview geometry:\n%s", plainRender(t, content))
	}
	if x < m.width/3 || x > m.width*2/3 {
		t.Fatalf("narrow picker preview x=%d, want centered in width %d", x, m.width)
	}
	listRow := -1
	for index, line := range viewLines(content) {
		if strings.Contains(ansi.Strip(line), "Boba") {
			listRow = index
			break
		}
	}
	if listRow < 0 || y <= listRow {
		t.Fatalf("stacked preview row %d should follow list row %d:\n%s", y, listRow, plainRender(t, content))
	}
}

func TestAmbientPetFreePositionTracksViewportResize(t *testing.T) {
	m := interactivePetTestModel(t)
	m.width, m.height = 89, 25
	m.petPositionSet = true
	originalCenterX := (m.width - petImageColumns) / 2
	originalCenterY := (m.height - petImageRows) / 2
	m.petPositionX = originalCenterX + 12
	m.petPositionY = originalCenterY - 6

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 169, Height: 45})
	next := updated.(model)
	wantX := (next.width-petImageColumns)/2 + 12
	wantY := (next.height-petImageRows)/2 - 6
	if next.petPositionX != wantX || next.petPositionY != wantY {
		t.Fatalf("resized free pet = (%d,%d), want preserved center offset (%d,%d)",
			next.petPositionX, next.petPositionY, wantX, wantY)
	}

	updated, _ = next.Update(tea.WindowSizeMsg{Width: 89, Height: 25})
	roundTrip := updated.(model)
	if roundTrip.petPositionX != m.petPositionX || roundTrip.petPositionY != m.petPositionY {
		t.Fatalf("round-trip resize moved free pet to (%d,%d), want (%d,%d)",
			roundTrip.petPositionX, roundTrip.petPositionY, m.petPositionX, m.petPositionY)
	}
}

func TestAmbientPetSubCellPositionTracksViewportResize(t *testing.T) {
	m := interactivePetTestModel(t)
	m.width, m.height = 89, 25
	m.petCellPixelWidth, m.petCellPixelHeight = 8, 16
	m.petPositionSet = true
	m.petPositionX, m.petPositionOffsetX = 20, 4
	m.petPositionY, m.petPositionOffsetY = 5, 7
	originalX := m.petPositionX*m.petCellPixelWidth + m.petPositionOffsetX
	originalY := m.petPositionY*m.petCellPixelHeight + m.petPositionOffsetY

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 169, Height: 45})
	next := updated.(model)
	gotX := next.petPositionX*next.petCellPixelWidth + next.petPositionOffsetX
	gotY := next.petPositionY*next.petCellPixelHeight + next.petPositionOffsetY
	oldMaxX := (m.width - petImageColumns) * m.petCellPixelWidth
	oldMaxY := (m.height - petImageRows) * m.petCellPixelHeight
	newMaxX := (next.width - petImageColumns) * next.petCellPixelWidth
	newMaxY := (next.height - petImageRows) * next.petCellPixelHeight
	wantX := originalX + newMaxX/2 - oldMaxX/2
	wantY := originalY + newMaxY/2 - oldMaxY/2
	if gotX != wantX || gotY != wantY {
		t.Fatalf("resized sub-cell pet = (%d,%d), want preserved center offset (%d,%d)", gotX, gotY, wantX, wantY)
	}

	updated, _ = next.Update(tea.WindowSizeMsg{Width: 89, Height: 25})
	roundTrip := updated.(model)
	gotX = roundTrip.petPositionX*roundTrip.petCellPixelWidth + roundTrip.petPositionOffsetX
	gotY = roundTrip.petPositionY*roundTrip.petCellPixelHeight + roundTrip.petPositionOffsetY
	if gotX != originalX || gotY != originalY {
		t.Fatalf("round-trip sub-cell resize = (%d,%d), want (%d,%d)", gotX, gotY, originalX, originalY)
	}
}

func TestAmbientPetBottomRightPositionTracksViewportEdges(t *testing.T) {
	m := interactivePetTestModel(t)
	m.width, m.height = 89, 25
	m.petPositionSet = true
	oldMaxX := m.width - petImageColumns
	oldMaxY := m.height - petImageRows
	m.petPositionX = oldMaxX - 2
	m.petPositionY = oldMaxY - 1

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 169, Height: 45})
	next := updated.(model)
	wantX := next.width - petImageColumns - 2
	wantY := next.height - petImageRows - 1
	if next.petPositionX != wantX || next.petPositionY != wantY {
		t.Fatalf("resized bottom-right pet = (%d,%d), want preserved edge gaps (%d,%d)",
			next.petPositionX, next.petPositionY, wantX, wantY)
	}
}

func TestAmbientPetSubCellBottomRightPositionTracksViewportEdges(t *testing.T) {
	m := interactivePetTestModel(t)
	m.width, m.height = 89, 25
	m.petCellPixelWidth, m.petCellPixelHeight = 8, 16
	m.petPositionSet = true
	oldMaxX := (m.width - petImageColumns) * m.petCellPixelWidth
	oldMaxY := (m.height - petImageRows) * m.petCellPixelHeight
	originalX := oldMaxX - 7
	originalY := oldMaxY - 5
	m.petPositionX, m.petPositionOffsetX = originalX/m.petCellPixelWidth, originalX%m.petCellPixelWidth
	m.petPositionY, m.petPositionOffsetY = originalY/m.petCellPixelHeight, originalY%m.petCellPixelHeight

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 169, Height: 45})
	next := updated.(model)
	gotX := next.petPositionX*next.petCellPixelWidth + next.petPositionOffsetX
	gotY := next.petPositionY*next.petCellPixelHeight + next.petPositionOffsetY
	wantX := (next.width-petImageColumns)*next.petCellPixelWidth - 7
	wantY := (next.height-petImageRows)*next.petCellPixelHeight - 5
	if gotX != wantX || gotY != wantY {
		t.Fatalf("resized sub-cell bottom-right pet = (%d,%d), want preserved edge gaps (%d,%d)",
			gotX, gotY, wantX, wantY)
	}
}

func TestPetLayoutRequiresRoomAndNoModal(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.altScreen, m.width, m.height = true, 120, 30
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "hello"})
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	animation, _ := terminalpet.ThumbnailAnimation(frame)
	m.petID, m.petAnimation = "boba", animation
	if !m.petLayoutActive() {
		t.Fatal("pet layout should be active in a wide, non-modal transcript")
	}
	m.picker = &commandPicker{kind: pickerPet}
	if m.petLayoutActive() {
		t.Fatal("pet layout should hide behind a modal")
	}
}

func TestAmbientPetFloatsWithoutSidebarDivider(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width, m.height = 110, 34
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "hello"})
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	m.petAnimation, _ = terminalpet.ThumbnailAnimation(frame)
	m.petID = "boba"
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})

	view := m.View()
	plain := plainRender(t, view.Content)
	dividerColumn := m.width - petReservedColumns
	for _, line := range strings.Split(plain, "\n") {
		runes := []rune(line)
		if len(runes) > dividerColumn && runes[dividerColumn] == '│' {
			t.Fatalf("ambient pet rendered a sidebar divider at column %d: %q", dividerColumn, line)
		}
	}
	var output bytes.Buffer
	if err := m.petRenderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "_Ga=T,t=d,f=100,c=9,r=5") {
		t.Fatalf("ambient pet did not use floating image geometry: %q", got)
	}
}

func TestAmbientPetRemainsVisibleInNarrowTerminal(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width, m.height = 60, 24
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "hello"})
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	m.petAnimation, _ = terminalpet.ThumbnailAnimation(frame)
	m.petID = "boba"
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})

	_ = m.View()
	var output bytes.Buffer
	if err := m.petRenderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "_Ga=T,t=d,f=100,c=9,r=5") {
		t.Fatalf("narrow terminal hid the ambient pet: %q", got)
	}
}

func TestAmbientPetRetainsSingleColumnComposerDock(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width, m.height = 110, 34
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "hello"})
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	m.petAnimation, _ = terminalpet.ThumbnailAnimation(frame)
	m.petID = "boba"
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	m.unpricedTokens = 11700

	view := m.View()
	plain := plainRender(t, view.Content)
	composerWidth := 0
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "╭") && strings.Contains(line, "─") {
			composerWidth = len([]rune(strings.TrimRight(line, " ")))
		}
	}
	if composerWidth != m.width-petReservedColumns {
		t.Fatalf("visible composer width = %d, want dock to start at %d", composerWidth, m.width-petReservedColumns)
	}
	dockEdge := m.width - petReservedColumns - 1
	foundTop, foundInput := false, false
	for _, line := range strings.Split(plain, "\n") {
		runes := []rune(line)
		if len(runes) < m.width {
			continue
		}
		if strings.HasPrefix(line, "╭") {
			foundTop = true
			if runes[dockEdge] != '╮' {
				t.Fatalf("composer top is not closed before the pet dock: %q", line)
			}
		}
		if strings.HasPrefix(line, "│") && strings.Contains(line, "describe a task for zero") {
			foundInput = true
			if runes[dockEdge] != '│' {
				t.Fatalf("composer input is not closed before the pet dock: %q", line)
			}
		}
	}
	if !foundTop || !foundInput {
		t.Fatalf("composer rows missing: top=%v input=%v", foundTop, foundInput)
	}
	expectedY := len(viewLines(view.Content)) - petImageRows - 1
	expectedX := m.width - petImageColumns - 2
	var output bytes.Buffer
	if err := m.petRenderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	wantCursor := fmt.Sprintf("\x1b[%d;%dH", expectedY+1, expectedX+1)
	if got := output.String(); !strings.Contains(got, wantCursor) {
		t.Fatalf("pet cursor missing %q: %q", wantCursor, got)
	}
}

func TestAmbientPetDoesNotChangeFullWidthFooterGeometry(t *testing.T) {
	m := sidebarTestModel()
	m.width, m.height = 120, 34
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	m.petAnimation, _ = terminalpet.ThumbnailAnimation(frame)
	m.petID = "boba"
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})

	view := m.View()
	var output bytes.Buffer
	if err := m.petRenderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	wantCursor := fmt.Sprintf("\x1b[%d;%dH", len(viewLines(view.Content))-petImageRows, m.chatColumnWidth()-petImageColumns-1)
	if got := output.String(); !strings.Contains(got, wantCursor) {
		t.Fatalf("sidebar hid or misplaced pet; cursor missing %q: %q", wantCursor, got)
	}

	plain := plainRender(t, view.Content)
	for _, line := range strings.Split(plain, "\n") {
		if !strings.HasPrefix(line, "╭") {
			continue
		}
		runes := []rune(line)
		dockEdge := m.chatColumnWidth() - petReservedColumns - 1
		if len(runes) < m.width || runes[dockEdge] != '╮' {
			t.Fatalf("pet changed the normal full-width composer geometry: %q", line)
		}
		return
	}
	t.Fatal("composer top not found")
}

func TestDockedPetFollowsSidebarLayoutWithoutBecomingFreePositioned(t *testing.T) {
	m := sidebarTestModel()
	m.width, m.height = 120, 34
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	m.petAnimation, _ = terminalpet.ThumbnailAnimation(frame)
	m.petID = "boba"
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})

	withSidebarX, _ := m.ambientPetPosition(m.width, m.height)
	if want := m.chatColumnWidth() - petImageColumns - 2; withSidebarX != want {
		t.Fatalf("sidebar dock x = %d, want chat-column dock %d", withSidebarX, want)
	}
	m.sidebarHidden = true
	withoutSidebarX, _ := m.ambientPetPosition(m.width, m.height)
	if want := m.width - petImageColumns - 2; withoutSidebarX != want {
		t.Fatalf("single-column dock x = %d, want %d", withoutSidebarX, want)
	}
	if m.petPositionSet {
		t.Fatal("layout transitions must not turn a docked pet into a free-positioned pet")
	}
}

func TestAmbientPetDragMovesAndClampsInsideViewport(t *testing.T) {
	m := interactivePetTestModel(t)
	startX, startY := m.ambientPetPosition(m.width, m.height)

	next, _, handled := m.handlePetMouse(testMouseClick(tea.MouseLeft, startX+2, startY+2))
	if !handled || !next.petDragActive {
		t.Fatal("press inside the pet should start a drag")
	}
	next, _, handled = next.handlePetMouse(testMouseMotion(tea.MouseLeft, -20, -20))
	if !handled || next.petDragTargetX != 0 || next.petDragTargetY != 0 {
		t.Fatalf("drag target should clamp to top-left, got (%d,%d)", next.petDragTargetX, next.petDragTargetY)
	}
	next, _, handled = next.handlePetMouse(testMouseRelease(tea.MouseLeft, -20, -20))
	if !handled || next.petDragActive || !next.petPositionSet {
		t.Fatalf("release should finish and retain the drag: %#v", next)
	}
	draw := next.petImageDraw(next.transcriptView())
	if draw == nil || draw.X != 0 || draw.Y != 0 {
		t.Fatalf("dragged pet draw = %#v, want top-left", draw)
	}
}

func TestAmbientPetDragKeepsAdvancingFrames(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petPhase = 7
	m.petPlaybackState = terminalpet.Idle
	startX, startY := m.ambientPetPosition(m.width, m.height)
	m, _, handled := m.handlePetMouse(testMouseClick(tea.MouseLeft, startX+2, startY+2))
	if !handled || !m.petDragActive {
		t.Fatal("press inside the pet should start a drag")
	}
	m.petPhase = 12
	draw := m.petImageDraw(m.transcriptView())
	if draw == nil {
		t.Fatal("held pet draw is nil")
	}
	if draw.Phase != 12 {
		t.Fatalf("held pet phase = %d, want current phase 12", draw.Phase)
	}
}

func TestAmbientPetDragAllowsBottomEdge(t *testing.T) {
	m := interactivePetTestModel(t)
	startX, startY := m.ambientPetPosition(m.width, m.height)

	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, startX+2, startY+2))
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, startX+2, m.height+20))
	m, _, _ = m.handlePetMouse(testMouseRelease(tea.MouseLeft, startX+2, m.height+20))

	draw := m.petImageDraw(m.transcriptView())
	if draw == nil {
		t.Fatal("dragged pet draw is nil")
	}
	if draw.Y != m.height-petImageRows {
		t.Fatalf("dragged pet y = %d, want bottom-edge row %d", draw.Y, m.height-petImageRows)
	}
}

func TestAmbientKittyPetRestoresDockAfterFreeDrag(t *testing.T) {
	m := interactivePetTestModel(t)
	homeX, homeY := m.ambientPetPosition(m.width, m.height)

	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, homeX+2, homeY+2))
	if got := m.petComposerReservedColumns(m.width); got != petReservedColumns {
		t.Fatalf("pressing a docked pet changed composer reservation to %d", got)
	}
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, 30, 12))
	if got := m.petComposerReservedColumns(m.width); got != petReservedColumns {
		t.Fatalf("dragging away reflowed composer before release: reservation=%d", got)
	}
	m, _, _ = m.handlePetMouse(testMouseRelease(tea.MouseLeft, 30, 12))
	if !m.petPositionSet || m.petComposerReservedColumns(m.width) != 0 {
		t.Fatal("moving a Kitty pet should release the composer dock")
	}

	freeX, freeY := m.ambientPetPosition(m.width, m.height)
	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, freeX+2, freeY+2))
	if got := m.petComposerReservedColumns(m.width); got != 0 {
		t.Fatalf("dragging a free pet recreated the dock reservation: %d", got)
	}
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, homeX+2, homeY+2))
	if got := m.petComposerReservedColumns(m.width); got != 0 {
		t.Fatalf("hovering over the dock reflowed composer before release: %d", got)
	}
	m, _, _ = m.handlePetMouse(testMouseRelease(tea.MouseLeft, homeX+2, homeY+2))
	if m.petPositionSet {
		t.Fatal("returning the pet home should restore its docked state")
	}
	if got := m.petComposerReservedColumns(m.width); got != petReservedColumns {
		t.Fatalf("restored Kitty dock reservation = %d, want %d", got, petReservedColumns)
	}
}

func TestAmbientPetDrawAndHitUseTerminalHeight(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petPositionSet = true
	m.petPositionX = 20
	m.petPositionY = m.height - petImageRows
	draw := m.petImageDraw("short content")
	if draw == nil || draw.Y != m.petPositionY {
		t.Fatalf("draw position = %#v, want terminal-relative y %d", draw, m.petPositionY)
	}
	if !m.petHit(draw.X+1, draw.Y+1) {
		t.Fatal("rendered pet position did not match its hit target")
	}
}

func TestPetComposerDividerFallbackPreservesDockedColumns(t *testing.T) {
	m := interactivePetTestModel(t)
	m.modelName = strings.Repeat("long-model-name", 8)

	line := plainRender(t, m.composerDividerLine(m.width))
	if got := ansi.StringWidth(line); got != m.width {
		t.Fatalf("divider width = %d, want %d", got, m.width)
	}
	if suffix := strings.Repeat(" ", petReservedColumns); !strings.HasSuffix(line, suffix) {
		t.Fatalf("divider did not preserve %d docked columns: %q", petReservedColumns, line)
	}
}

func TestAmbientSixelPetRetainsDockedComposerFallbackSlot(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolSixel})
	if got := m.petComposerReservedColumns(m.width); got != petReservedColumns {
		t.Fatalf("docked Sixel fallback reservation = %d, want %d", got, petReservedColumns)
	}
}

func TestAmbientSixelPetCannotLeaveReservedDock(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolSixel})
	m.petPositionSet = true
	m.petPositionX, m.petPositionY = 3, 2
	homeX, homeY := m.petHomePosition(m.width, m.height)

	if x, y := m.ambientPetPosition(m.width, m.height); x != homeX || y != homeY {
		t.Fatalf("Sixel pet position = (%d,%d), want reserved dock (%d,%d)", x, y, homeX, homeY)
	}
	m.petPositionSet = false
	m, _, handled := m.handlePetMouse(testMouseClick(tea.MouseLeft, homeX+2, homeY+2))
	if !handled || !m.petDragActive {
		t.Fatal("Sixel pet press should remain available for click animations")
	}
	m, _, handled = m.handlePetMouse(testMouseMotion(tea.MouseLeft, 2, 2))
	if !handled || m.petDragMoved {
		t.Fatal("Sixel pet motion must not move it out of the reserved dock")
	}
	m, _, handled = m.handlePetMouse(testMouseRelease(tea.MouseLeft, 2, 2))
	if !handled || m.petPositionSet || m.petDragActive {
		t.Fatal("Sixel pet release must keep the reserved dock without leaving drag state")
	}
	if got := m.petComposerReservedColumns(m.width); got != petReservedColumns {
		t.Fatalf("Sixel dock reservation = %d, want %d", got, petReservedColumns)
	}
}

func TestAmbientKittyPetStillSupportsFreePositioning(t *testing.T) {
	m := interactivePetTestModel(t)
	homeX, homeY := m.ambientPetPosition(m.width, m.height)
	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, homeX+2, homeY+2))
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, 2, 2))
	m, _, _ = m.handlePetMouse(testMouseRelease(tea.MouseLeft, 2, 2))

	if !m.petPositionSet {
		t.Fatal("Kitty pet no longer supports free positioning")
	}
	if x, y := m.ambientPetPosition(m.width, m.height); x == homeX && y == homeY {
		t.Fatal("Kitty pet remained docked after a free-positioning drag")
	}
}

func TestSixelPetFitsItsReservedTerminalRows(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolSixel})
	m.petCellPixelHeight = 10
	draw := m.petImageDraw(m.transcriptView())
	if draw == nil {
		t.Fatal("Sixel pet draw is unavailable")
	}
	want := petImageRows * m.petCellPixelHeight
	if draw.HeightPixels != want {
		t.Fatalf("Sixel height = %dpx, want reserved height %dpx", draw.HeightPixels, want)
	}
}

func TestAmbientPetRemainsVisibleButNotInteractiveDuringPermissionPrompt(t *testing.T) {
	m := sidebarTestModel()
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	m.petAnimation, _ = terminalpet.ThumbnailAnimation(frame)
	m.petID = "boba"
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	m.pendingPermission = &pendingPermissionPrompt{request: testPromptPermissionRequest()}

	view := m.View()
	var output bytes.Buffer
	if err := m.petRenderer.Render(&output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "_Ga=T") {
		t.Fatalf("permission prompt hid the ambient pet: %q", output.String())
	}

	draw := m.petImageDraw(view.Content)
	wantX := m.chatColumnWidth() - petImageColumns - 2
	if draw == nil || draw.X != wantX {
		t.Fatalf("permission pet should remain in the chat-column dock at x=%d: %#v", wantX, draw)
	}
	x, y := draw.X, draw.Y
	if m.petHit(x+1, y+1) {
		t.Fatal("a pet shown during a permission prompt must not intercept modal clicks")
	}
}

func TestAmbientPetDragReleaseDoesNotClearScreen(t *testing.T) {
	m := interactivePetTestModel(t)
	homeX, homeY := m.ambientPetPosition(m.width, m.height)

	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, homeX+2, homeY+2))
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, 30, 12))
	m, cmd, _ := m.handlePetMouse(testMouseRelease(tea.MouseLeft, 30, 12))
	if cmdIncludesClearScreen(cmd) {
		t.Fatal("releasing a freely positioned pet must not flash a full-screen clear")
	}

	freeX, freeY := m.ambientPetPosition(m.width, m.height)
	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, freeX+2, freeY+2))
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, homeX+2, homeY+2))
	_, cmd, _ = m.handlePetMouse(testMouseRelease(tea.MouseLeft, homeX+2, homeY+2))
	if cmdIncludesClearScreen(cmd) {
		t.Fatal("snapping a pet into its dock must not flash a full-screen clear")
	}
}

func TestAmbientPetDockSnapAcceptsNearbySubCellPosition(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petCellPixelWidth, m.petCellPixelHeight = 8, 16
	homeX, homeY := m.petHomePosition(m.width, m.height)
	m.petDragTargetX = homeX - 1
	m.petDragTargetY = homeY
	m.petDragTargetOffsetX = 7
	m.petDragTargetOffsetY = 7
	if !m.petDragTargetIsDocked() {
		t.Fatal("a visually returned sub-cell position should snap into the dock")
	}
	m.petDragTargetOffsetY = 9
	if m.petDragTargetIsDocked() {
		t.Fatal("a position beyond half a row should remain freely positioned")
	}
	m.petDragTargetY = homeY - 1
	m.petDragTargetOffsetY = 0
	if !m.petDragTargetIsDocked() {
		t.Fatal("the row immediately above home should remain inside the visual dock target")
	}
	m.petDragTargetY = homeY + 1
	if m.petDragTargetIsDocked() {
		t.Fatal("the row below home should remain available for free bottom-edge placement")
	}
}

func TestAmbientPetDragFollowsPointerBeforeRelease(t *testing.T) {
	m := interactivePetTestModel(t)
	x, y := m.ambientPetPosition(m.width, m.height)
	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, x+2, y+2))
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, 25, 12))

	content := m.transcriptView()
	draw := m.petImageDraw(content)
	if draw == nil || draw.X != m.petDragTargetX || draw.Y != m.petDragTargetY {
		t.Fatalf("dragged pet should follow target (%d,%d), got %#v", m.petDragTargetX, m.petDragTargetY, draw)
	}
	if plain := plainRender(t, content); strings.Contains(plain, "│ move  │") {
		t.Fatalf("drag should not draw a placeholder:\n%s", plain)
	}
}

func TestAmbientPetPixelDragUsesSubCellOffsetsOnlyWhileHeld(t *testing.T) {
	m := interactivePetTestModel(t)
	m.reducedMotion = true
	m.petCellPixelWidth = 8
	m.petCellPixelHeight = 16
	x, y := m.ambientPetPosition(m.width, m.height)

	m, cmd, handled := m.handlePetMouse(testMouseClick(tea.MouseLeft, x+2, y+2))
	if !handled || !m.petPixelDrag || cmd == nil {
		t.Fatal("pressing a Kitty pet with known cell pixels should start pixel drag mode")
	}
	if !petCommandIncludesRaw(cmd, ansi.SetModeMouseExtSgrPixel) {
		t.Fatal("pixel drag command does not include pixel mouse mode enable")
	}

	pointerX := (x+2)*m.petCellPixelWidth + 3
	pointerY := (y+2)*m.petCellPixelHeight + 5
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, pointerX, pointerY))
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, pointerX-21, pointerY+11))
	draw := m.petImageDraw(m.transcriptView())
	if draw == nil || draw.X != x-3 || draw.Y != y || draw.OffsetX != 3 || draw.OffsetY != 11 {
		t.Fatalf("pixel drag draw = %#v, want cell (%d,%d) with offset (3,11)", draw, x-3, y)
	}

	m, cmd, handled = m.handlePetMouse(testMouseRelease(tea.MouseLeft, pointerX-21, pointerY+11))
	if !handled || m.petPixelDrag || m.petDragActive || cmd == nil {
		t.Fatal("pixel release should commit the position and restore cell mouse mode")
	}
	if m.petPositionOffsetX != 3 || m.petPositionOffsetY != 11 {
		t.Fatalf("committed pixel offset = (%d,%d), want (3,11)", m.petPositionOffsetX, m.petPositionOffsetY)
	}
}

func TestAmbientPetPixelDragTracksEveryAcceptedEventExactly(t *testing.T) {
	m := interactivePetTestModel(t)
	m.reducedMotion = false
	m.petCellPixelWidth = 8
	m.petCellPixelHeight = 16
	x, y := m.ambientPetPosition(m.width, m.height)

	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, x+2, y+2))
	pointerX := (x+2)*m.petCellPixelWidth + 3
	pointerY := (y+2)*m.petCellPixelHeight + 5
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, pointerX, pointerY))

	destinationX, destinationY := pointerX-64, pointerY-32
	m, cmd, _ := m.handlePetMouse(testMouseMotion(tea.MouseLeft, destinationX, destinationY))
	wantX := clampInt(destinationX-m.petDragOffsetPixelX, 0, maxInt(0, (m.width-petImageColumns)*m.petCellPixelWidth))
	wantY := clampInt(destinationY-m.petDragOffsetPixelY, 0, maxInt(0, (m.height-petImageRows)*m.petCellPixelHeight))
	gotX, gotY := m.petDragAbsolutePosition()
	if gotX != wantX || gotY != wantY {
		t.Fatalf("pet trails accepted pointer event: got (%d,%d), want (%d,%d)", gotX, gotY, wantX, wantY)
	}
	if cmd == nil {
		t.Fatal("direct drag should flush the external pet image even when the text view is unchanged")
	}
	if !petCommandIncludesRaw(cmd, terminalSyncStart+terminalSyncEnd) {
		t.Fatal("direct drag command does not include the external image flush")
	}
}

func TestAmbientPetDragFlushesEquallyBeforeAndAfterGenerationEnds(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petCellPixelWidth = 8
	m.petCellPixelHeight = 16
	x, y := m.ambientPetPosition(m.width, m.height)
	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, x+2, y+2))
	pointerX := (x+2)*m.petCellPixelWidth + 3
	pointerY := (y+2)*m.petCellPixelHeight + 5
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, pointerX, pointerY))

	m.pending = true
	m, activeCmd, _ := m.handlePetMouse(testMouseMotion(tea.MouseLeft, pointerX-16, pointerY))
	m.pending = false
	_, idleCmd, _ := m.handlePetMouse(testMouseMotion(tea.MouseLeft, pointerX-32, pointerY))
	for state, cmd := range map[string]tea.Cmd{"active": activeCmd, "idle": idleCmd} {
		if cmd == nil {
			t.Fatalf("%s drag did not request an external image flush", state)
		}
		if !petCommandIncludesRaw(cmd, terminalSyncStart+terminalSyncEnd) {
			t.Fatalf("%s drag command does not include the external image flush", state)
		}
	}
}

func petCommandIncludesRaw(cmd tea.Cmd, want any) bool {
	if cmd == nil {
		return false
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	select {
	case msg := <-result:
		if raw, ok := msg.(tea.RawMsg); ok {
			return raw.Msg == want
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, child := range batch {
				if petCommandIncludesRaw(child, want) {
					return true
				}
			}
		}
	case <-time.After(100 * time.Millisecond):
		// Animation ticks are deliberately delayed and are not raw terminal
		// commands, so do not wait for them while inspecting a batch.
	}
	return false
}

func TestSchedulePetPreviewWithoutPicker(t *testing.T) {
	m := interactivePetTestModel(t)
	m.picker = nil
	m.petPreviewLoading = true
	m.petPreviewSlug = "stale"
	next, cmd := m.schedulePetPreview()
	if cmd != nil {
		t.Fatal("schedulePetPreview scheduled work without a picker")
	}
	if next.petPreviewLoading || next.petPreviewSlug != "" {
		t.Fatalf("stale preview state was retained: loading=%v slug=%q", next.petPreviewLoading, next.petPreviewSlug)
	}
}

func TestAmbientPetPixelDragReleaseCommitsWithoutGap(t *testing.T) {
	m := interactivePetTestModel(t)
	m.reducedMotion = false
	m.petCellPixelWidth = 8
	m.petCellPixelHeight = 16
	x, y := m.ambientPetPosition(m.width, m.height)

	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, x+2, y+2))
	pointerX := (x+2)*m.petCellPixelWidth + 3
	pointerY := (y+2)*m.petCellPixelHeight + 5
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, pointerX, pointerY))
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, pointerX-53, pointerY-27))
	wantX, wantY := m.petDragAbsolutePosition()

	m, cmd, handled := m.handlePetMouse(testMouseRelease(tea.MouseLeft, pointerX-53, pointerY-27))
	if !handled || m.petDragActive || !m.petPositionSet {
		t.Fatal("release should immediately commit the exact direct-drag position")
	}
	if cmdIncludesClearScreen(cmd) {
		t.Fatal("committing a direct drag must not flash a full-screen clear")
	}
	gotX := m.petPositionX*m.petCellPixelWidth + m.petPositionOffsetX
	gotY := m.petPositionY*m.petCellPixelHeight + m.petPositionOffsetY
	if gotX != wantX || gotY != wantY {
		t.Fatalf("released pet = (%d,%d), want pointer destination (%d,%d)", gotX, gotY, wantX, wantY)
	}
}

func TestAmbientPetPixelDragFallsBackBeforeTerminalBoundary(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petCellPixelWidth = 8
	m.petCellPixelHeight = 16
	x, y := m.ambientPetPosition(m.width, m.height)

	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, x+2, y+2))
	pointerX := (x+2)*m.petCellPixelWidth + 3
	pointerY := (y+2)*m.petCellPixelHeight + 5
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, pointerX, pointerY))
	m, cmd, handled := m.handlePetMouse(testMouseMotion(tea.MouseLeft, m.petCellPixelWidth, pointerY))
	if !handled || !m.petDragActive || m.petPixelDrag || cmd == nil {
		t.Fatal("approaching the terminal edge should keep dragging but leave pixel mouse mode")
	}
	want := ansi.ResetModeMouseExtSgrPixel + ansi.SetModeMouseExtSgr + terminalSyncStart + terminalSyncEnd
	if !petCommandIncludesRaw(cmd, want) {
		t.Fatalf("edge fallback command did not include %q", want)
	}
}

func TestAmbientPetDragDropsMouseReportFragmentsInsteadOfTypingThem(t *testing.T) {
	m := interactivePetTestModel(t)
	m.input.SetValue("safe")
	m.petDragActive = true

	next, cmd := m.updateModel(testKey('1'))
	m = next.(model)
	if got := m.input.Value(); got != "safe" {
		t.Fatalf("drag-time key fragment changed composer to %q", got)
	}
	if cmd != nil || !m.petDragActive {
		t.Fatal("a leaked mouse fragment should be ignored without ending the drag")
	}
}

func TestPetPixelDragCellSizeAndEscapeRestoreNormalMouseMode(t *testing.T) {
	m := interactivePetTestModel(t)
	next, _ := m.updateModel(uv.CellSizeEvent{Width: 9, Height: 18})
	m = next.(model)
	if m.petCellPixelWidth != 9 || m.petCellPixelHeight != 18 {
		t.Fatalf("cell pixels = (%d,%d), want (9,18)", m.petCellPixelWidth, m.petCellPixelHeight)
	}
	m.petDragActive = true
	m.petPixelDrag = true
	m.lastKeyTime = time.Now()
	m.burstCount = 4
	next, cmd := m.updateModel(testKey(tea.KeyEsc))
	m = next.(model)
	if m.petDragActive || m.petPixelDrag || cmd == nil {
		t.Fatal("Escape should cancel pixel drag and return a mouse-mode restore command")
	}
	raw, ok := cmd().(tea.RawMsg)
	want := ansi.ResetModeMouseExtSgrPixel + ansi.SetModeMouseExtSgr
	if !ok || raw.Msg != want {
		t.Fatalf("pixel mouse restore command = %#v, want %q", raw, want)
	}
	if !m.lastKeyTime.IsZero() || m.burstCount != 0 {
		t.Fatalf("cancelled drag retained paste-burst state: time=%s count=%d", m.lastKeyTime, m.burstCount)
	}
}

func TestPetNonPixelDragCancelsOnTerminalBlur(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petDragActive = true
	m.petPixelDrag = false
	m.petDragMoved = true
	m.petDragState = terminalpet.Running
	m.lastKeyTime = time.Now()
	m.burstCount = 3

	next, cmd := m.updateModel(tea.BlurMsg{})
	m = next.(model)
	if m.petDragActive || m.petDragMoved || m.petDragState != terminalpet.Idle {
		t.Fatalf("blur left non-pixel drag active: active=%t moved=%t state=%q", m.petDragActive, m.petDragMoved, m.petDragState)
	}
	if cmd != nil {
		t.Fatal("non-pixel drag cancellation should not emit a pixel mouse command")
	}
	if !m.lastKeyTime.IsZero() || m.burstCount != 0 {
		t.Fatalf("blur retained paste-burst state: time=%s count=%d", m.lastKeyTime, m.burstCount)
	}
}

func TestDraggedKittyPetKeepsBackgroundContentForAlphaOverlay(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petPositionSet = true
	m.petPositionX, m.petPositionY = 3, 1
	lines := []string{
		"abcdefghijklmnopqrstuvwxyz",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"01234567890123456789012345",
		"content remains behind pet",
		"transparent pixels show it",
		"last line stays unchanged!",
	}

	got := m.reservePetImageSlot(lines, 26)
	if !reflect.DeepEqual(got, lines) {
		t.Fatalf("Kitty alpha overlay changed background rows:\ngot  %#v\nwant %#v", got, lines)
	}

	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolSixel})
	got = m.reservePetImageSlot(lines, 26)
	if reflect.DeepEqual(got, lines) {
		t.Fatal("Sixel placement should retain a cleared fallback region")
	}
}

func TestAmbientPetSingleClickWavesAndDoubleClickJumps(t *testing.T) {
	m := interactivePetTestModel(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	x, y := m.ambientPetPosition(m.width, m.height)

	click := func(current model) model {
		next, _, handled := current.handlePetMouse(testMouseClick(tea.MouseLeft, x+2, y+2))
		if !handled {
			t.Fatal("pet press was not handled")
		}
		next, _, handled = next.handlePetMouse(testMouseRelease(tea.MouseLeft, x+2, y+2))
		if !handled {
			t.Fatal("pet release was not handled")
		}
		return next
	}

	m = click(m)
	if got := m.petState(); got != terminalpet.Waving {
		t.Fatalf("single click state = %q, want waving", got)
	}
	if m.petPhase != 1 {
		t.Fatalf("single click phase = %d, want first visibly active waving frame", m.petPhase)
	}
	now = now.Add(200 * time.Millisecond)
	m = click(m)
	if got := m.petState(); got != terminalpet.Jumping {
		t.Fatalf("double click state = %q, want jumping", got)
	}
}

func TestAmbientPetClickRestartsAnimationTimerImmediately(t *testing.T) {
	m := interactivePetTestModel(t)
	m.reducedMotion = false
	m.petTickSeq = 7
	x, y := m.ambientPetPosition(m.width, m.height)

	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, x+2, y+2))
	next, cmd, handled := m.handlePetMouse(testMouseRelease(tea.MouseLeft, x+2, y+2))
	if !handled {
		t.Fatal("pet release was not handled")
	}
	if next.petTickSeq != 8 || cmd == nil {
		t.Fatalf("click restart = seq:%d cmd:%v, want seq 8 and a fresh frame timer", next.petTickSeq, cmd)
	}
}

func TestAmbientPetIgnoresTicksFromReplacedAnimationLoops(t *testing.T) {
	m := interactivePetTestModel(t)
	m.reducedMotion = false
	m.petTickSeq = 7
	m.petPhase = 3
	m.petPlaybackState = terminalpet.Idle

	updated, cmd := m.Update(petTickMsg{seq: 6})
	stale := updated.(model)
	if stale.petPhase != 3 || cmd != nil {
		t.Fatalf("stale tick advanced phase or rescheduled: phase=%d cmd=%v", stale.petPhase, cmd)
	}

	updated, cmd = stale.Update(petTickMsg{seq: 7})
	current := updated.(model)
	if current.petPhase != 4 || cmd == nil {
		t.Fatalf("current tick phase=%d cmd=%v, want phase 4 and a reschedule", current.petPhase, cmd)
	}
}

func TestAmbientPetRendersFirstFrameImmediatelyWhenPlaybackStateChanges(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petPhase = 5
	m.petPlaybackState = terminalpet.Idle
	m.pending = true

	draw := m.petImageDraw(m.transcriptView())
	if draw == nil {
		t.Fatal("working pet has no image draw")
	}
	if draw.State != terminalpet.Running || draw.Phase != 0 {
		t.Fatalf("working draw = state:%q phase:%d, want running phase zero", draw.State, draw.Phase)
	}
}

func TestAmbientPetKeepsLongActionAliveForItsFullPrimaryAnimation(t *testing.T) {
	m := interactivePetTestModel(t)
	atlas := image.NewNRGBA(image.Rect(0, 0, 192, 26*9))
	animation, err := terminalpet.AtlasAnimation(atlas, 1)
	if err != nil {
		t.Fatal(err)
	}
	m.petAnimation = animation
	started := time.Now()
	m.now = func() time.Time { return started.Add(2300 * time.Millisecond) }
	m.petOutcome = terminalpet.Jumping
	m.petOutcomeAt = started

	if got := m.petState(); got != terminalpet.Jumping {
		t.Fatalf("state after fixed 2.2s hold = %q, want jumping until its 2.52s sequence completes", got)
	}
	m.now = func() time.Time { return started.Add(2600 * time.Millisecond) }
	if got := m.petState(); got != terminalpet.Idle {
		t.Fatalf("state after full jumping sequence = %q, want idle", got)
	}
}

func TestAmbientPetDragUsesAdvancingDirectionalAnimationPhase(t *testing.T) {
	m := interactivePetTestModel(t)
	m.petDragActive = true
	m.petDragState = terminalpet.MoveRight
	m.petPlaybackState = terminalpet.MoveRight
	m.petPhase = 3

	draw := m.petImageDraw(m.transcriptView())
	if draw == nil {
		t.Fatal("dragging pet has no image draw")
	}
	if draw.State != terminalpet.MoveRight || draw.Phase != 3 {
		t.Fatalf("drag draw = state:%q phase:%d, want running-right phase 3", draw.State, draw.Phase)
	}
}

func TestAmbientPetFirstDragMovementRestartsDirectionalTicker(t *testing.T) {
	m := interactivePetTestModel(t)
	m.reducedMotion = false
	m.petTickSeq = 7
	m.petPhase = 4
	m.petPlaybackState = terminalpet.Idle
	x, y := m.ambientPetPosition(m.width, m.height)

	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, x+2, y+2))
	m, cmd, handled := m.handlePetMouse(testMouseMotion(tea.MouseLeft, x, y+2))
	if !handled {
		t.Fatal("first drag movement was not handled")
	}
	if m.petDragState != terminalpet.MoveLeft || m.petPlaybackState != terminalpet.MoveLeft ||
		m.petPhase != 0 || m.petTickSeq != 8 || cmd == nil {
		t.Fatalf("first movement = drag:%q playback:%q phase:%d seq:%d cmd:%v",
			m.petDragState, m.petPlaybackState, m.petPhase, m.petTickSeq, cmd)
	}
}

func TestAmbientPetVerticalDragUsesRunningAnimation(t *testing.T) {
	m := interactivePetTestModel(t)
	x, y := m.ambientPetPosition(m.width, m.height)
	m, _, _ = m.handlePetMouse(testMouseClick(tea.MouseLeft, x+2, y+2))
	m, _, _ = m.handlePetMouse(testMouseMotion(tea.MouseLeft, x+2, y-2))
	if m.petDragState != terminalpet.MoveRight {
		t.Fatalf("vertical drag state = %q, want running-right fallback", m.petDragState)
	}
}

func TestAmbientPetMouseLeavesOutsideClicksUntouched(t *testing.T) {
	m := interactivePetTestModel(t)
	if _, _, handled := m.handlePetMouse(testMouseClick(tea.MouseLeft, 0, 0)); handled {
		t.Fatal("click outside the pet should remain available to the normal mouse pipeline")
	}
}

func interactivePetTestModel(t *testing.T) model {
	t.Helper()
	m := mouseTestModel()
	m.width, m.height = 110, 34
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "hello"})
	frame := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	m.petAnimation, _ = terminalpet.ThumbnailAnimation(frame)
	m.petID = "boba"
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	return m
}

func TestPetsCommandExplainsUnsupportedTerminal(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.petClient = terminalpet.NewClient(t.TempDir())
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Reason: "Terminal companions need Kitty graphics or Sixel image support."})
	next, cmd := m.handlePetsCommand("")
	if cmd != nil {
		t.Fatal("unsupported terminal should not start a catalog request")
	}
	nextModel := next.(model)
	if nextModel.picker != nil {
		t.Fatal("unsupported terminal should not open the pet picker")
	}
	if got := plainRender(t, nextModel.View().Content); !strings.Contains(got, "Kitty graphics or Sixel") {
		t.Fatalf("unsupported-terminal guidance is missing: %q", got)
	}
}

func TestPetsCommandSelectsBuiltinCompanionByNameWithoutCatalog(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	m := newModel(context.Background(), Options{UserConfigPath: configPath})
	m.userConfigPath = configPath
	// An unreachable catalog: choosing a built-in must not need it.
	m.petClient.ManifestURL = "https://127.0.0.1:1/manifest"
	m.petClient.RankingURL = "https://127.0.0.1:1/ranking"

	updated, cmd := m.handlePetsCommand("  Twig ")
	next := updated.(model)
	if next.picker != nil || cmd == nil {
		t.Fatalf("a built-in name should select directly: picker=%v cmd=%v", next.picker, cmd)
	}
	installed, ok := cmd().(petInstalledMsg)
	if !ok || installed.err != nil || installed.animation == nil {
		t.Fatalf("install message = %#v", installed)
	}
	updated, _ = next.applyPetInstall(installed)
	next = updated.(model)
	if next.petID != "zero:twig" || next.petName != "Twig" || next.petAnimation == nil {
		t.Fatalf("selected pet: id=%q name=%q animation=%v", next.petID, next.petName, next.petAnimation)
	}
	saved, err := os.ReadFile(configPath)
	if err != nil || !strings.Contains(string(saved), `"zero:twig"`) {
		t.Fatalf("saved config = %q, %v", saved, err)
	}
	if _, err := os.Stat(filepath.Join(root, "pets")); !os.IsNotExist(err) {
		t.Fatalf("selecting a built-in companion wrote pet files: %v", err)
	}

	// The saved choice comes back on the next start, again with no catalog.
	restarted := newModel(context.Background(), Options{UserConfigPath: configPath, SavedPet: "zero:twig"})
	if restarted.petAnimation == nil || restarted.petName != "Twig" {
		t.Fatalf("restart did not restore the built-in companion: name=%q animation=%v", restarted.petName, restarted.petAnimation)
	}
}

// installTestPet writes the marker of an installed community pet.
func installTestPet(t *testing.T, root, slug, name string) {
	t.Helper()
	dir := filepath.Join(root, "pets", "installed", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := fmt.Sprintf(`{"slug":%q,"displayName":%q,"kind":"character","submittedBy":"tester","spriteVersionNumber":1}`, slug, name)
	if err := os.WriteFile(filepath.Join(dir, "source.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
}

// offlinePetModel has a pet client whose community catalog cannot be reached.
func offlinePetModel(t *testing.T, root string) model {
	t.Helper()
	m := newModel(context.Background(), Options{UserConfigPath: filepath.Join(root, "config.json")})
	m.width = 100
	m.petClient.ManifestURL = "https://127.0.0.1:1/manifest"
	m.petClient.RankingURL = "https://127.0.0.1:1/ranking"
	return m
}

func TestPetsCommandKeepsInstalledPetThatSharesABuiltinName(t *testing.T) {
	root := t.TempDir()
	installTestPet(t, root, "twig", "Twig the bird")
	m := offlinePetModel(t, root)

	// The installed pet answered to this name before built-ins existed.
	updated, _ := m.handlePetsCommand("twig")
	next := updated.(model)
	if next.picker == nil || next.petRequestedSlug != "twig" {
		t.Fatalf("/pets twig did not go to the installed pet: picker=%v requested=%q", next.picker, next.petRequestedSlug)
	}

	// The built-in stays reachable by its own slug, and by name once nothing
	// installed claims it.
	updated, cmd := m.handlePetsCommand("zero:twig")
	if next := updated.(model); next.picker != nil || cmd == nil {
		t.Fatalf("/pets zero:twig should select the built-in directly: picker=%v", next.picker)
	}
	if installed := cmd().(petInstalledMsg); installed.entry.Slug != "zero:twig" || !installed.entry.Builtin {
		t.Fatalf("selected %#v, want the built-in", installed.entry)
	}
	updated, cmd = offlinePetModel(t, t.TempDir()).handlePetsCommand("twig")
	if next := updated.(model); next.picker != nil || cmd == nil {
		t.Fatalf("/pets twig should select the built-in when no installed pet has that slug: picker=%v", next.picker)
	}
}

func TestPetsCommandReportsCatalogFailureForUnknownName(t *testing.T) {
	m := offlinePetModel(t, t.TempDir())
	updated, _ := m.handlePetsCommand("boba")
	next := updated.(model)
	entries, err := next.petClient.Catalog(context.Background())
	if err == nil {
		t.Fatal("the test catalog should be unreachable")
	}
	updated, _ = next.applyPetCatalog(petCatalogLoadedMsg{entries: entries, err: err})
	next = updated.(model)
	if next.picker != nil {
		t.Fatal("picker should close when the requested pet cannot be found")
	}
	plain := plainRender(t, next.View().Content)
	// "No pet named boba" would be a guess: the catalog that lists it never loaded.
	if !strings.Contains(plain, "Could not load the pet catalog") || strings.Contains(plain, "No pet named") {
		t.Fatalf("wrong explanation for an unreachable catalog:\n%s", plain)
	}

	// With the catalog loaded, an unknown name is simply unknown.
	m.picker = &commandPicker{kind: pickerPet, loading: true}
	m.petRequestedSlug = "boba"
	updated, _ = m.applyPetCatalog(petCatalogLoadedMsg{entries: terminalpet.BuiltinEntries()})
	if plain := plainRender(t, updated.(model).View().Content); !strings.Contains(plain, `No pet named "boba"`) {
		t.Fatalf("missing unknown-pet notice:\n%s", plain)
	}
}

func TestPetCatalogGroupsBuiltinCompanionsAndReportsMissingCommunityCatalog(t *testing.T) {
	root := t.TempDir()
	installTestPet(t, root, "local", "Local")
	m := offlinePetModel(t, root)
	m.picker = &commandPicker{kind: pickerPet, loading: true}
	// What the client really returns offline with a pet installed.
	entries, err := m.petClient.Catalog(context.Background())
	if err == nil {
		t.Fatal("the test catalog should be unreachable")
	}
	updated, _ := m.applyPetCatalog(petCatalogLoadedMsg{entries: entries, err: err})
	next := updated.(model)
	if next.picker == nil {
		t.Fatal("a failed community catalog should not close a picker that still has companions")
	}
	if first := next.picker.items[1]; first.Group != "Built in" || first.Label != "Twig" || first.Value != "zero:twig" {
		t.Fatalf("first companion = %#v, want built-in Twig", first)
	}
	if last := next.picker.items[len(next.picker.items)-1]; last.Group != "Installed" || last.Value != "local" {
		t.Fatalf("last companion = %#v, want the installed pet", last)
	}
	plain := plainRender(t, next.petPickerOverlay(next.width))
	for _, want := range []string{"Built in", "Twig", "Community catalog unavailable"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("picker is missing %q:\n%s", want, plain)
		}
	}

	// A later successful load clears the note.
	updated, _ = next.applyPetCatalog(petCatalogLoadedMsg{entries: entries})
	if plain := plainRender(t, updated.(model).petPickerOverlay(next.width)); strings.Contains(plain, "Community catalog unavailable") {
		t.Fatalf("stale catalog note:\n%s", plain)
	}
}

// textPetModel is a full-screen session on a terminal without image support
// but with true colour, showing built-in Twig.
func textPetModel(t *testing.T) model {
	t.Helper()
	m := newModel(context.Background(), Options{AltScreen: true, UserConfigPath: filepath.Join(t.TempDir(), "config.json")})
	m.width, m.height = 100, 30
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "hello"})
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Reason: "tmux does not pass terminal images through reliably."})
	m.petColorProfile = colorprofile.TrueColor
	m.petID, m.petName, m.petAnimation = "zero:twig", "Twig", terminalpet.BuiltinAnimation(petart.Twig)
	m.petOutcome = ""
	return m
}

func TestColorProfileMessageIsRecorded(t *testing.T) {
	m := newModel(context.Background(), Options{})
	updated, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.ANSI256})
	if got := updated.(model).petColorProfile; got != colorprofile.ANSI256 {
		t.Fatalf("petColorProfile = %v, want ANSI256", got)
	}
}

func TestTextPetDrawsInItsSlotWhereClicksLand(t *testing.T) {
	m := textPetModel(t)
	if !m.petTextMode() || !m.petLayoutActive() {
		t.Fatalf("text companion inactive: textMode=%v layout=%v", m.petTextMode(), m.petLayoutActive())
	}
	if m.petImageDraw("") != nil {
		t.Fatal("a text companion must not send an image")
	}
	if got := m.petComposerReservedColumns(m.width); got != terminalpet.TextColumns+petWrapGapColumns {
		t.Fatalf("composer reserves %d columns, want %d", got, terminalpet.TextColumns+petWrapGapColumns)
	}
	content := m.View().Content
	lines := strings.Split(plainRender(t, content), "\n")
	if len(lines) != m.height {
		t.Fatalf("view has %d lines, want the terminal height %d", len(lines), m.height)
	}
	x, y := m.ambientPetPosition(m.width, m.height)
	state, phase := m.petTextPlayback()
	want, ok := m.petAnimation.TextFrame(state, phase, terminalpet.TextTrueColor)
	if !ok {
		t.Fatal("no text frame")
	}
	for index, spriteLine := range want {
		row := []rune(lines[y+index])
		if len(row) < x+terminalpet.TextColumns {
			t.Fatalf("row %d is %d cells, too short for the companion at column %d:\n%s", y+index, len(row), x, strings.Join(lines, "\n"))
		}
		got := string(row[x : x+terminalpet.TextColumns])
		if wantPlain := ansi.Strip(spriteLine); got != wantPlain {
			t.Fatalf("row %d of the companion = %q, want %q:\n%s", index, got, wantPlain, strings.Join(lines, "\n"))
		}
		// Clicks on any of its cells reach it.
		if !m.petHit(x, y+index) || !m.petHit(x+terminalpet.TextColumns-1, y+index) {
			t.Fatalf("row %d of the companion does not take clicks", index)
		}
	}
	if m.petHit(x-1, y) || m.petHit(x, y-1) {
		t.Fatal("clicks outside the companion reach it")
	}
	if !strings.Contains(content, "\x1b[0;38;2;") {
		t.Fatal("a true-colour terminal did not get the art's exact colours")
	}
	// A 256-colour terminal gets the art's own 256-colour palette.
	m.petColorProfile = colorprofile.ANSI256
	if limited := m.View().Content; !strings.Contains(limited, "\x1b[0;38;5;") || strings.Contains(limited, "\x1b[0;38;2;") {
		t.Fatal("a 256-colour terminal did not get the 256-colour palette")
	}
}

func TestTextPetNeedsColourRoomAndABuiltin(t *testing.T) {
	sheet := image.NewNRGBA(image.Rect(0, 0, 12, 12))
	thumbnail, _ := terminalpet.ThumbnailAnimation(sheet)
	cases := []struct {
		name   string
		change func(*model)
		text   bool
		active bool
	}{
		{"true colour", func(*model) {}, true, true},
		{"256 colours", func(m *model) { m.petColorProfile = colorprofile.ANSI256 }, true, true},
		{"16 colours", func(m *model) { m.petColorProfile = colorprofile.ANSI }, false, false},
		{"no colour", func(m *model) { m.petColorProfile = colorprofile.ASCII }, false, false},
		{"profile not yet known", func(m *model) { m.petColorProfile = colorprofile.Unknown }, false, false},
		{"narrow terminal", func(m *model) { m.width = petTextMinWidth - 1 }, true, false},
		{"community pet", func(m *model) { m.petID, m.petAnimation = "boba", thumbnail }, true, false},
		{"image terminal", func(m *model) {
			m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
		}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := textPetModel(t)
			tc.change(&m)
			if got := m.petTextMode(); got != tc.text {
				t.Errorf("petTextMode = %v, want %v", got, tc.text)
			}
			if got := m.petLayoutActive(); got != tc.active {
				t.Errorf("petLayoutActive = %v, want %v", got, tc.active)
			}
		})
	}
	// An image terminal keeps the image slot.
	m := textPetModel(t)
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolSixel})
	if m.petSlotColumns() != petImageColumns || m.petSlotRows() != petImageRows {
		t.Fatalf("image slot is %dx%d", m.petSlotColumns(), m.petSlotRows())
	}
}

func TestPetsCommandOnTextTerminalOffersOnlyBuiltins(t *testing.T) {
	m := textPetModel(t)
	updated, _ := m.handlePetsCommand("")
	next := updated.(model)
	if next.picker == nil || next.picker.loading {
		t.Fatalf("picker = %#v, want the built-ins without a catalog fetch", next.picker)
	}
	want := []pickerItem{{Label: "No companion", Value: terminalpet.DisabledID, Meta: "off"}}
	for _, entry := range terminalpet.BuiltinEntries() {
		want = append(want, pickerItem{Group: "Built in", Label: entry.Label(), Value: entry.Slug, Local: true})
	}
	if !reflect.DeepEqual(next.picker.items, want) {
		t.Fatalf("text picker items = %#v", next.picker.items)
	}
	if plain := plainRender(t, next.petPickerOverlay(next.width)); !strings.Contains(plain, "Community pets need images") {
		t.Fatalf("text picker does not say why community pets are missing:\n%s", plain)
	}

	// A built-in by name is chosen directly.
	updated, cmd := m.handlePetsCommand("ember")
	if next := updated.(model); next.picker != nil || cmd == nil {
		t.Fatalf("/pets ember did not select the built-in: picker=%v", next.picker)
	}
	if installed := cmd().(petInstalledMsg); installed.entry.Slug != "zero:ember" || installed.err != nil {
		t.Fatalf("installed %#v, %v", installed.entry, installed.err)
	}

	// A community pet cannot be drawn here, and says so.
	updated, cmd = m.handlePetsCommand("boba")
	next = updated.(model)
	if cmd != nil || next.picker != nil {
		t.Fatal("/pets boba should not open a picker or fetch on a text terminal")
	}
	if plain := plainRender(t, next.View().Content); !strings.Contains(plain, `"boba" needs terminal images. Run /pets for the built-in companions.`) {
		t.Fatalf("missing explanation for a community pet on a text terminal:\n%s", plain)
	}
}

func TestPetsCommandExplainsWhenNeitherImagesNorTextWork(t *testing.T) {
	m := textPetModel(t)
	m.petColorProfile = colorprofile.ANSI
	updated, cmd := m.handlePetsCommand("")
	next := updated.(model)
	if cmd != nil || next.picker != nil {
		t.Fatal("a 16-colour terminal without images should not open the picker")
	}
	plain := plainRender(t, next.View().Content)
	if !strings.Contains(plain, "tmux does not pass terminal images through reliably. Text companions need 256 colours.") {
		t.Fatalf("missing explanation:\n%s", plain)
	}
}

func TestTextPetPickerDrawsThePreview(t *testing.T) {
	for _, width := range []int{100, 50} {
		m := textPetModel(t)
		m.width = width
		m.petID = "zero:twig" // the picker opens with Twig selected
		updated, debounce := m.handlePetsCommand("")
		// Let the real preview load: the debounce, then the preview itself.
		updated, load := updated.(model).Update(execCmd(debounce))
		updated, _ = updated.(model).Update(execCmd(load))
		next := updated.(model)
		if next.petPreview == nil || next.petPreviewLoading {
			t.Fatalf("width %d: preview did not load: preview=%v loading=%v", width, next.petPreview, next.petPreviewLoading)
		}
		plain := plainRender(t, next.petPickerOverlay(next.width))
		preview, _ := next.petPreview.TextFrame(terminalpet.Idle, next.petPhase, next.petTextColors())
		// Every row of the drawing appears whole, on consecutive lines, all
		// starting in the same column.
		plainLines := strings.Split(plain, "\n")
		first, column := -1, -1
		for index, line := range preview {
			want := ansi.Strip(line)
			if first < 0 {
				for row, candidate := range plainLines {
					if at := strings.Index(candidate, want); at >= 0 && strings.TrimSpace(want) != "" {
						first, column = row-index, len([]rune(candidate[:at]))
						break
					}
				}
				if first < 0 {
					continue
				}
			}
			row := []rune(plainLines[first+index])
			if len(row) < column+len([]rune(want)) || string(row[column:column+len([]rune(want))]) != want {
				t.Fatalf("width %d: preview row %d %q is not in line with the rest:\n%s", width, index, want, plain)
			}
		}
		if first < 0 {
			t.Fatalf("width %d: preview not drawn:\n%s", width, plain)
		}
	}
}

func TestPetMoodsFollowTheAgent(t *testing.T) {
	thumbnail, _ := terminalpet.ThumbnailAnimation(image.NewNRGBA(image.Rect(0, 0, 12, 12)))
	running := func(m model) model {
		m.pending = true
		m.petOutcome = ""
		return m
	}
	m := running(textPetModel(t))
	if got := m.petState(); got != terminalpet.Thinking {
		t.Fatalf("a turn with nothing streaming yet = %s, want thinking", got)
	}
	m.streamingText = []byte("Here is the plan")
	if got := m.petState(); got != terminalpet.Running {
		t.Fatalf("a turn writing its answer = %s, want running", got)
	}
	m.streamingText = []byte("  \n")
	if got := m.petState(); got != terminalpet.Thinking {
		t.Fatalf("whitespace is not an answer: %s, want thinking", got)
	}
	m.streamingText = nil
	m.streamCallName = "bash"
	if got := m.petState(); got != terminalpet.Running {
		t.Fatalf("a turn streaming a tool call = %s, want running", got)
	}
	// A tool that was called and has no result yet is still running.
	m.streamCallName = ""
	m.activeRunID = 7
	m.transcript = append(m.transcript, transcriptRow{kind: rowToolCall, runID: 7, id: "call-1", tool: "bash"})
	if got := m.petState(); got != terminalpet.Running {
		t.Fatalf("a turn running a tool = %s, want running", got)
	}
	m.transcript = append(m.transcript, transcriptRow{kind: rowToolResult, runID: 7, id: "call-1", tool: "bash"})
	if got := m.petState(); got != terminalpet.Thinking {
		t.Fatalf("after the tool finished = %s, want thinking", got)
	}
	// Pets without a thinking animation keep the running one throughout.
	m = running(textPetModel(t))
	m.petAnimation = thumbnail
	if got := m.petState(); got != terminalpet.Running {
		t.Fatalf("a community pet while thinking = %s, want running", got)
	}
}

func TestPetFallsAsleepWhenIdleAndWakesUp(t *testing.T) {
	base := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	now := base
	m := textPetModel(t)
	m.now = func() time.Time { return now }
	m.petLastActiveAt = base
	now = base.Add(petSleepAfter - time.Second)
	if got := m.petState(); got != terminalpet.Idle {
		t.Fatalf("just before the timeout = %s, want idle", got)
	}
	now = base.Add(petSleepAfter)
	if got := m.petState(); got != terminalpet.Sleeping {
		t.Fatalf("after %s idle = %s, want sleeping", petSleepAfter, got)
	}
	// Pets without a sleeping animation stay idle.
	community := m
	community.petAnimation, _ = terminalpet.ThumbnailAnimation(image.NewNRGBA(image.Rect(0, 0, 12, 12)))
	if got := community.petState(); got != terminalpet.Idle {
		t.Fatalf("a community pet asleep = %s, want idle", got)
	}
	// A key press wakes it.
	updated, _ := m.Update(testKey('a'))
	if got := updated.(model).petState(); got != terminalpet.Idle {
		t.Fatalf("after a key press = %s, want idle", got)
	}
	// So does a click on it: the reaction it starts counts as activity.
	clicked := m
	clicked.petOutcome, clicked.petOutcomeAt = terminalpet.Waving, now
	now = now.Add(10 * time.Second)
	if got := clicked.petState(); got != terminalpet.Idle {
		t.Fatalf("after a click = %s, want idle", got)
	}
	// A running turn is never asleep.
	busy := m
	busy.pending = true
	if got := busy.petState(); got == terminalpet.Sleeping {
		t.Fatal("asleep during a turn")
	}
}

func TestPetCelebratesLongTurnsAndHopsAfterShortOnes(t *testing.T) {
	m := newModel(context.Background(), Options{})
	start := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	m.turnStartedAt = start
	m.turnTimer = newActiveTurnTimer(start)
	if got := m.petTurnOutcome(nil, start.Add(petBigTurn)); got != terminalpet.Jumping {
		t.Errorf("a %s turn = %s, want jumping", petBigTurn, got)
	}
	if got := m.petTurnOutcome(nil, start.Add(petBigTurn-time.Second)); got != terminalpet.Review {
		t.Errorf("a short turn = %s, want review", got)
	}
	if got := m.petTurnOutcome(errors.New("boom"), start.Add(2*petBigTurn)); got != terminalpet.Failed {
		t.Errorf("a failed long turn = %s, want failed", got)
	}
	m.turnStartedAt, m.turnTimer = time.Time{}, nil
	if got := m.petTurnOutcome(nil, start.Add(time.Hour)); got != terminalpet.Review {
		t.Errorf("a turn with no start time = %s, want review", got)
	}
}

func TestFinishedTurnSetsThePetReaction(t *testing.T) {
	for _, tc := range []struct {
		name     string
		duration time.Duration
		want     terminalpet.State
	}{
		{"short", 5 * time.Second, terminalpet.Review},
		{"long", 90 * time.Second, terminalpet.Jumping},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			provider := &fakeProvider{events: []zeroruntime.StreamEvent{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			}}
			m := newModel(context.Background(), Options{
				Cwd: t.TempDir(), ProviderName: "tokenrouter", ModelName: "MiniMax-M3",
				Provider: provider, Registry: tools.NewRegistry(),
			})
			base := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
			current := base
			m.now = func() time.Time { return current }
			m.input.SetValue("hello")
			// The turn starts and runs at base; it finishes tc.duration later.
			updated, cmd := m.Update(testKey(tea.KeyEnter))
			finished := execCmd(cmd)
			current = base.Add(tc.duration)
			updated, _ = updated.(model).Update(finished)
			next := updated.(model)
			if next.pending {
				t.Fatal("the turn did not finish")
			}
			if next.petOutcome != tc.want {
				t.Fatalf("pet reaction = %s, want %s", next.petOutcome, tc.want)
			}
			if !next.petLastActiveAt.Equal(base.Add(tc.duration)) {
				t.Fatalf("petLastActiveAt = %s, want the turn's end", next.petLastActiveAt)
			}
		})
	}
}

func TestPetGreetsWhenItAppears(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	started := newModel(context.Background(), Options{UserConfigPath: configPath, SavedPet: "zero:merlin"})
	// Look a moment after start, on a fixed clock, so the check cannot race
	// the wave's end.
	greetedAt := started.petOutcomeAt
	started.now = func() time.Time { return greetedAt.Add(time.Second) }
	if started.petAnimation == nil || started.petState() != terminalpet.Waving {
		t.Fatalf("a saved companion should wave on start: state=%s", started.petState())
	}
	if none := newModel(context.Background(), Options{UserConfigPath: configPath}); none.petOutcome != "" {
		t.Fatalf("no companion, yet a reaction is set: %s", none.petOutcome)
	}

	m := textPetModel(t)
	updated, cmd := m.handlePetsCommand("kage")
	updated, _ = updated.(model).applyPetInstall(cmd().(petInstalledMsg))
	chosen := updated.(model)
	chosenAt := chosen.petOutcomeAt
	chosen.now = func() time.Time { return chosenAt.Add(time.Second) }
	if got := chosen.petState(); got != terminalpet.Waving {
		t.Fatalf("a newly chosen companion = %s, want waving", got)
	}
}

func TestTextPetHidesNoTranscriptOrFooterText(t *testing.T) {
	answer := "The last line of the answer runs nearly the width of the screen and ends with a word: lambda."
	build := func(withPet bool) model {
		m := textPetModel(t)
		// Enough conversation to fill the screen, so the answer's last line
		// sits at the bottom of the transcript, right above the footer.
		for index := 0; index < 30; index++ {
			m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: fmt.Sprintf("question %d", index)})
		}
		m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowAssistant, text: answer})
		m.copyStatus = "Copied 12 lines"
		if !withPet {
			m.petID, m.petAnimation = terminalpet.DisabledID, nil
		}
		return m
	}
	plain := plainRender(t, build(true).View().Content)
	without := plainRender(t, build(false).View().Content)
	for _, want := range []string{"ends with a word: lambda.", "Copied 12 lines"} {
		if !strings.Contains(without, want) {
			t.Fatalf("test setup: %q is not on screen even without a companion:\n%s", want, without)
		}
		if !strings.Contains(plain, want) {
			t.Fatalf("the text companion hides %q:\n%s", want, plain)
		}
	}

	// The companion stands only beside footer rows: the footer grows to its
	// height, for every caller that measures it, and the transcript ends above.
	m := build(true)
	footer := m.footerView(chatWidth(m.width))
	if got, want := strings.Count(footer, "\n")+1, terminalpet.TextRows+1; got != want {
		t.Fatalf("footer with a docked text companion is %d rows, want %d", got, want)
	}
	plainFooter := strings.Split(ansi.Strip(footer), "\n")
	reserved := m.petComposerReservedColumns(m.width)
	for index, line := range plainFooter[:len(plainFooter)-1] {
		if width := len([]rune(strings.TrimRight(line, " "))); width > m.width-reserved {
			t.Fatalf("footer row %d runs under the companion (%d cells, %d free): %q", index, width, m.width-reserved, line)
		}
	}
	// The status line is below the companion and keeps its full width.
	if got := m.footerStatusLine(m.width); lipgloss.Width(got) != lipgloss.Width(m.statusLine(m.width)) || got != m.statusLine(m.width) {
		t.Fatalf("status line was shortened beside a text companion: %q", ansi.Strip(got))
	}
	plainBuild := build(false)
	if got := strings.Count(plainBuild.footerView(chatWidth(plainBuild.width)), "\n") + 1; got >= terminalpet.TextRows+1 {
		t.Fatalf("test setup: the footer is %d rows without a companion, so nothing was added", got)
	}
}

func TestTextPetStepsAsideForPermissionPrompts(t *testing.T) {
	m := textPetModel(t)
	m.pendingPermission = &pendingPermissionPrompt{}
	if m.petLayoutActive() {
		t.Fatal("a text companion would cover the permission card's options")
	}
	// Image companions are drawn over the grid by the terminal and keep their
	// behaviour.
	m.petRenderer = terminalpet.NewImageRenderer(terminalpet.ImageSupport{Protocol: terminalpet.ImageProtocolKitty})
	if !m.petLayoutActive() {
		t.Fatal("an image companion should stay beside a permission prompt")
	}
}

func TestTextPetPickerDoesNotStartOnNoCompanion(t *testing.T) {
	m := textPetModel(t)
	m.petID = "boba" // a community pet, which this picker cannot list
	updated, _ := m.handlePetsCommand("")
	current, ok := updated.(model).picker.current()
	if !ok || current.Value == terminalpet.DisabledID {
		t.Fatalf("picker starts on %#v: Enter would switch the pet off", current)
	}
	m.petID = terminalpet.DisabledID
	updated, _ = m.handlePetsCommand("")
	if current, _ := updated.(model).picker.current(); current.Value != terminalpet.DisabledID {
		t.Fatalf("with no pet, the picker starts on %#v, want No companion", current)
	}
}

func TestTextPetChosenInANarrowWindowSaysWhenItShows(t *testing.T) {
	m := textPetModel(t)
	m.width = petTextMinWidth - 5
	updated, cmd := m.handlePetsCommand("nova")
	updated, _ = updated.(model).applyPetInstall(cmd().(petInstalledMsg))
	next := updated.(model)
	if next.petLayoutActive() {
		t.Fatal("test setup: the window is wide enough to show the companion")
	}
	if plain := plainRender(t, next.View().Content); !strings.Contains(plain, fmt.Sprintf("Nova shows once the window is %d+ columns wide.", petTextMinWidth)) {
		t.Fatalf("no explanation for the missing companion:\n%s", plain)
	}
}

func TestComposerClicksAndArrowsFollowTheRowsBesideACompanion(t *testing.T) {
	m := textPetModel(t)
	m.width = 80
	words := make([]string, 0, 50)
	for index := 0; index < 50; index++ {
		words = append(words, fmt.Sprintf("w%02d", index))
	}
	text := strings.Join(words, " ")
	m.setComposerState(composerState{text: text, cursor: 0})
	if !m.petLayoutActive() || m.petComposerReservedColumns(m.width) == 0 {
		t.Fatal("test setup: no docked companion beside the composer")
	}
	// The composer must wrap differently with and without the companion's
	// columns, or a wrong width would map clicks the same way.
	full := composerWrappedVisualLines(m.input, m.currentComposerState(), maxInt(1, m.width-4))
	narrow := composerWrappedVisualLines(m.input, m.currentComposerState(), m.composerInnerWidth(m.width))
	if len(full) == len(narrow) {
		t.Fatalf("test setup: %d rows at both widths", len(full))
	}

	lines := strings.Split(plainRender(t, m.View().Content), "\n")
	width := m.chatColumnWidth()
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	// The composer's second text row, below its top border and first row. It
	// is drawn from the narrow wrap: it starts where that wrap's second
	// segment starts.
	row := frame.composerRect.y + 2
	start := narrow[1].start
	opening := text[start : start+6]
	column := strings.Index(lines[row], opening)
	if column < 0 {
		t.Fatalf("test setup: composer row %q does not start with %q", lines[row], opening)
	}
	column = len([]rune(lines[row][:column]))

	// A click on the row's first character puts the cursor there.
	position, ok := m.composerPositionAtMouse(testMouseClick(tea.MouseLeft, column, row))
	if !ok || position != start {
		t.Fatalf("click on %q at column %d gave cursor %d (%v), want %d", opening, column, position, ok, start)
	}

	// Down from the start of the text moves to the start of the drawn second row.
	moved, ok := m.moveComposerVisualCursor(1)
	if !ok || moved.currentComposerState().cursor != start {
		t.Fatalf("Down moved the cursor to %d (%v), want %d, the start of the second row", moved.currentComposerState().cursor, ok, start)
	}
}
