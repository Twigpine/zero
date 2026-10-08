package tui

import "strings"

// theme_palettes.go is the sole home of raw color hex in the TUI: every UI and
// code palette literal and the ordered theme registry live here, so theme.go stays hex-free
// (just the palette struct, buildTheme, and the resolved tuiTheme). Adding a theme
// is a new palette{...} literal plus one themeRegistry entry — nothing else.
//
// Contrast discipline: every palette must satisfy the same WCAG invariants the
// built-ins do (asserted across the whole registry in theme_select_test.go): ink
// and faintest ≥ AA on panel; onAccent ≥ AA on accent; addInk/delInk readable on
// their diff bands and word spans; the muted>faint>faintest ramp monotonic toward
// the surface; and selBg visibly separated from panel while its label stays legible.

type systemSurfacePalette struct {
	selection string
	add       string
	del       string
	addWord   string
	delWord   string
}

var darkSystemSurfaces = systemSurfacePalette{
	selection: "#2b2f2d",
	add:       "#212922",
	del:       "#3c170f",
	addWord:   "#212922",
	delWord:   "#3c170f",
}

var lightSystemSurfaces = systemSurfacePalette{
	selection: "#e7e9e7",
	add:       "#dafbe1",
	del:       "#ffebe9",
	addWord:   "#aceebb",
	delWord:   "#ffcecb",
}

// darkPalette is the original Lime palette: a near-black chat surface with one
// lime accent. bg (#070708) is the terminal's own canvas — deliberately never
// painted — so no token references it.
var darkPalette = palette{
	panel:     "#0e0e10",
	promptBg:  "#262626",
	line:      "#242429",
	line2:     "#414147",
	ink:       "#ececee",
	muted:     "#9a9aa2", // secondary text — lifted so it clearly out-ranks faint
	faint:     "#8a8a92", // hints/metadata — nudged up to separate from faintest
	faintest:  "#7c7c82", // line numbers/separators — pinned at the WCAG-AA floor on the dark panel
	accent:    "#caff3f", // original bright lime (the brand accent)
	green:     "#5dd1a4",
	red:       "#ff7a7a",
	amber:     "#ffc25c",
	blue:      "#7db4ff",
	gitAdd:    "#7db87a",
	gitDel:    "#b87a7a",
	addBg:     "#18352c",
	delBg:     "#241819",
	addBgWord: "#2e654d", // changed span within an added line — brighter green (sep 1.83 vs addBg, addInk 7.1:1)
	delBgWord: "#502d30", // changed span within a deleted line — brighter red (sep 1.44 vs delBg, delInk 7.7:1)
	permBg:    "#1c1915",
	selBg:     "#32401b", // selected row bg — brightened from #1d2114 so the highlighted row separates from the panel (sep 1.18→1.73) while ink label contrast stays ~9.4:1
	addInk:    "#bdeed7",
	delInk:    "#f2c4c4",
	onAccent:  "#000000",
	cardRun:   "#5a6b2e",
	cardErr:   "#6b3434",
	cardPerm:  "#6b5a2e",
}

// draculaPalette — the Dracula scheme (dracula.com): muted-violet surface, purple
// accent, high-chroma pink/green/cyan signals.
var draculaPalette = palette{
	panel:     "#282a36",
	promptBg:  "#383c4d",
	line:      "#363a4b",
	line2:     "#484c62",
	ink:       "#f8f8f2",
	muted:     "#b9bccb",
	faint:     "#a2a5b8",
	faintest:  "#9195ac",
	accent:    "#bd93f9",
	green:     "#50fa7b",
	red:       "#ff5555",
	amber:     "#ffb86c",
	blue:      "#8be9fd",
	gitAdd:    "#77c58c",
	gitDel:    "#d98d8d",
	addBg:     "#1c3b2a",
	delBg:     "#3a2026",
	addBgWord: "#235035",
	delBgWord: "#5e333b",
	permBg:    "#322a1e",
	selBg:     "#504482",
	addInk:    "#cbf2dd",
	delInk:    "#f4c9c9",
	onAccent:  "#000000",
	cardRun:   "#7c6aa6",
	cardErr:   "#98505a",
	cardPerm:  "#9a7c62",
}

// nordPalette — the Nord scheme (nordtheme.com): cool polar-night slate with a
// frost-blue accent and desaturated aurora signals.
var nordPalette = palette{
	panel:     "#3b4252",
	promptBg:  "#464f62",
	line:      "#434c5e",
	line2:     "#4c566a",
	ink:       "#eceff4",
	muted:     "#c8cfda",
	faint:     "#b4bdcb",
	faintest:  "#a5afc1",
	accent:    "#88c0d0",
	green:     "#a3be8c",
	red:       "#bf616a",
	amber:     "#d08770",
	blue:      "#81a1c1",
	gitAdd:    "#8ba077",
	gitDel:    "#b0757d",
	addBg:     "#37433a",
	delBg:     "#45383d",
	addBgWord: "#456d46",
	delBgWord: "#6d4650",
	permBg:    "#47413a",
	selBg:     "#40688a",
	addInk:    "#d6ecca",
	delInk:    "#f0c6cb",
	onAccent:  "#000000",
	cardRun:   "#4c6672",
	cardErr:   "#6b4a51",
	cardPerm:  "#6a564d",
}

// gruvboxPalette — Gruvbox dark (medium): warm retro browns with an olive-green
// accent and high-contrast cream ink.
var gruvboxPalette = palette{
	panel:     "#32302f",
	promptBg:  "#3c3836",
	line:      "#504945",
	line2:     "#665c54",
	ink:       "#ebdbb2",
	muted:     "#c9b99a",
	faint:     "#b7a78d",
	faintest:  "#a89984",
	accent:    "#8ec07c",
	green:     "#b8bb26",
	red:       "#fb4934",
	amber:     "#fabd2f",
	blue:      "#83a598",
	gitAdd:    "#98971a",
	gitDel:    "#cc241d",
	addBg:     "#2f3a29",
	delBg:     "#3b2b29",
	addBgWord: "#3e5236",
	delBgWord: "#593733",
	permBg:    "#38331e",
	selBg:     "#3d4e30",
	addInk:    "#c5e6b0",
	delInk:    "#f0c0bb",
	onAccent:  "#000000",
	cardRun:   "#6f8460",
	cardErr:   "#a5493d",
	cardPerm:  "#a5833a",
}

// tokyoNightPalette — Tokyo Night (storm): deep indigo surface, soft blue accent,
// cool high-contrast ink.
var tokyoNightPalette = palette{
	panel:     "#1e2030",
	promptBg:  "#2c3149",
	line:      "#262a3d",
	line2:     "#3b4261",
	ink:       "#c8d3f5",
	muted:     "#a9b1d0",
	faint:     "#9099b2",
	faintest:  "#838ba8",
	accent:    "#82aaff",
	green:     "#c3e88d",
	red:       "#ff757f",
	amber:     "#ffc777",
	blue:      "#86e1fc",
	gitAdd:    "#96bf7d",
	gitDel:    "#c77e85",
	addBg:     "#20303b",
	delBg:     "#37222c",
	addBgWord: "#2b5a4a",
	delBgWord: "#5c2e3a",
	permBg:    "#2a2419",
	selBg:     "#2a385b",
	addInk:    "#b8e4d3",
	delInk:    "#f3c4cb",
	onAccent:  "#000000",
	cardRun:   "#4b5d8b",
	cardErr:   "#7d4857",
	cardPerm:  "#7d6954",
}

// catppuccinPalette — Catppuccin Mocha: soft lavender surface with a mauve accent
// and pastel signals.
var catppuccinPalette = palette{
	panel:     "#1e1e2e",
	promptBg:  "#34364b",
	line:      "#313244",
	line2:     "#45475a",
	ink:       "#cdd6f4",
	muted:     "#a6adc8",
	faint:     "#9399b2",
	faintest:  "#83889f",
	accent:    "#cba6f7",
	green:     "#a6e3a1",
	red:       "#f38ba8",
	amber:     "#f9e2af",
	blue:      "#89b4fa",
	gitAdd:    "#8cbf8a",
	gitDel:    "#cc8a9b",
	addBg:     "#24312b",
	delBg:     "#3c2a32",
	addBgWord: "#2f5140",
	delBgWord: "#56333f",
	permBg:    "#29261b",
	selBg:     "#322e46",
	addInk:    "#c4ecd6",
	delInk:    "#f4cdd6",
	onAccent:  "#000000",
	cardRun:   "#7a6a99",
	cardErr:   "#8a5b72",
	cardPerm:  "#8d8274",
}

// oneDarkPalette — Atom One Dark: slate-gray surface with a blue accent.
var oneDarkPalette = palette{
	panel:     "#2e323b",
	promptBg:  "#3a3f4a",
	line:      "#393f4a",
	line2:     "#4b525f",
	ink:       "#abb2bf",
	muted:     "#a2a9b6",
	faint:     "#9aa1af",
	faintest:  "#969cab",
	accent:    "#61afef",
	green:     "#98c379",
	red:       "#e06c75",
	amber:     "#e5c07b",
	blue:      "#56b6c2",
	gitAdd:    "#82a06a",
	gitDel:    "#bd8087",
	addBg:     "#2c382b",
	delBg:     "#3a2d2f",
	addBgWord: "#3d5a3a",
	delBgWord: "#5c3e40",
	permBg:    "#3c3826",
	selBg:     "#354256",
	addInk:    "#cdeab3",
	delInk:    "#f0c3c7",
	onAccent:  "#000000",
	cardRun:   "#496c8c",
	cardErr:   "#7c515b",
	cardPerm:  "#7e735e",
}

// solarizedDarkPalette — Solarized Dark (Ethan Schoonover): the signature teal
// base03/base02 surface with a cyan accent and the fixed accent wheel.
var solarizedDarkPalette = palette{
	panel:     "#073642",
	promptBg:  "#0b3b46",
	line:      "#123f48",
	line2:     "#4b636c",
	ink:       "#cdd6d6",
	muted:     "#a9b3b3",
	faint:     "#9ba5a5",
	faintest:  "#929c9c",
	accent:    "#3bb3a6", // solarized cyan, brightened to clear AA on the lifted base02 panel
	green:     "#859900",
	red:       "#dc322f",
	amber:     "#b58900",
	blue:      "#268bd2",
	gitAdd:    "#93a05e",
	gitDel:    "#c67b71",
	addBg:     "#123f31",
	delBg:     "#45302e",
	addBgWord: "#1e5c44",
	delBgWord: "#6a3f3a",
	permBg:    "#2e2a18",
	selBg:     "#17505a",
	addInk:    "#c3ecd6",
	delInk:    "#f2cbc6",
	onAccent:  "#000000",
	cardRun:   "#1d6b6c",
	cardErr:   "#6d393d",
	cardPerm:  "#5b6028",
}

// rosePinePalette — Rosé Pine (main): muted rose-quartz base with a soft-rose
// accent and cool pine/foam signals.
var rosePinePalette = palette{
	panel:     "#1f1d2e",
	promptBg:  "#2f2b47",
	line:      "#2b2840",
	line2:     "#403d52",
	ink:       "#e0def4",
	muted:     "#a8a3c0",
	faint:     "#928ea9",
	faintest:  "#8985a0",
	accent:    "#ebbcba",
	green:     "#31748f",
	red:       "#eb6f92",
	amber:     "#f6c177",
	blue:      "#9ccfd8",
	gitAdd:    "#5e88a2",
	gitDel:    "#d589a7",
	addBg:     "#1f2d3a",
	delBg:     "#3a1f2d",
	addBgWord: "#274b5e",
	delBgWord: "#763a4f",
	permBg:    "#4a3e3d",
	selBg:     "#44415a",
	addInk:    "#cfe8ef",
	delInk:    "#f6c9cd",
	onAccent:  "#000000",
	cardRun:   "#7c6673",
	cardErr:   "#7c4662",
	cardPerm:  "#806857",
}

// everforestPalette — Everforest dark (medium): warm forest-gray surface with a
// sage-green accent.
var everforestPalette = palette{
	panel:     "#333c43",
	promptBg:  "#3d484d",
	line:      "#414b52",
	line2:     "#55636b",
	ink:       "#d3c6aa",
	muted:     "#b0bab0",
	faint:     "#a4aea3",
	faintest:  "#9ca99b",
	accent:    "#a7c080",
	green:     "#83c092",
	red:       "#e67e80",
	amber:     "#dbbc7f",
	blue:      "#7fbbb3",
	gitAdd:    "#8faa78",
	gitDel:    "#c08888",
	addBg:     "#2c3f37",
	delBg:     "#3e3234",
	addBgWord: "#324e3b",
	delBgWord: "#573a3c",
	permBg:    "#3c382d",
	selBg:     "#3b482e",
	addInk:    "#cfead0",
	delInk:    "#f4cfcd",
	onAccent:  "#000000",
	cardRun:   "#798b6b",
	cardErr:   "#9c676b",
	cardPerm:  "#96896b",
}

// neonPalette is a neon-on-black color scheme: pitch-black surface with
// neon green ink and a cyan accent.
var neonPalette = palette{
	panel:     "#050b06",
	promptBg:  "#0c180d",
	line:      "#1c3820",
	line2:     "#2c5230",
	ink:       "#c9ffd2",
	muted:     "#80db8f",
	faint:     "#6eca7d",
	faintest:  "#74c468", // brightened from #58af69 so line numbers quantize to #87d75f and stay AA on the xterm-green addBg (#005f00); still dimmer than faint, keeping the ramp monotonic
	accent:    "#00e5c8",
	green:     "#39ff6a",
	red:       "#ff4d6d",
	amber:     "#f4ff3a",
	blue:      "#22e0ff",
	gitAdd:    "#4fdc6a",
	gitDel:    "#ff6f80",
	addBg:     "#083c10", // quantizes to xterm green #005f00 instead of the same gray as delBg, keeping add/del rows distinct on 256-color terminals
	delBg:     "#3c0810", // quantizes to xterm red #5f0000 (see addBg)
	addBgWord: "#147828", // quantizes to xterm green #008700, distinct from both addBg's #005f00 and delBgWord's red
	delBgWord: "#74202e", // quantizes to xterm red #870000 (see addBgWord)
	permBg:    "#2a2a0c",
	selBg:     "#123a1e",
	addInk:    "#ecffdc", // quantizes to #ffffd7, which keeps AA on addBgWord's xterm #008700 (the old #c8ffcf quantized to #d7ffd7 at 4.29:1)
	delInk:    "#ffd0d6",
	onAccent:  "#001410",
	cardRun:   "#1f8a6e",
	cardErr:   "#9a4042", // raised from #8a2f42 for the 3:1 non-text border threshold against the panel (2.43:1 before), holding after xterm-256 quantization too
	cardPerm:  "#8a8a1f",
}

// lightPalette is dark-on-light: a warm cream surface (so cards lift off the
// terminal page, which Zero never paints) with near-black ink and an olive-lime
// accent that keeps the brand identity while clearing AA on the light panel. The
// muted/faint/faintest grays get progressively LIGHTER toward the surface; diff and
// permission tints are light surfaces. Replaces the old flat cool-gray light set
// whose muddy panel, sub-AA diff spans, and near-invisible selBg read as broken.
var lightPalette = palette{
	panel:     "#efebd4",
	promptBg:  "#e3ddc2",
	line:      "#d8d2bd",
	line2:     "#b7b199",
	ink:       "#22201a",
	muted:     "#4b5149",
	faint:     "#575e55",
	faintest:  "#636a61",
	accent:    "#54700a",
	green:     "#1e725c",
	red:       "#c02434",
	amber:     "#8a5f00",
	blue:      "#1f66c0",
	gitAdd:    "#3d6f46",
	gitDel:    "#a34a4a",
	addBg:     "#ddf0df",
	delBg:     "#f8dcdc",
	addBgWord: "#a2daae",
	delBgWord: "#f2b6b6",
	permBg:    "#f7ebc6",
	selBg:     "#d4e08f",
	addInk:    "#0c4026",
	delInk:    "#641a1d",
	onAccent:  "#ffffff",
	cardRun:   "#b0be7e",
	cardErr:   "#d8b0a8",
	cardPerm:  "#d6c496",
}

// solarizedLightPalette — Solarized Light: the base3/base2 cream surface with the
// same fixed accent wheel as Solarized Dark, dark-on-light.
var solarizedLightPalette = palette{
	panel:     "#eee8d5",
	promptBg:  "#e1d9be",
	line:      "#d8d1bc",
	line2:     "#c0b89e",
	ink:       "#304049",
	muted:     "#495b61",
	faint:     "#506469",
	faintest:  "#576b72",
	accent:    "#0c665c", // solarized cyan, darkened for AA on the cream panel (with white onAccent)
	green:     "#859900",
	red:       "#dc322f",
	amber:     "#7a5c00", // solarized yellow, darkened so white onAccent works on amber fills
	blue:      "#268bd2",
	gitAdd:    "#788d34",
	gitDel:    "#ac4f50",
	addBg:     "#dde8c6",
	delBg:     "#f1ddd2",
	addBgWord: "#b9d488", // deepened so the changed span separates from addBg (sep 1.28)
	delBgWord: "#edc4b4",
	permBg:    "#f0e6bd",
	selBg:     "#a6d6c4",
	addInk:    "#38480a",
	delInk:    "#6f1614",
	onAccent:  "#ffffff", // white — accent and amber are both dark on this light theme
	cardRun:   "#7fbaaf",
	cardErr:   "#d8837a",
	cardPerm:  "#c4ae63",
}

// dunePalette is a warm sand-and-cream color scheme: sand/cream surface,
// charcoal ink, and a soft amber accent.
var dunePalette = palette{
	panel:     "#f2e9d8",
	promptBg:  "#e9dcbf",
	line:      "#d9c7a3",
	line2:     "#c2a97c",
	ink:       "#2b241a",
	muted:     "#473e32",
	faint:     "#554a3a",
	faintest:  "#655648",
	accent:    "#724028", // darkened from #8f5215 for AA on selBg (5.46:1) that also survives ANSI-256 downsampling (quantizes to #444444, 6.47:1 on quantized selBg; the previous #7c4712 quantized to #875f00 at 3.81:1)
	green:     "#38572a",
	red:       "#872d24", // darkened from #963328 so delBg contrast survives ANSI-256 downsampling (true 6.57:1, 256 7.86:1)
	amber:     "#6d4600",
	blue:      "#2f5680", // darkened from #3d6a9e for AA on selBg (was 3.61:1, now 4.90:1)
	gitAdd:    "#38572a",
	gitDel:    "#963328",
	addBg:     "#dcecd0",
	delBg:     "#f5dbd5",
	addBgWord: "#b9dc9e",
	delBgWord: "#eebba9",
	permBg:    "#f0dfae",
	selBg:     "#e0cf98",
	addInk:    "#264018",
	delInk:    "#5c1810",
	onAccent:  "#fdf6ea",
	cardRun:   "#b08a4a",
	cardErr:   "#b57560",
	cardPerm:  "#c2a04a",
}

// codeThemes provide syntax-token colors for named palettes. Zero continues to
// use its cached Chroma lexers; this compact table avoids initializing an
// unrelated global syntax-theme registry just to select a palette.
var codeThemes = map[themeMode]codeSyntaxTheme{
	"dracula": {
		text:         codeStyle("#f8f8f2"),
		keyword:      codeStyle("#ff79c6"),
		typeName:     codeStyle("#8be9fd"),
		function:     codeStyle("#50fa7b"),
		name:         codeStyle("#50fa7b"),
		string:       codeStyle("#f1fa8c"),
		number:       codeStyle("#bd93f9"),
		comment:      codeStyle("#6272a4"),
		preprocessor: codeStyle("#ff79c6"),
		operator:     codeStyle("#ff79c6"),
		punctuation:  codeStyle("#f8f8f2"),
	},
	"nord": {
		text:         codeStyle("#d8dee9"),
		keyword:      boldCodeStyle("#81a1c1"),
		typeName:     codeStyle("#81a1c1"),
		function:     codeStyle("#88c0d0"),
		name:         codeStyle("#8fbcbb"),
		string:       codeStyle("#a3be8c"),
		number:       codeStyle("#b48ead"),
		comment:      italicCodeStyle("#616e87"),
		preprocessor: codeStyle("#5e81ac"),
		operator:     codeStyle("#81a1c1"),
		punctuation:  codeStyle("#eceff4"),
	},
	"gruvbox": {
		text:         codeStyle("#ebdbb2"),
		keyword:      codeStyle("#fe8019"),
		typeName:     codeStyle("#fabd2f"),
		function:     codeStyle("#fabd2f"),
		name:         boldCodeStyle("#b8bb26"),
		string:       codeStyle("#b8bb26"),
		number:       codeStyle("#d3869b"),
		comment:      italicCodeStyle("#928374"),
		preprocessor: codeStyle("#8ec07c"),
		operator:     codeStyle("#fe8019"),
		punctuation:  codeStyle("#ebdbb2"),
	},
	"tokyo-night": {
		text:         codeStyle("#c0caf5"),
		keyword:      codeStyle("#bb9af7"),
		typeName:     codeStyle("#41a6b5"),
		function:     codeStyle("#7aa2f7"),
		name:         codeStyle("#9ece6a"),
		string:       codeStyle("#9ece6a"),
		number:       codeStyle("#e0af68"),
		comment:      italicCodeStyle("#414868"),
		preprocessor: italicCodeStyle("#414868"),
		operator:     boldCodeStyle("#9ece6a"),
		punctuation:  codeStyle("#c0caf5"),
	},
	"catppuccin": {
		text:         codeStyle("#cdd6f4"),
		keyword:      codeStyle("#cba6f7"),
		typeName:     codeStyle("#f38ba8"),
		function:     codeStyle("#89b4fa"),
		name:         codeStyle("#f9e2af"),
		string:       codeStyle("#a6e3a1"),
		number:       codeStyle("#fab387"),
		comment:      italicCodeStyle("#6c7086"),
		preprocessor: italicCodeStyle("#6c7086"),
		operator:     boldCodeStyle("#89dceb"),
		punctuation:  codeStyle("#cdd6f4"),
	},
	"one-dark": {
		text:        codeStyle("#abb2bf"),
		keyword:     codeStyle("#c678dd"),
		typeName:    codeStyle("#e5c07b"),
		function:    boldCodeStyle("#61afef"),
		name:        codeStyle("#e5c07b"),
		string:      codeStyle("#98c379"),
		number:      codeStyle("#d19a66"),
		comment:     codeStyle("#7f848e"),
		operator:    codeStyle("#56b6c2"),
		punctuation: codeStyle("#abb2bf"),
	},
	"solarized-dark": {
		text:         codeStyle("#839496"),
		keyword:      codeStyle("#719e07"),
		typeName:     codeStyle("#dc322f"),
		function:     codeStyle("#268bd2"),
		name:         codeStyle("#b58900"),
		string:       codeStyle("#2aa198"),
		number:       codeStyle("#2aa198"),
		comment:      codeStyle("#586e75"),
		preprocessor: codeStyle("#719e07"),
		operator:     codeStyle("#719e07"),
		punctuation:  codeStyle("#839496"),
	},
	"solarized-light": {
		text:        codeStyle("#586e75"),
		keyword:     codeStyle("#859900"),
		typeName:    boldCodeStyle("#586e75"),
		function:    codeStyle("#586e75"),
		name:        codeStyle("#cb4b16"),
		string:      codeStyle("#586e75"),
		number:      boldCodeStyle("#586e75"),
		comment:     italicCodeStyle("#93a1a1"),
		operator:    codeStyle("#586e75"),
		punctuation: codeStyle("#586e75"),
	},
	"rose-pine": {
		text:        codeStyle("#e0def4"),
		keyword:     codeStyle("#31748f"),
		typeName:    codeStyle("#9ccfd8"),
		function:    codeStyle("#ebbcba"),
		name:        codeStyle("#ebbcba"),
		string:      codeStyle("#f6c177"),
		number:      codeStyle("#f6c177"),
		comment:     codeStyle("#6e6a86"),
		operator:    codeStyle("#908caa"),
		punctuation: codeStyle("#908caa"),
	},
	"everforest": {
		text:         codeStyle("#d3c6aa"),
		keyword:      codeStyle("#a7c080"),
		typeName:     codeStyle("#dbbc7f"),
		function:     codeStyle("#7fbbb3"),
		name:         codeStyle("#83c092"),
		string:       codeStyle("#a7c080"),
		number:       codeStyle("#e67e80"),
		comment:      italicCodeStyle("#a4aea3"),
		preprocessor: codeStyle("#dbbc7f"),
		operator:     codeStyle("#7fbbb3"),
		punctuation:  codeStyle("#d3c6aa"),
	},
	"neon": {
		text:         codeStyle("#c9ffd2"),
		keyword:      boldCodeStyle("#00e5c8"),
		typeName:     codeStyle("#22e0ff"),
		function:     codeStyle("#39ff6a"),
		name:         codeStyle("#39ff6a"),
		string:       codeStyle("#f4ff3a"),
		number:       codeStyle("#ff4d6d"),
		comment:      italicCodeStyle("#74c468"),
		preprocessor: codeStyle("#00e5c8"),
		operator:     codeStyle("#22e0ff"),
		punctuation:  codeStyle("#c9ffd2"),
	},
	"dune": {
		text:         codeStyle("#2b241a"),
		keyword:      boldCodeStyle("#724028"),
		typeName:     codeStyle("#2f5680"),
		function:     codeStyle("#38572a"),
		name:         codeStyle("#2f5680"),
		string:       codeStyle("#38572a"),
		number:       codeStyle("#872d24"),
		comment:      italicCodeStyle("#655648"),
		preprocessor: codeStyle("#6d4600"),
		operator:     codeStyle("#724028"),
		punctuation:  codeStyle("#2b241a"),
	},
}

// themeEntry is one registered theme: Name is the /theme value + ZERO_THEME/--theme
// token (lowercase, kebab), Label is the picker display text, and IsDark groups the
// picker into Dark/Light sections.
type themeEntry struct {
	Name    string
	Label   string
	Palette palette
	IsDark  bool
}

// themeRegistry is the ordered source of truth for every selectable theme. Order is
// the picker order: all Dark themes first, then all Light, with the brand
// dark/light built-ins leading their groups. themeModes (theme_select.go) prepends
// `system` to this. Append here to add a theme — nothing else needs editing.
var themeRegistry = []themeEntry{
	{Name: "dark", Label: "dark", Palette: darkPalette, IsDark: true},
	{Name: "dracula", Label: "Dracula", Palette: draculaPalette, IsDark: true},
	{Name: "nord", Label: "Nord", Palette: nordPalette, IsDark: true},
	{Name: "gruvbox", Label: "Gruvbox", Palette: gruvboxPalette, IsDark: true},
	{Name: "tokyo-night", Label: "Tokyo Night", Palette: tokyoNightPalette, IsDark: true},
	{Name: "catppuccin", Label: "Catppuccin", Palette: catppuccinPalette, IsDark: true},
	{Name: "one-dark", Label: "One Dark", Palette: oneDarkPalette, IsDark: true},
	{Name: "solarized-dark", Label: "Solarized Dark", Palette: solarizedDarkPalette, IsDark: true},
	{Name: "rose-pine", Label: "Rosé Pine", Palette: rosePinePalette, IsDark: true},
	{Name: "everforest", Label: "Everforest", Palette: everforestPalette, IsDark: true},
	{Name: "neon", Label: "Neon", Palette: neonPalette, IsDark: true},
	{Name: "light", Label: "light", Palette: lightPalette, IsDark: false},
	{Name: "solarized-light", Label: "Solarized Light", Palette: solarizedLightPalette, IsDark: false},
	{Name: "dune", Label: "Dune", Palette: dunePalette, IsDark: false},
}

// themeByName indexes the registry by lowercased name for O(1) lookup.
var themeByName = func() map[string]themeEntry {
	byName := make(map[string]themeEntry, len(themeRegistry))
	for _, entry := range themeRegistry {
		byName[entry.Name] = entry
	}
	return byName
}()

// lookupTheme resolves a theme name (case/space-insensitive) to its entry.
func lookupTheme(name string) (themeEntry, bool) {
	entry, ok := themeByName[strings.ToLower(strings.TrimSpace(name))]
	return entry, ok
}
