package ui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// minimalTheme is a quiet dark palette: near-black background, soft
// off-white foreground, a single muted accent. Sizes lean a touch larger
// for breathing room.
type minimalTheme struct{}

var _ fyne.Theme = minimalTheme{}

// MinimalTheme returns the application's custom dark theme.
func MinimalTheme() fyne.Theme { return minimalTheme{} }

func (minimalTheme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return color.NRGBA{R: 0x14, G: 0x16, B: 0x1a, A: 0xff}
	case theme.ColorNameForeground:
		return color.NRGBA{R: 0xe6, G: 0xe6, B: 0xe6, A: 0xff}
	case theme.ColorNameDisabled:
		return color.NRGBA{R: 0x55, G: 0x59, B: 0x60, A: 0xff}
	case theme.ColorNamePlaceHolder:
		return color.NRGBA{R: 0x80, G: 0x84, B: 0x8c, A: 0xff}
	case theme.ColorNameButton, theme.ColorNameDisabledButton:
		return color.NRGBA{R: 0x1c, G: 0x1f, B: 0x24, A: 0xff}
	case theme.ColorNameInputBackground, theme.ColorNameInputBorder:
		return color.NRGBA{R: 0x1c, G: 0x1f, B: 0x24, A: 0xff}
	case theme.ColorNameMenuBackground, theme.ColorNameOverlayBackground, theme.ColorNameHeaderBackground:
		return color.NRGBA{R: 0x18, G: 0x1b, B: 0x20, A: 0xff}
	case theme.ColorNameSeparator:
		return color.NRGBA{R: 0x26, G: 0x2a, B: 0x31, A: 0xff}
	case theme.ColorNameScrollBar:
		return color.NRGBA{R: 0x2e, G: 0x33, B: 0x3a, A: 0xff}
	case theme.ColorNameScrollBarBackground:
		return color.NRGBA{R: 0x14, G: 0x16, B: 0x1a, A: 0x00}
	case theme.ColorNameShadow:
		return color.NRGBA{R: 0, G: 0, B: 0, A: 0x80}
	case theme.ColorNameHover:
		return color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0x14}
	case theme.ColorNamePressed:
		return color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0x22}
	case theme.ColorNameSelection:
		return color.NRGBA{R: 0x4d, G: 0x9a, B: 0xff, A: 0x55}
	case theme.ColorNameFocus:
		return color.NRGBA{R: 0x4d, G: 0x9a, B: 0xff, A: 0x66}
	case theme.ColorNamePrimary, theme.ColorNameHyperlink:
		return color.NRGBA{R: 0x6a, G: 0xb0, B: 0xff, A: 0xff}
	case theme.ColorNameError:
		return color.NRGBA{R: 0xe5, G: 0x6b, B: 0x6f, A: 0xff}
	case theme.ColorNameWarning:
		return color.NRGBA{R: 0xe5, G: 0xc0, B: 0x7b, A: 0xff}
	case theme.ColorNameSuccess:
		return color.NRGBA{R: 0x98, G: 0xc3, B: 0x79, A: 0xff}
	}
	return theme.DefaultTheme().Color(name, theme.VariantDark)
}

func (minimalTheme) Font(s fyne.TextStyle) fyne.Resource { return theme.DefaultTheme().Font(s) }
func (minimalTheme) Icon(n fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(n)
}

func (minimalTheme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameText:
		return 10
	case theme.SizeNamePadding:
		return 6
	case theme.SizeNameInnerPadding:
		return 8
	case theme.SizeNameSeparatorThickness:
		return 1
	case theme.SizeNameInputBorder:
		return 1
	case theme.SizeNameInputRadius:
		return 6
	case theme.SizeNameSelectionRadius:
		return 6
	case theme.SizeNameScrollBar:
		return 8
	case theme.SizeNameScrollBarSmall:
		return 4
	}
	return theme.DefaultTheme().Size(name)
}
