package ui

import (
	"gioui.org/widget"
	"golang.org/x/exp/shiny/materialdesign/icons"
)

// IconSet is the bundle of vector icons used by the toolbar and sidebar.
// Loaded once at startup and held by Theme so callers can reach it from
// anywhere a Theme is available.
type IconSet struct {
	All        *widget.Icon
	Photos     *widget.Icon
	Videos     *widget.Icon
	RAW        *widget.Icon
	Years      *widget.Icon
	Import     *widget.Icon
	Duplicates *widget.Icon
	Organize   *widget.Icon
	Rebuild    *widget.Icon
	WarmUp     *widget.Icon
	Settings   *widget.Icon
	Star       *widget.Icon
	SortDur    *widget.Icon
	Trash      *widget.Icon
	WebServer  *widget.Icon
}

func newIconSet() *IconSet {
	mk := func(data []byte) *widget.Icon {
		ic, _ := widget.NewIcon(data)
		return ic
	}
	return &IconSet{
		All:        mk(icons.ImagePhotoLibrary),
		Photos:     mk(icons.ImagePhoto),
		Videos:     mk(icons.AVMovie),
		RAW:        mk(icons.ImageFilter),
		Years:      mk(icons.ActionDateRange),
		Import:     mk(icons.ActionGetApp),
		Duplicates: mk(icons.ContentContentCopy),
		Organize:   mk(icons.FileFolderOpen),
		Rebuild:    mk(icons.ActionCached),
		WarmUp:     mk(icons.ActionTrendingUp),
		Settings:   mk(icons.ActionSettings),
		Star:       mk(icons.ToggleStar),
		SortDur:    mk(icons.AVAVTimer),
		Trash:      mk(icons.ActionDelete),
		WebServer:  mk(icons.ActionDNS),
	}
}
