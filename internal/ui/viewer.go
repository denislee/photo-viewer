package ui

import (
	"image/color"
	"os/exec"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
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
func Open(parent fyne.Window, startIndex int, entries []cache.Entry, store *cache.ThumbStore) {
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

	loadingLabel := widget.NewLabel("Loading...")
	loadingLabel.Alignment = fyne.TextAlignCenter

	bg := canvas.NewRectangle(color.Black)
	stack := container.NewStack(bg, img, container.NewCenter(loadingLabel))

	var d dialog.Dialog

	loadPhoto := func(idx int) {
		entry := entries[idx]

		img.Hide()
		loadingLabel.Show()
		img.Refresh()

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

	kh := newKeyHandler(stack, func(e *fyne.KeyEvent) {
		if e.Name == fyne.KeyRight {
			navNext()
		} else if e.Name == fyne.KeyLeft {
			navPrev()
		} else if e.Name == fyne.KeyEscape {
			d.Hide()
		}
	}, func(r rune) {
		if r == 'j' || r == 'J' {
			navNext()
		} else if r == 'k' || r == 'K' {
			navPrev()
		} else if r == 'q' || r == 'Q' {
			d.Hide()
		}
	})

	d = dialog.NewCustomWithoutButtons("Photo Viewer", kh, parent)
	winSize := parent.Canvas().Size()
	d.Resize(fyne.NewSize(winSize.Width*0.9, winSize.Height*0.9))
	d.Show()
	parent.Canvas().Focus(kh)

	loadPhoto(currentIndex)
}
