package tui

import (
	"bytes"
	"context"
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
	"github.com/Gitlawb/zero/internal/terminalpet"
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
	want := []pickerItem{
		{Label: "No companion", Value: terminalpet.DisabledID, Meta: "off"},
		{Group: "Installed", Label: "Kratos Greek", Value: "kratos", Local: true},
	}
	if !reflect.DeepEqual(next.picker.items, want) {
		t.Fatalf("initial pet picker items = %#v, want %#v", next.picker.items, want)
	}
	if current, ok := next.picker.current(); !ok || current.Value != "kratos" {
		t.Fatalf("initial pet picker selection = %#v, %v; want installed current pet", current, ok)
	}
	plain := plainRender(t, next.petPickerOverlay(next.width))
	for _, wantText := range []string{"No companion", "Installed", "Kratos Greek", "Discover", "Fetching companions"} {
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
