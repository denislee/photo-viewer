package ui

import (
	"image/color"
	"os"

	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
	"gioui.org/text"
	"gioui.org/widget/material"
)

// Theme bundles the material theme (used for text rendering) with the few
// custom colors the photo viewer needs. Material's defaults are kept for
// labels and buttons.
type Theme struct {
	*material.Theme

	Background  color.NRGBA
	Foreground  color.NRGBA
	Muted       color.NRGBA
	CellBG      color.NRGBA
	SelectionBG color.NRGBA
	Accent      color.NRGBA
	Destructive color.NRGBA

	// Icons holds the vector glyphs used by the toolbar and sidebar.
	Icons *IconSet
}

func NewTheme() *Theme {
	mt := material.NewTheme()
	mt.Shaper = text.NewShaper(text.WithCollection(buildFontCollection()))
	return &Theme{
		Theme:       mt,
		Background:  color.NRGBA{R: 0x1e, G: 0x1e, B: 0x22, A: 0xff},
		Foreground:  color.NRGBA{R: 0xe6, G: 0xe6, B: 0xe6, A: 0xff},
		Muted:       color.NRGBA{R: 0x90, G: 0x95, B: 0xa0, A: 0xff},
		CellBG:      color.NRGBA{R: 0x2a, G: 0x2a, B: 0x30, A: 0xff},
		SelectionBG: color.NRGBA{R: 0x3a, G: 0x55, B: 0x88, A: 0xff},
		Accent:      color.NRGBA{R: 0x5e, G: 0x9c, B: 0xea, A: 0xff},
		Destructive: color.NRGBA{R: 0xbd, G: 0x39, B: 0x39, A: 0xff},
		Icons:       newIconSet(),
	}
}

// buildFontCollection returns the gofont family followed by any system fallback
// faces we manage to load. The fallback covers unicode symbols (⏸ ▶ ✕ ↗ ★ ✓ …)
// that the Go font doesn't include — without it those glyphs render as empty
// tofu squares. Faces are appended (not prepended) so plain text still uses Go.
//
// U5-07: candidates are grouped into families and loading stops at the first
// family whose Regular face loads and parses — the first working family covers
// every glyph the later ones would, so parsing all ~23 candidates on startup
// just cost ~23 ms. Within the winning family the Bold face is loaded too, and
// the preference order (DejaVu, Noto, Liberation, then the macOS fallbacks) is
// unchanged.
func buildFontCollection() []font.FontFace {
	coll := gofont.Collection()
	type fontFile struct {
		path   string
		weight font.Weight
	}
	families := [][]fontFile{
		{
			{"/usr/share/fonts/TTF/DejaVuSans.ttf", font.Normal},
			{"/usr/share/fonts/TTF/DejaVuSans-Bold.ttf", font.Bold},
			{"/usr/share/fonts/dejavu/DejaVuSans.ttf", font.Normal},
			{"/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf", font.Bold},
			{"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", font.Normal},
			{"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf", font.Bold},
		},
		{
			{"/usr/share/fonts/noto/NotoSans-Regular.ttf", font.Normal},
			{"/usr/share/fonts/noto/NotoSans-Bold.ttf", font.Bold},
		},
		{
			{"/usr/share/fonts/liberation/LiberationSans-Regular.ttf", font.Normal},
			{"/usr/share/fonts/liberation/LiberationSans-Bold.ttf", font.Bold},
		},
		// The macOS files have no paired Bold, so each is its own family.
		{{"/Library/Fonts/Arial Unicode.ttf", font.Normal}},
		{{"/System/Library/Fonts/Apple Symbols.ttf", font.Normal}},
	}
	load := func(c fontFile) (font.FontFace, bool) {
		data, err := os.ReadFile(c.path)
		if err != nil {
			return font.FontFace{}, false
		}
		face, err := opentype.Parse(data)
		if err != nil {
			return font.FontFace{}, false
		}
		return font.FontFace{Font: font.Font{Weight: c.weight}, Face: face}, true
	}
	for _, family := range families {
		// The Regular face is what makes a family usable; without it the
		// family is skipped whole rather than contributing a lone Bold.
		var regular font.FontFace
		found := false
		for _, c := range family {
			if c.weight != font.Normal {
				continue
			}
			if face, ok := load(c); ok {
				regular, found = face, true
				break
			}
		}
		if !found {
			continue
		}
		coll = append(coll, regular)
		for _, c := range family {
			if c.weight != font.Bold {
				continue
			}
			if bold, ok := load(c); ok {
				coll = append(coll, bold)
				break
			}
		}
		break
	}
	return coll
}
