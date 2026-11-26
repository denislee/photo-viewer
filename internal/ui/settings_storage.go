package ui

import (
	"fmt"
	"sync"

	"gioui.org/layout"
	"gioui.org/widget"

	"github.com/dns/photo-viewer/internal/format"
)

// storageSection: the trash (status + Empty Trash) and the read-only list of
// where the config, index, cache and trash live.
type storageSection struct {
	host *SettingsView

	emptyTrashBtn widget.Clickable

	mu       sync.Mutex
	trashMsg string
}

func (s *storageSection) id() string { return "storage" }

func (s *storageSection) show(Config) {
	s.setTrashMsg(s.trashStatus())
}

func (s *storageSection) setTrashMsg(msg string) {
	s.mu.Lock()
	s.trashMsg = msg
	s.mu.Unlock()
}

// trashStatus formats the current trash usage. Returns "" when no
// controller is wired.
func (s *storageSection) trashStatus() string {
	if s.host.ctrl == nil {
		return ""
	}
	count, bytes := s.host.ctrl.TrashStats()
	if count == 0 {
		return "Trash is empty"
	}
	return fmt.Sprintf("Trash: %d item(s), %s", count, format.Bytes(bytes))
}

func (s *storageSection) layout(gtx layout.Context, th *Theme) layout.Dimensions {
	ctrl := s.host.ctrl
	if ctrl == nil {
		return layout.Dimensions{}
	}
	if s.emptyTrashBtn.Clicked(gtx) {
		s.setTrashMsg("Emptying trash...")
		ctrl.EmptyTrash(func(count int, bytes int64, err error) {
			switch {
			case err != nil:
				s.setTrashMsg(fmt.Sprintf("Empty trash failed: %v", err))
			case count == 0:
				s.setTrashMsg("Trash is empty")
			default:
				s.setTrashMsg(fmt.Sprintf("Wiped %d item(s), freed %s", count, format.Bytes(bytes)))
			}
			s.host.redraw()
		})
	}
	s.mu.Lock()
	msg := s.trashMsg
	s.mu.Unlock()

	indexPath, cacheDir, trashDir := ctrl.StoragePaths()
	rows := []layout.FlexChild{
		layout.Rigid(sectionHeader(th, "Storage & maintenance",
			"Deleted files go to the trash first, so they can be restored until it is emptied.")),
		vspace(8),
		layout.Rigid(statusLine(th, msg, &s.emptyTrashBtn, "Empty Trash")),
		vspace(10),
		layout.Rigid(subHeader(th, "Locations")),
		vspace(2),
	}
	for _, p := range storagePathRows(configPath(), indexPath, cacheDir, trashDir) {
		rows = append(rows, layout.Rigid(mutedText(th, p)))
	}
	return column(gtx, rows...)
}

// storagePathRows labels the resolved locations, skipping unknown ones.
func storagePathRows(config, index, cacheDir, trash string) []string {
	var out []string
	for _, r := range [][2]string{
		{"Config file", config}, {"Index", index}, {"Cache", cacheDir}, {"Trash", trash},
	} {
		if r[1] != "" {
			out = append(out, r[0]+": "+r[1])
		}
	}
	return out
}
