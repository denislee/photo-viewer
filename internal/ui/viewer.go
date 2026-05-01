package ui

import (
	"image/color"
	"os"
	"os/exec"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

type keyHandler struct {
	widget.BaseWidget
	content fyne.CanvasObject
	onKey   func(*fyne.KeyEvent)
	onRune  func(rune)
}

func newKeyHandler(content fyne.CanvasObject, onKey func(*fyne.KeyEvent), onRune func(rune)) *keyHandler {
	k := &keyHandler{content: content, onKey: onKey, onRune: onRune}
	k.ExtendBaseWidget(k)
	return k
}

func (k *keyHandler) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(k.content)
}

func (k *keyHandler) TypedKey(e *fyne.KeyEvent) {
	if k.onKey != nil {
		k.onKey(e)
	}
}

func (k *keyHandler) TypedRune(r rune) {
	if k.onRune != nil {
		k.onRune(r)
	}
}

func (k *keyHandler) FocusGained()                {}
func (k *keyHandler) FocusLost()                  {}
func (k *keyHandler) TypedShortcut(fyne.Shortcut) {}

// Open shows e in the viewer: a fullscreen-ish image dialog for photos, or
// the system default player for videos.
func Open(parent fyne.Window, startIndex int, entries []cache.Entry, store *cache.ThumbStore, idx *cache.Index, onChange func()) {
	if startIndex < 0 || startIndex >= len(entries) {
		return
	}

	currentIndex := startIndex
	e := entries[currentIndex]

	if e.Type == scan.TypeVideo {
		go func() {
			_ = exec.Command("xdg-open", e.Path).Start()
		}()
		return
	}

	img := &canvas.Image{}
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSize(600, 400))

	loadingLabel := canvas.NewText("Loading…", color.NRGBA{R: 0xa0, G: 0xa4, B: 0xab, A: 0xff})
	loadingLabel.TextSize = 13
	loadingLabel.Alignment = fyne.TextAlignCenter

	confirmText := canvas.NewText("Delete this photo? Enter to confirm, Esc to cancel", color.NRGBA{R: 0xff, G: 0x66, B: 0x66, A: 0xff})
	confirmText.TextSize = 16
	confirmText.Alignment = fyne.TextAlignCenter
	confirmBg := canvas.NewRectangle(color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0xcc})
	confirmOverlay := container.NewStack(confirmBg, container.NewCenter(confirmText))
	confirmOverlay.Hide()

	favLabel := canvas.NewText("★", color.NRGBA{R: 0xff, G: 0xd7, B: 0x00, A: 0xff})
	favLabel.TextSize = 28
	favLabel.Hide()
	favRow := container.NewHBox(layout.NewSpacer(), container.NewPadded(favLabel))
	favOverlay := container.NewBorder(favRow, nil, nil, nil)

	bg := canvas.NewRectangle(color.NRGBA{R: 0x0a, G: 0x0b, B: 0x0d, A: 0xff})
	stack := container.NewStack(bg, img, container.NewCenter(loadingLabel), favOverlay, confirmOverlay)

	var d *widget.PopUp
	confirmingDelete := false

	updateFavIndicator := func() {
		if currentIndex < 0 || currentIndex >= len(entries) {
			favLabel.Hide()
			favLabel.Refresh()
			return
		}
		if entries[currentIndex].Favorite {
			favLabel.Show()
		} else {
			favLabel.Hide()
		}
		favLabel.Refresh()
	}

	loadPhoto := func(idx int) {
		entry := entries[idx]

		img.Hide()
		loadingLabel.Show()
		loadingLabel.Refresh()
		img.Refresh()
		updateFavIndicator()

		go func() {
			displayPath := entry.Path
			if entry.Type == scan.TypeRAW || entry.Type == scan.TypeHEIC {
				if p, err := store.Path(entry); err == nil {
					displayPath = p
				}
			}

			fyne.Do(func() {
				if currentIndex == idx {
					img.Resource = nil
					img.File = displayPath
					img.Show()
					loadingLabel.Hide()
					loadingLabel.Refresh()
					img.Refresh()
				}
			})
		}()
	}

	navNext := func() {
		for i := currentIndex + 1; i < len(entries); i++ {
			if entries[i].Type != scan.TypeVideo {
				currentIndex = i
				loadPhoto(i)
				return
			}
		}
	}

	navPrev := func() {
		for i := currentIndex - 1; i >= 0; i-- {
			if entries[i].Type != scan.TypeVideo {
				currentIndex = i
				loadPhoto(i)
				return
			}
		}
	}

	showConfirm := func() {
		confirmingDelete = true
		confirmOverlay.Show()
		confirmOverlay.Refresh()
	}

	hideConfirm := func() {
		confirmingDelete = false
		confirmOverlay.Hide()
		confirmOverlay.Refresh()
	}

	toggleFavorite := func() {
		if idx == nil || currentIndex < 0 || currentIndex >= len(entries) {
			return
		}
		entry := &entries[currentIndex]
		newVal := !entry.Favorite
		if err := idx.SetFavorite(entry.Path, newVal); err != nil {
			return
		}
		entry.Favorite = newVal
		updateFavIndicator()
		if onChange != nil {
			onChange()
		}
	}

	deleteCurrent := func() {
		entry := entries[currentIndex]
		if err := os.Remove(entry.Path); err != nil {
			return
		}
		if idx != nil {
			_ = idx.RemoveEntry(entry.Path)
		}
		if store != nil {
			store.Forget(cache.ThumbIDFor(entry.Path))
		}
		entries = append(entries[:currentIndex], entries[currentIndex+1:]...)
		if onChange != nil {
			onChange()
		}
		if len(entries) == 0 {
			d.Hide()
			return
		}
		target := -1
		for i := currentIndex; i < len(entries); i++ {
			if entries[i].Type != scan.TypeVideo {
				target = i
				break
			}
		}
		if target == -1 {
			for i := currentIndex - 1; i >= 0; i-- {
				if entries[i].Type != scan.TypeVideo {
					target = i
					break
				}
			}
		}
		if target == -1 {
			d.Hide()
			return
		}
		currentIndex = target
		loadPhoto(target)
	}

	kh := newKeyHandler(stack, func(e *fyne.KeyEvent) {
		if confirmingDelete {
			if e.Name == fyne.KeyReturn || e.Name == fyne.KeyEnter {
				hideConfirm()
				deleteCurrent()
			} else if e.Name == fyne.KeyEscape {
				hideConfirm()
			}
			return
		}
		if e.Name == fyne.KeyRight {
			navNext()
		} else if e.Name == fyne.KeyLeft {
			navPrev()
		} else if e.Name == fyne.KeyEscape {
			d.Hide()
		}
	}, func(r rune) {
		if confirmingDelete {
			return
		}
		if r == 'j' || r == 'J' {
			navNext()
		} else if r == 'k' || r == 'K' {
			navPrev()
		} else if r == 'q' || r == 'Q' {
			d.Hide()
		} else if r == 'd' || r == 'D' {
			showConfirm()
		} else if r == 'f' || r == 'F' {
			toggleFavorite()
		}
	})

	d = widget.NewModalPopUp(kh, parent.Canvas())
	winSize := parent.Canvas().Size()
	d.Resize(fyne.NewSize(winSize.Width*0.98, winSize.Height*0.98))
	d.Show()
	parent.Canvas().Focus(kh)

	loadPhoto(currentIndex)
}
