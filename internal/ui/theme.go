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
	}
}

// buildFontCollection returns the gofont family followed by any system fallback
// faces we manage to load. The fallback covers unicode symbols (⏸ ▶ ✕ ↗ ★ ✓ …)
// that the Go font doesn't include — without it those glyphs render as empty
// tofu squares. Faces are appended (not prepended) so plain text still uses Go.
func buildFontCollection() []font.FontFace {
	coll := gofont.Collection()
	candidates := []struct {
		path   string
		weight font.Weight
	}{
		{"/usr/share/fonts/TTF/DejaVuSans.ttf", font.Normal},
		{"/usr/share/fonts/TTF/DejaVuSans-Bold.ttf", font.Bold},
		{"/usr/share/fonts/dejavu/DejaVuSans.ttf", font.Normal},
		{"/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf", font.Bold},
		{"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", font.Normal},
		{"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf", font.Bold},
		{"/usr/share/fonts/noto/NotoSans-Regular.ttf", font.Normal},
		{"/usr/share/fonts/noto/NotoSans-Bold.ttf", font.Bold},
		{"/usr/share/fonts/liberation/LiberationSans-Regular.ttf", font.Normal},
		{"/usr/share/fonts/liberation/LiberationSans-Bold.ttf", font.Bold},
		{"/Library/Fonts/Arial Unicode.ttf", font.Normal},
		{"/System/Library/Fonts/Apple Symbols.ttf", font.Normal},
	}
	for _, c := range candidates {
		data, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		face, err := opentype.Parse(data)
		if err != nil {
			continue
		}
		coll = append(coll, font.FontFace{
			Font: font.Font{Weight: c.weight},
			Face: face,
		})
	}
	return coll
}
