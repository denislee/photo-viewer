package ui

import (
	"image/color"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

type tappableCell struct {
	widget.BaseWidget
	content   fyne.CanvasObject
	boundPath string
	onTap     func()
	onDouble  func()
}

func newTappableCell(c fyne.CanvasObject, onTap, onDouble func()) *tappableCell {
	t := &tappableCell{content: c, onTap: onTap, onDouble: onDouble}
	t.ExtendBaseWidget(t)
	return t
}
func (t *tappableCell) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(t.content)
}

func (t *tappableCell) Tapped(*fyne.PointEvent) {
	if t.onTap != nil {
		t.onTap()
	}
}

func (t *tappableCell) DoubleTapped(*fyne.PointEvent) {
	if t.onDouble != nil {
		t.onDouble()
	}
}

type customGridWrap struct {
	widget.GridWrap
	g *ThumbGrid
}

func newCustomGridWrap(g *ThumbGrid, length func() int, createItem func() fyne.CanvasObject, updateItem func(id widget.GridWrapItemID, obj fyne.CanvasObject)) *customGridWrap {
	cg := &customGridWrap{g: g}
	cg.Length = length
	cg.CreateItem = createItem
	cg.UpdateItem = updateItem
	cg.ExtendBaseWidget(cg)
	return cg
}

func (c *customGridWrap) AcceptsTab() bool { return true }

func (c *customGridWrap) TypedKey(e *fyne.KeyEvent) {
	if e.Name == fyne.KeyTab {
		if c.g.OnTab != nil {
			c.g.OnTab()
		}
		return
	} else if e.Name == fyne.KeyReturn || e.Name == fyne.KeyEnter || e.Name == fyne.KeySpace {
		c.g.OpenSelected()
	} else if e.Name == fyne.KeyLeft {
		if c.g.AtLeftEdge() {
			if c.g.OnTab != nil {
				c.g.OnTab()
			}
			return
		}
		c.g.MoveSelection(-1)
	} else if e.Name == fyne.KeyRight {
		c.g.MoveSelection(1)
	} else if e.Name == fyne.KeyUp {
		c.g.MoveSelection(-c.g.ColumnCount())
	} else if e.Name == fyne.KeyDown {
		c.g.MoveSelection(c.g.ColumnCount())
	} else {
		c.GridWrap.TypedKey(e)
	}
}

func (c *customGridWrap) TypedRune(r rune) {
	switch r {
	case 'h':
		if c.g.AtLeftEdge() {
			if c.g.OnTab != nil {
				c.g.OnTab()
			}
			return
		}
		c.g.MoveSelection(-1)
	case 'j':
		c.g.MoveSelection(c.g.ColumnCount())
	case 'k':
		c.g.MoveSelection(-c.g.ColumnCount())
	case 'l':
		c.g.MoveSelection(1)
	case ' ':
		c.g.OpenSelected()
	case 'q':
		fyne.CurrentApp().Quit()
	default:
		c.GridWrap.TypedRune(r)
	}
}

// ThumbGrid renders a scrollable grid of thumbnails for a slice of cache
// entries. Thumbnails are loaded lazily on the first time each cell is bound.
type ThumbGrid struct {
	container *fyne.Container
	grid      *customGridWrap
	store     *cache.ThumbStore
	window    fyne.Window

	mu            sync.Mutex
	entries       []cache.Entry
	loaded        map[int]bool
	selectedIndex int
	cellSize      float32

	onActivate func(int, []cache.Entry)
	OnTab      func()
}

func NewThumbGrid(window fyne.Window, store *cache.ThumbStore, onActivate func(int, []cache.Entry)) *ThumbGrid {
	size := fyne.CurrentApp().Preferences().FloatWithFallback("ThumbSize", 160)
	g := &ThumbGrid{
		window:     window,
		store:      store,
		loaded:     map[int]bool{},
		onActivate: onActivate,
		cellSize:   float32(size),
	}
	g.container = container.NewStack()
	g.rebuildGrid()
	return g
}

// focusGrid grabs keyboard focus so TypedRune (h/j/k/l, space) and TypedKey
// (arrows, enter) reach the GridWrap.
func (g *ThumbGrid) focusGrid() {
	if g.window == nil || g.grid == nil {
		return
	}
	g.window.Canvas().Focus(g.grid)
}

func (g *ThumbGrid) OpenSelected() {
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

func (g *ThumbGrid) MoveSelection(delta int) {
	g.mu.Lock()
	cur := g.selectedIndex
	max := len(g.entries) - 1
	g.mu.Unlock()

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
	g.grid.Select(widget.GridWrapItemID(next))
	g.grid.ScrollTo(widget.GridWrapItemID(next))
}

func (g *ThumbGrid) AtLeftEdge() bool {
	g.mu.Lock()
	idx := g.selectedIndex
	empty := len(g.entries) == 0
	g.mu.Unlock()
	if empty {
		return true
	}
	return idx%g.ColumnCount() == 0
}

func (g *ThumbGrid) ColumnCount() int {
	if g.grid == nil {
		return 1
	}
	cols := g.grid.ColumnCount()
	if cols < 1 {
		return 1
	}
	return cols
}

func (g *ThumbGrid) HandleZoom(in bool) {
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
	g.grid = newCustomGridWrap(g,
		func() int {
			g.mu.Lock()
			defer g.mu.Unlock()
			return len(g.entries)
		},
		func() fyne.CanvasObject {
			img := canvas.NewImageFromResource(theme.FileImageIcon())
			img.FillMode = canvas.ImageFillContain
			img.SetMinSize(fyne.NewSize(g.cellSize, g.cellSize))

			// Wrap img in padding so the grid selection highlight is visible
			// around the outside of the thumbnail.
			paddedImg := container.NewPadded(img)

			// Play badge: small dark pill with white triangle, parked in			// the bottom-right corner. Hidden for non-video entries.
			pill := canvas.NewRectangle(color.NRGBA{R: 0, G: 0, B: 0, A: 0xb0})
			pill.CornerRadius = 4
			pill.SetMinSize(fyne.NewSize(22, 16))
			tri := canvas.NewText("▶", color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
			tri.TextSize = 9
			tri.Alignment = fyne.TextAlignCenter
			badge := container.NewStack(pill, container.NewCenter(tri))
			badge.Hide()

			// Float the badge in the bottom-right corner using spacers.
			corner := container.NewVBox(layout.NewSpacer(),
				container.NewHBox(layout.NewSpacer(), badge),
			)

			stack := container.NewStack(paddedImg, container.NewPadded(corner))
			return newTappableCell(stack, nil, nil)
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

			tc.onTap = func() {
				g.mu.Lock()
				g.selectedIndex = int(id)
				g.mu.Unlock()
				g.grid.Select(id)
				g.focusGrid()
			}
			tc.onDouble = func() {
				g.mu.Lock()
				g.selectedIndex = int(id)
				g.mu.Unlock()
				g.OpenSelected()
			}

			// Selection-induced refreshes call UpdateItem on every visible cell.
			// If the cell is already bound to this entry, skip the image reset
			// so the thumbnail doesn't flash back to the placeholder.
			if tc.boundPath == entry.Path {
				return
			}
			tc.boundPath = entry.Path

			paddedImg := stack.Objects[0].(*fyne.Container)
			img := paddedImg.Objects[0].(*canvas.Image)
			badge := findBadge(stack.Objects[1].(*fyne.Container))

			if badge != nil {
				if entry.Type == scan.TypeVideo {
					badge.Show()
				} else {
					badge.Hide()
				}
			}

			img.Resource = theme.FileImageIcon()
			img.File = ""
			img.Refresh()

			go g.loadThumb(int(id), entry, tc, img)
		},
	)
	g.grid.OnSelected = func(id widget.GridWrapItemID) {
		g.mu.Lock()
		g.selectedIndex = int(id)
		g.mu.Unlock()
	}

	g.container.Objects = []fyne.CanvasObject{g.grid}
	g.container.Refresh()
}

func (g *ThumbGrid) loadThumb(id int, e cache.Entry, tc *tappableCell, img *canvas.Image) {
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
		if tc.boundPath != e.Path {
			return
		}
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
		if g.window != nil && g.window.Canvas().Focused() == nil {
			g.focusGrid()
		}
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

// findBadge walks the known cell structure to retrieve the play badge so we
// can show/hide it per-entry without recomputing layout.
func findBadge(padded *fyne.Container) *fyne.Container {
	corner, _ := padded.Objects[0].(*fyne.Container)
	if corner == nil || len(corner.Objects) < 2 {
		return nil
	}
	hbox, _ := corner.Objects[1].(*fyne.Container)
	if hbox == nil || len(hbox.Objects) < 2 {
		return nil
	}
	badge, _ := hbox.Objects[1].(*fyne.Container)
	return badge
}
