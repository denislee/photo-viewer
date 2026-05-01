package ui

import (
	"context"
	"fmt"
	"os"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/dns/photo-viewer/internal/cache"
)

// ShowDuplicates opens a window that hashes any files missing a content hash,
// then lists groups of files with identical content. Each group offers a
// "Delete newer (keep oldest)" action that removes every file in the group
// except the one with the earliest mtime.
func ShowDuplicates(parent fyne.Window, idx *cache.Index, store *cache.ThumbStore) {
	win := fyne.CurrentApp().NewWindow("Duplicates")
	win.Resize(fyne.NewSize(900, 700))

	progress := widget.NewProgressBar()
	statusLbl := widget.NewLabel("Hashing files…")
	groupsBox := container.NewVBox()
	scroll := container.NewVScroll(groupsBox)

	body := container.NewBorder(
		container.NewVBox(statusLbl, progress),
		nil, nil, nil,
		scroll,
	)
	win.SetContent(body)

	ctx, cancel := context.WithCancel(context.Background())
	win.SetOnClosed(cancel)

	var render func()
	render = func() {
		groupsBox.Objects = nil
		groups := idx.FindDuplicates()
		statusLbl.SetText(fmt.Sprintf("%d duplicate group(s)", len(groups)))
		if len(groups) == 0 {
			groupsBox.Add(widget.NewLabel("No duplicates found."))
		}
		for i := range groups {
			g := groups[i]
			card := buildGroupCard(win, idx, store, g, func() {
				render()
			})
			groupsBox.Add(card)
		}
		groupsBox.Refresh()
	}

	go func() {
		err := idx.EnsureHashes(ctx, func(done, total int) {
			fyne.Do(func() {
				if total <= 0 {
					progress.SetValue(1)
					statusLbl.SetText("Nothing to hash")
					return
				}
				progress.SetValue(float64(done) / float64(total))
				statusLbl.SetText(fmt.Sprintf("Hashing %d / %d", done, total))
			})
		})
		if err != nil {
			return
		}
		fyne.Do(func() {
			progress.Hide()
			render()
		})
	}()

	win.Show()
}

func buildGroupCard(win fyne.Window, idx *cache.Index, store *cache.ThumbStore, group cache.DuplicateGroup, onChange func()) fyne.CanvasObject {
	if len(group.Entries) < 2 {
		return container.NewVBox()
	}

	header := canvas.NewText(fmt.Sprintf("%d copies — %s each", len(group.Entries), formatBytes(group.Entries[0].Size)), theme.Color(theme.ColorNameForeground))
	header.TextStyle = fyne.TextStyle{Bold: true}

	rows := []fyne.CanvasObject{header}
	for _, e := range group.Entries {
		ts := e.ModTime.Format("2006-01-02 15:04:05")
		rows = append(rows, widget.NewLabel(fmt.Sprintf("• %s   [%s]", e.Path, ts)))
	}

	deleteNewer := widget.NewButton("Delete newer (keep oldest)", func() {
		oldest := group.Entries[0]
		var victims []cache.Entry
		for _, e := range group.Entries[1:] {
			victims = append(victims, e)
		}
		if len(victims) == 0 {
			return
		}
		msg := fmt.Sprintf("Keep:\n  %s\n\nDelete:\n", oldest.Path)
		for _, v := range victims {
			msg += "  " + v.Path + "\n"
		}
		dialog.ShowConfirm("Delete duplicates?", msg, func(ok bool) {
			if !ok {
				return
			}
			for _, v := range victims {
				if err := os.Remove(v.Path); err != nil {
					dialog.ShowError(err, win)
					continue
				}
				_ = idx.RemoveEntry(v.Path)
				store.Forget(cache.ThumbIDFor(v.Path))
			}
			if onChange != nil {
				onChange()
			}
		}, win)
	})

	rows = append(rows, deleteNewer)

	card := container.NewVBox(rows...)
	sep := canvas.NewRectangle(theme.Color(theme.ColorNameDisabled))
	sep.SetMinSize(fyne.NewSize(0, 1))
	return container.NewVBox(container.NewPadded(card), sep)
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
