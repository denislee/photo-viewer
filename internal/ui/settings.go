package ui

import (
	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// SettingsView is the Settings overlay: a fixed header (title + Close) over
// one scrollable page of titled sections. Each section lives in its own
// settings_<name>.go file and owns its widgets, lock and status line; this
// file is only the shell.
//
// The zero value is valid (modals_test.go builds one): sections are created
// lazily on first use, and the ones that need the controller draw nothing
// while it is nil.
type SettingsView struct {
	Open    bool
	OnClose func()
	// OnOpenWebServer closes Settings and opens the web-server panel. Wired
	// by wireToolbar (linkSettingsToWebServer); the link row is hidden while
	// it is nil.
	OnOpenWebServer func()

	closeBtn widget.Clickable
	list     widget.List

	// ctrl is wired by window.go. Optional: sections that need it hide
	// themselves while it is nil.
	ctrl       *Controller
	invalidate func()

	secs *settingsSections
}

// settingsSection is one titled block of the Settings page.
type settingsSection interface {
	// id is the stable key used by PV_DEBUG_START_VIEW=settings:<id>.
	id() string
	// show is called from SettingsView.Show: reload widget state from c / the
	// controller and reset transient state (e.g. the Forget confirmation).
	show(c Config)
	// layout handles this section's events, then draws it, header included.
	// It returns zero dimensions when the section has nothing to show (no
	// controller wired).
	layout(gtx layout.Context, th *Theme) layout.Dimensions
}

// settingsSections holds the typed sections (for tests and cross-section
// wiring) plus their display order.
type settingsSections struct {
	imp    *importSection
	iface  *interfaceSection
	faces  *facesSection
	export *exportSection
	store  *storageSection
	web    *webLinkSection
	order  []settingsSection
}

// sectionsFor builds the sections on first use.
func (v *SettingsView) sectionsFor() *settingsSections {
	if v.secs == nil {
		s := &settingsSections{
			imp:    &importSection{host: v},
			iface:  &interfaceSection{},
			faces:  &facesSection{host: v},
			export: &exportSection{host: v},
			store:  &storageSection{host: v},
			web:    &webLinkSection{host: v},
		}
		s.order = []settingsSection{s.imp, s.iface, s.faces, s.export, s.store, s.web}
		v.secs = s
		v.list.Axis = layout.Vertical
	}
	return v.secs
}

// SetController gives the settings view a handle to the controller (trash,
// faces, export, resolved paths). Call once at startup.
func (v *SettingsView) SetController(c *Controller) { v.ctrl = c }

// SetInvalidate registers the redraw callback used by background goroutines
// (folder pickers, saves, trash, export) after they change a status line.
func (v *SettingsView) SetInvalidate(f func()) { v.invalidate = f }

func (v *SettingsView) redraw() {
	if v.invalidate != nil {
		v.invalidate()
	}
}

// NewSettingsView constructs the overlay.
func NewSettingsView() *SettingsView {
	v := &SettingsView{}
	v.sectionsFor()
	return v
}

// Show reloads every section from the persisted config and reveals the modal
// scrolled to the top.
func (v *SettingsView) Show() {
	c := GetConfig()
	for _, s := range v.sectionsFor().order {
		s.show(c)
	}
	v.list.Position = layout.Position{}
	v.Open = true
}

// ShowSection is Show, scrolled to the section with the given id (see
// settingsSection.id); an unknown id leaves the page at the top.
func (v *SettingsView) ShowSection(id string) {
	v.Show()
	for i, s := range v.secs.order {
		if s.id() == id {
			v.list.Position = layout.Position{First: i}
			return
		}
	}
}

// Close hides the modal.
func (v *SettingsView) Close() {
	v.Open = false
	if v.OnClose != nil {
		v.OnClose()
	}
}

// Layout draws the overlay over the supplied constraints.
func (v *SettingsView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	secs := v.sectionsFor()
	if v.closeBtn.Clicked(gtx) {
		v.Close()
	}

	drawBackground(gtx, th.Background)

	pad := layout.Inset{Top: unit.Dp(16), Bottom: unit.Dp(16), Left: unit.Dp(20), Right: unit.Dp(20)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return column(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						lbl := material.H6(th.Theme, "Settings")
						lbl.Color = th.Foreground
						return lbl.Layout(gtx)
					}),
					layout.Rigid(material.Button(th.Theme, &v.closeBtn, "Close (esc)").Layout),
				)
			}),
			vspace(12),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return material.List(th.Theme, &v.list).Layout(gtx, len(secs.order), func(gtx layout.Context, i int) layout.Dimensions {
					// Right inset keeps the controls clear of the scrollbar.
					in := layout.Inset{Right: unit.Dp(14)}
					if i > 0 {
						in.Top = unit.Dp(24)
					}
					return in.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return secs.order[i].layout(gtx, th)
					})
				})
			}),
		)
	})
}

// webLinkSection points at the web-server panel, which keeps its own
// port / network settings because they apply when the server starts.
type webLinkSection struct {
	host    *SettingsView
	openBtn widget.Clickable
}

func (s *webLinkSection) id() string  { return "webserver" }
func (s *webLinkSection) show(Config) {}

func (s *webLinkSection) layout(gtx layout.Context, th *Theme) layout.Dimensions {
	open := s.host.OnOpenWebServer
	if open == nil {
		return layout.Dimensions{}
	}
	if s.openBtn.Clicked(gtx) {
		open()
	}
	return column(gtx,
		layout.Rigid(sectionHeader(th, "Web server", "")),
		vspace(6),
		layout.Rigid(statusLine(th, "Port and network access are set in the Web server panel.", &s.openBtn, "Open")),
	)
}
