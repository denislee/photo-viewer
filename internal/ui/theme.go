package ui

import (
	"image/color"

	"gioui.org/font/gofont"
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
}

func NewTheme() *Theme {
	mt := material.NewTheme()
	mt.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	return &Theme{
		Theme:       mt,
		Background:  color.NRGBA{R: 0x1e, G: 0x1e, B: 0x22, A: 0xff},
		Foreground:  color.NRGBA{R: 0xe6, G: 0xe6, B: 0xe6, A: 0xff},
		Muted:       color.NRGBA{R: 0x90, G: 0x95, B: 0xa0, A: 0xff},
		CellBG:      color.NRGBA{R: 0x2a, G: 0x2a, B: 0x30, A: 0xff},
		SelectionBG: color.NRGBA{R: 0x3a, G: 0x55, B: 0x88, A: 0xff},
		Accent:      color.NRGBA{R: 0x5e, G: 0x9c, B: 0xea, A: 0xff},
	}
}
