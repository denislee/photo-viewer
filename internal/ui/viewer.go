package ui

import (
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n2 := n / unit; n2 >= unit; n2 /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// infoPanel renders the metadata side panel for the viewer.
type infoPanel struct {
	root *fyne.Container

	nameLbl *widget.Label
	rows    *fyne.Container

	pathVal *widget.Label
	typeVal *widget.Label
	sizeVal *widget.Label
	timeVal *widget.Label
	favVal  *widget.Label
	dimVal  *widget.Label
}

func newInfoPanel() *infoPanel {
	p := &infoPanel{}

	p.nameLbl = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	p.nameLbl.Wrapping = fyne.TextWrapWord

	mk := func() *widget.Label {
		l := widget.NewLabel("")
		l.Wrapping = fyne.TextWrapWord
		return l
	}
	p.pathVal = mk()
	p.typeVal = mk()
	p.sizeVal = mk()
	p.timeVal = mk()
	p.favVal = mk()
	p.dimVal = mk()

	row := func(label string, val *widget.Label) fyne.CanvasObject {
		title := widget.NewLabelWithStyle(label, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		return container.NewVBox(title, val)
	}

	p.rows = container.NewVBox(
		row("Path", p.pathVal),
		row("Type", p.typeVal),
		row("Size", p.sizeVal),
		row("Modified", p.timeVal),
		row("Dimensions", p.dimVal),
		row("Favorite", p.favVal),
	)

	header := container.NewVBox(p.nameLbl, widget.NewSeparator())
	scroll := container.NewVScroll(container.NewPadded(p.rows))
	bg := canvas.NewRectangle(theme.Color(theme.ColorNameBackground))
	sizer := canvas.NewRectangle(color.Transparent)
	sizer.SetMinSize(fyne.NewSize(280, 0))
	content := container.NewBorder(container.NewPadded(header), nil, nil, nil, scroll)
	p.root = container.NewStack(bg, sizer, content)
	return p
}

func (p *infoPanel) update(e cache.Entry) {
	p.nameLbl.SetText(filepath.Base(e.Path))
	p.pathVal.SetText(e.Path)
	p.typeVal.SetText(titleCase(e.Type.String()))
	p.sizeVal.SetText(formatSize(e.Size))
	p.timeVal.SetText(e.ModTime.Local().Format("2006-01-02 15:04:05"))
	if e.Favorite {
		p.favVal.SetText("Yes")
	} else {
		p.favVal.SetText("No")
	}
	p.dimVal.SetText("…")

	go func(path string, mt scan.MediaType) {
		dim := decodeDimensions(path, mt)
		fyne.Do(func() { p.dimVal.SetText(dim) })
	}(e.Path, e.Type)
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}

func decodeDimensions(path string, mt scan.MediaType) string {
	if mt == scan.TypeRAW || mt == scan.TypeHEIC || mt == scan.TypeVideo {
		return "—"
	}
	f, err := os.Open(path)
	if err != nil {
		return "—"
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "—"
	}
	return fmt.Sprintf("%d × %d", cfg.Width, cfg.Height)
}

// openExternal launches the system default player for entries the embedded
// player can't or won't handle.
func openExternal(path string) {
	go func() { _ = exec.Command("xdg-open", path).Start() }()
}

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

// Open shows entries[startIndex] in the viewer. Photos render via canvas.Image;
// videos use an embedded silent player (see video.go). Navigation works across
// both media types.
func Open(parent fyne.Window, startIndex int, entries []cache.Entry, store *cache.ThumbStore, idx *cache.Index, onChange func()) {
	if startIndex < 0 || startIndex >= len(entries) {
		return
	}

	currentIndex := startIndex

	img := &canvas.Image{}
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSize(600, 400))

	loadingLabel := canvas.NewText("Loading…", color.NRGBA{R: 0xa0, G: 0xa4, B: 0xab, A: 0xff})
	loadingLabel.TextSize = 13
	loadingLabel.Alignment = fyne.TextAlignCenter

	confirmText := canvas.NewText("Delete this item? Enter to confirm, Esc to cancel", color.NRGBA{R: 0xff, G: 0x66, B: 0x66, A: 0xff})
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
	imgStack := container.NewStack(bg, img, container.NewCenter(loadingLabel), favOverlay, confirmOverlay)

	// Player controls bar lives below the image. Replaced per video; hidden
	// for photos. Wrapped in a container so we can swap children without
	// rebuilding the outer Border.
	controlsHolder := container.NewStack()
	controlsHolder.Hide()

	info := newInfoPanel()
	prefs := fyne.CurrentApp().Preferences()
	infoVisible := prefs.BoolWithFallback("ViewerInfoPanel", true)
	if !infoVisible {
		info.root.Hide()
	}

	stack := container.NewBorder(nil, controlsHolder, nil, info.root, imgStack)
	toggleInfo := func() {
		if info.root.Visible() {
			info.root.Hide()
		} else {
			info.root.Show()
		}
		prefs.SetBool("ViewerInfoPanel", info.root.Visible())
		stack.Refresh()
	}

	var player *videoPlayer
	stopPlayer := func() {
		if player != nil {
			player.Close()
			player = nil
		}
		controlsHolder.Objects = nil
		controlsHolder.Hide()
		controlsHolder.Refresh()
	}

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

	loadCurrent := func(idxN int) {
		stopPlayer()
		entry := entries[idxN]
		updateFavIndicator()
		info.update(entry)
		loadingLabel.Text = "Loading…"

		if entry.Type == scan.TypeVideo {
			p, err := newVideoPlayer(entry.Path, img)
			if err != nil {
				if os.IsNotExist(err) {
					loadingLabel.Text = "File not found"
				} else {
					loadingLabel.Text = "Cannot play video — opening externally"
					openExternal(entry.Path)
				}
				img.Hide()
				loadingLabel.Show()
				loadingLabel.Refresh()
				return
			}
			loadingLabel.Hide()
			loadingLabel.Refresh()
			player = p
			controlsHolder.Objects = []fyne.CanvasObject{p.Bar()}
			controlsHolder.Show()
			controlsHolder.Refresh()
			img.Show()
			img.Refresh()
			return
		}

		img.Hide()
		loadingLabel.Show()
		loadingLabel.Refresh()
		img.Refresh()

		go func() {
			displayPath := entry.Path
			if entry.Type == scan.TypeRAW || entry.Type == scan.TypeHEIC {
				if p, err := store.Path(entry); err == nil {
					displayPath = p
				}
			}

			fyne.Do(func() {
				if currentIndex == idxN {
					img.Resource = nil
					img.Image = nil
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
		if currentIndex+1 < len(entries) {
			currentIndex++
			loadCurrent(currentIndex)
		}
	}

	navPrev := func() {
		if currentIndex-1 >= 0 {
			currentIndex--
			loadCurrent(currentIndex)
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
		info.update(*entry)
		if onChange != nil {
			onChange()
		}
	}

	deleteCurrent := func() {
		entry := entries[currentIndex]
		stopPlayer()
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
			stopPlayer()
			d.Hide()
			return
		}
		if currentIndex >= len(entries) {
			currentIndex = len(entries) - 1
		}
		loadCurrent(currentIndex)
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
		} else if e.Name == fyne.KeySpace {
			if player != nil {
				player.toggle()
			}
		} else if e.Name == fyne.KeyEscape {
			stopPlayer()
			d.Hide()
		}
	}, func(r rune) {
		if confirmingDelete {
			return
		}
		if r == ' ' {
			if player != nil {
				player.toggle()
			}
			return
		}
		if r == 'j' || r == 'J' {
			navNext()
		} else if r == 'k' || r == 'K' {
			navPrev()
		} else if r == 'q' || r == 'Q' {
			stopPlayer()
			d.Hide()
		} else if r == 'd' || r == 'D' {
			showConfirm()
		} else if r == 'i' || r == 'I' {
			toggleInfo()
		} else if r == 'f' || r == 'F' {
			toggleFavorite()
		}
	})

	d = widget.NewModalPopUp(kh, parent.Canvas())
	winSize := parent.Canvas().Size()
	d.Resize(fyne.NewSize(winSize.Width*0.98, winSize.Height*0.98))
	d.Show()
	parent.Canvas().Focus(kh)

	loadCurrent(currentIndex)
}
