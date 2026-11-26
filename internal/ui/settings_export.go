package ui

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/export"
)

// defaultMaxEdge is the recompress limit used when the editor is blank or
// invalid.
const defaultMaxEdge = 1920

// exportSection: copy/move favorited media out of the library. Flatten,
// recompress and the max edge persist on change; Move never does (D-3).
type exportSection struct {
	host *SettingsView

	runBtn     widget.Clickable
	flatten    widget.Bool
	move       widget.Bool
	recompress widget.Bool
	maxEdge    widget.Editor

	mu  sync.Mutex
	msg string
}

func (s *exportSection) id() string { return "export" }

func (s *exportSection) show(c Config) {
	s.flatten.Value = c.ExportFlatten
	s.recompress.Value = c.ExportRecompress
	s.move.Value = false
	edge := ""
	if c.ExportMaxLongEdge > 0 {
		edge = strconv.Itoa(c.ExportMaxLongEdge)
	}
	s.maxEdge.SetText(edge)
	s.setMsg(s.favoritesStatus())
}

// favoritesStatus reports the number of favorites currently in the index,
// shown next to the Export button so the user knows what would be exported.
func (s *exportSection) favoritesStatus() string {
	ctrl := s.host.ctrl
	if ctrl == nil || ctrl.Index() == nil {
		return ""
	}
	switch n := ctrl.Index().CountFavorites("", true); n {
	case 0:
		return "No favorites to export"
	case 1:
		return "1 favorite ready to export"
	default:
		return fmt.Sprintf("%d favorites ready to export", n)
	}
}

func (s *exportSection) setMsg(msg string) {
	s.mu.Lock()
	s.msg = msg
	s.mu.Unlock()
	s.host.redraw()
}

func (s *exportSection) layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if s.host.ctrl == nil {
		return layout.Dimensions{}
	}
	if s.flatten.Update(gtx) {
		val := s.flatten.Value
		UpdateConfig(func(c *Config) { c.ExportFlatten = val })
	}
	if s.recompress.Update(gtx) {
		val := s.recompress.Value
		UpdateConfig(func(c *Config) { c.ExportRecompress = val })
	}
	s.maxEdge.SingleLine = true
	for {
		ev, ok := s.maxEdge.Update(gtx)
		if !ok {
			break
		}
		if _, changed := ev.(widget.ChangeEvent); changed {
			n := 0
			if t := strings.TrimSpace(s.maxEdge.Text()); t != "" {
				n = parseMaxEdge(t)
			}
			UpdateConfig(func(c *Config) { c.ExportMaxLongEdge = n })
		}
	}
	if s.runBtn.Clicked(gtx) {
		opts := ExportFavoritesOptions{Flatten: s.flatten.Value, Move: s.move.Value}
		if s.recompress.Value {
			opts.MaxLongEdge = parseMaxEdge(s.maxEdge.Text())
		}
		go s.pickDestAndRun(opts)
	}
	s.mu.Lock()
	msg := s.msg
	s.mu.Unlock()

	return column(gtx,
		layout.Rigid(sectionHeader(th, "Export favorites",
			"Copy or move every favorite to a folder you choose. Options are remembered, except Move.")),
		vspace(8),
		layout.Rigid(checkbox(th, &s.flatten, "Flatten into target directory (default: preserve subfolders)")),
		vspace(4),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
				layout.Rigid(checkbox(th, &s.recompress, "Recompress for smaller files — max long edge:")),
				layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					ed := material.Editor(th.Theme, &s.maxEdge, strconv.Itoa(defaultMaxEdge))
					ed.Color = th.Foreground
					ed.HintColor = th.Muted
					gtx.Constraints.Min.X = gtx.Dp(unit.Dp(72))
					gtx.Constraints.Max.X = gtx.Dp(unit.Dp(96))
					return drawEditorBox(gtx, th.CellBG, layout.UniformInset(unit.Dp(6)), ed.Layout)
				}),
				layout.Rigid(mutedText(th, " px (RAW always copied as-is)")),
			)
		}),
		vspace(4),
		layout.Rigid(checkbox(th, &s.move, "Move instead of copy (removes the favorite from the library; not remembered)")),
		vspace(8),
		layout.Rigid(statusLine(th, msg, &s.runBtn, "Choose target & export")),
	)
}

// pickDestAndRun pops a zenity directory picker and, if the user confirms,
// asks the controller to run the export. Status updates are surfaced both
// inline and in the main process bar.
func (s *exportSection) pickDestAndRun(opts ExportFavoritesOptions) {
	dst, err := runZenity("--file-selection", "--directory", "--title=Choose export destination")
	if err != nil {
		s.setMsg("Picker failed: " + err.Error())
		return
	}
	if dst == "" {
		return
	}
	opts.Dst = dst
	verb := "Copying"
	switch {
	case opts.Move:
		verb = "Moving"
	case opts.MaxLongEdge > 0:
		verb = fmt.Sprintf("Recompressing (≤%dpx)", opts.MaxLongEdge)
	}
	s.setMsg(verb + " favorites to " + dst + "…")
	s.host.ctrl.ExportFavorites(opts, func(res export.Result, err error) {
		switch {
		case err != nil:
			s.setMsg(fmt.Sprintf("Export failed: %v (done %d, skipped %d, failed %d)",
				err, res.Done, res.Skipped, res.Failed))
		case res.Failed > 0:
			s.setMsg(fmt.Sprintf("Finished: exported %d, skipped %d, failed %d → %s",
				res.Done, res.Skipped, res.Failed, dst))
		default:
			s.setMsg(fmt.Sprintf("Exported %d favorite(s) to %s (skipped %d)",
				res.Done, dst, res.Skipped))
		}
	})
}

// parseMaxEdge interprets the user's "max long edge" text. Empty / invalid
// / non-positive inputs default to 1920 so the recompress checkbox is safe
// even when the editor is blank.
func parseMaxEdge(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return defaultMaxEdge
	}
	return n
}
