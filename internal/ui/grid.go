package ui

import (
	"path/filepath"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"fyne.io/fyne/v2/driver/desktop"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

type tappableCell struct {
	widget.BaseWidget
	content  fyne.CanvasObject
	onDouble func()
}

func newTappableCell(c fyne.CanvasObject, onDouble func()) *tappableCell {
	t := &tappableCell{content: c, onDouble: onDouble}
	t.ExtendBaseWidget(t)
	return t
}
func (t *tappableCell) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(t.content) }
func (t *tappableCell) Tapped(*fyne.PointEvent) {}
func (t *tappableCell) DoubleTapped(*fyne.PointEvent) {
	if t.onDouble != nil {
		t.onDouble()
	}
}

type gridKeyHandler struct {
	widget.BaseWidget
	grid *widget.GridWrap
	g    *ThumbGrid
}

func newGridKeyHandler(g *ThumbGrid) *gridKeyHandler {
	k := &gridKeyHandler{grid: g.grid, g: g}
	k.ExtendBaseWidget(k)
	return k
}
func (k *gridKeyHandler) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(k.grid) }

func (k *gridKeyHandler) moveSelection(delta int) {
	k.g.mu.Lock()
	cur := k.g.selectedIndex
	max := len(k.g.entries) - 1
	k.g.mu.Unlock()
	
	if max < 0 {
		return
	}
	next := cur + delta
	if next < 0 {
		next = 0
	}
	if next > max {
		next = max
	}
	k.grid.Select(widget.GridWrapItemID(next))
	k.grid.ScrollTo(widget.GridWrapItemID(next))
}

func (k *gridKeyHandler) TypedKey(e *fyne.KeyEvent) {
	if e.Name == fyne.KeyReturn || e.Name == fyne.KeyEnter || e.Name == fyne.KeySpace {
		k.g.openSelected()
	} else if e.Name == fyne.KeyLeft {
		k.moveSelection(-1)
	} else if e.Name == fyne.KeyRight {
		k.moveSelection(1)
	} else if e.Name == fyne.KeyUp {
		k.moveSelection(-k.grid.ColumnCount())
	} else if e.Name == fyne.KeyDown {
		k.moveSelection(k.grid.ColumnCount())
	} else {
		k.grid.TypedKey(e)
	}
}
func (k *gridKeyHandler) TypedRune(r rune) {
	switch r {
	case 'h':
		k.moveSelection(-1)
	case 'j':
		k.moveSelection(k.grid.ColumnCount())
	case 'k':
		k.moveSelection(-k.grid.ColumnCount())
	case 'l':
		k.moveSelection(1)
	case ' ':
		k.g.openSelected()
	}
}
func (k *gridKeyHandler) FocusGained() { k.grid.FocusGained() }
func (k *gridKeyHandler) FocusLost()   { k.grid.FocusLost() }
func (k *gridKeyHandler) TypedShortcut(s fyne.Shortcut) {
	if _, ok := s.(*desktop.CustomShortcut); ok {
		cs := s.(*desktop.CustomShortcut)
		if cs.KeyName == fyne.KeyMinus || cs.KeyName == fyne.KeyEqual {
			k.g.handleZoom(cs.KeyName == fyne.KeyEqual)
			return
		}
	}
}

// ThumbGrid renders a scrollable grid of thumbnails for a slice of cache
// entries. Thumbnails are loaded lazily on the first time each cell is bound.
type ThumbGrid struct {
	container *fyne.Container
	grid      *widget.GridWrap
	store     *cache.ThumbStore

	mu            sync.Mutex
	entries       []cache.Entry
	loaded        map[int]bool
	selectedIndex int
	cellSize      float32

	onActivate func(int, []cache.Entry)
}

func NewThumbGrid(store *cache.ThumbStore, onActivate func(int, []cache.Entry)) *ThumbGrid {
	size := fyne.CurrentApp().Preferences().FloatWithFallback("ThumbSize", 160)
	g := &ThumbGrid{
		store:      store,
		loaded:     map[int]bool{},
		onActivate: onActivate,
		cellSize:   float32(size),
	}
	g.container = container.NewStack()
	g.rebuildGrid()
	return g
}

func (g *ThumbGrid) openSelected() {
	g.mu.Lock()
	id := g.selectedIndex
	ok := id >= 0 && id < len(g.entries)
	var entries []cache.Entry
	if ok {
		entries = g.entries
	}
	g.mu.Unlock()
	if ok && g.onActivate != nil {
		g.onActivate(id, entries)
	}
}

func (g *ThumbGrid) handleZoom(in bool) {
	if in {
		g.cellSize += 20
	} else {
		g.cellSize -= 20
	}
	if g.cellSize < 60 {
		g.cellSize = 60
	}
	if g.cellSize > 400 {
		g.cellSize = 400
	}
	fyne.CurrentApp().Preferences().SetFloat("ThumbSize", float64(g.cellSize))
	g.rebuildGrid()
}

func (g *ThumbGrid) rebuildGrid() {
	g.grid = widget.NewGridWrap(
		func() int {
			g.mu.Lock()
			defer g.mu.Unlock()
			return len(g.entries)
		},
		func() fyne.CanvasObject {
			img := canvas.NewImageFromResource(theme.FileImageIcon())
			img.FillMode = canvas.ImageFillContain
			img.SetMinSize(fyne.NewSize(g.cellSize, g.cellSize))
			badge := widget.NewLabel("")
			badge.Alignment = fyne.TextAlignCenter
			
			s := canvas.NewRectangle(nil)
			s.SetMinSize(fyne.NewSize(0, g.cellSize-24))
			
			stack := container.NewStack(img, container.NewVBox(s, badge))
			var cellID int
			return newTappableCell(stack, func() {
				g.mu.Lock()
				g.selectedIndex = cellID
				g.mu.Unlock()
				g.openSelected()
			})
		},
		func(id widget.GridWrapItemID, obj fyne.CanvasObject) {
			g.mu.Lock()
			if id < 0 || int(id) >= len(g.entries) {
				g.mu.Unlock()
				return
			}
			entry := g.entries[id]
			g.mu.Unlock()

			tc := obj.(*tappableCell)
			stack := tc.content.(*fyne.Container)
			
			// We must inject the ID into the closure capture space via a dirty trick or just pointer
			// Let's use a dynamic approach or bind via the struct. Wait, we can't change the closure.
			// Let's store id on the cell.
			tc.onDouble = func() {
				g.mu.Lock()
				g.selectedIndex = int(id)
				g.mu.Unlock()
				g.openSelected()
			}

			img := stack.Objects[0].(*canvas.Image)
			vbox := stack.Objects[1].(*fyne.Container)
			badge := vbox.Objects[1].(*widget.Label)

			switch entry.Type {
			case scan.TypeVideo:
				badge.SetText("▶  " + shortName(entry.Path))
			default:
				badge.SetText("")
			}

			img.Resource = theme.FileImageIcon()
			img.File = ""
			img.Refresh()

			go g.loadThumb(int(id), entry, img)
		},
	)
	g.grid.OnSelected = func(id widget.GridWrapItemID) {
		g.mu.Lock()
		g.selectedIndex = int(id)
		g.mu.Unlock()
	}
	
	kh := newGridKeyHandler(g)
	g.container.Objects = []fyne.CanvasObject{kh}
	g.container.Refresh()
}

func (g *ThumbGrid) loadThumb(id int, e cache.Entry, img *canvas.Image) {
	path, err := g.store.Path(e)
	if err != nil || path == "" {
		return
	}
	g.mu.Lock()
	stillSame := id < len(g.entries) && g.entries[id].Path == e.Path
	g.mu.Unlock()
	if !stillSame {
		return
	}
	fyne.Do(func() {
		img.Resource = nil
		img.File = path
		img.Refresh()
	})
}

// SetEntries replaces the displayed entries. The caller is responsible for
// running on the Fyne main goroutine (wrap in fyne.Do if calling from a
// background scan).
func (g *ThumbGrid) SetEntries(entries []cache.Entry) {
	g.mu.Lock()
	g.entries = entries
	g.selectedIndex = 0
	g.mu.Unlock()
	g.grid.Refresh()
	if len(entries) > 0 {
		g.grid.Select(0)
		g.grid.ScrollToTop()
	}
}

// Append adds entries to the displayed set. Same threading rules as SetEntries.
func (g *ThumbGrid) Append(more ...cache.Entry) {
	g.mu.Lock()
	g.entries = append(g.entries, more...)
	g.mu.Unlock()
	g.grid.Refresh()
}

// Widget returns the underlying GridWrap for placement in a container.
func (g *ThumbGrid) Widget() fyne.CanvasObject {
	return g.container
}

// Count returns the number of items currently displayed.
func (g *ThumbGrid) Count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.entries)
}

func shortName(path string) string { return filepath.Base(path) }
