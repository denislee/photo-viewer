package ui

import (
	"gioui.org/layout"
	"gioui.org/widget"
)

// interfaceSection: display preferences set from Settings. The toolbar /
// window state that is only remembered (sort, year grouping, zoom, sidebar
// width) is listed in Config but not editable here.
type interfaceSection struct {
	showShortcuts widget.Bool
}

func (s *interfaceSection) id() string { return "interface" }

func (s *interfaceSection) show(c Config) { s.showShortcuts.Value = c.ShowShortcutHints }

func (s *interfaceSection) layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if s.showShortcuts.Update(gtx) {
		val := s.showShortcuts.Value
		UpdateConfig(func(c *Config) { c.ShowShortcutHints = val })
	}
	return column(gtx,
		layout.Rigid(sectionHeader(th, "Interface", "")),
		vspace(6),
		layout.Rigid(checkbox(th, &s.showShortcuts, "Show keyboard-shortcut hints at the bottom of the window")),
		vspace(4),
		layout.Rigid(mutedText(th, "Sort order, year grouping, zoom and sidebar width are remembered from the toolbar.")),
	)
}
