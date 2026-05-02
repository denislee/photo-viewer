package ui

import (
	"image/color"
	"sort"
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
	} else if e.Name == fyne.KeyDelete || e.Name == fyne.KeyBackspace {
		c.g.DeleteSelected()
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
	case 'f', 'F':
		c.g.ToggleFavoriteSelected()
	case 'd', 'D':
		c.g.DeleteSelected()
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
	pathIndex     map[string]int
	loaded        map[int]bool
	selectedIndex int
	cellSize      float32
	isActive      bool

	onActivate         func(int, []cache.Entry)
	OnTab              func()
	OnToggleFavorite   func(entry cache.Entry, index int)
	OnDelete           func(entry cache.Entry, index int)
}

func NewThumbGrid(window fyne.Window, store *cache.ThumbStore, onActivate func(int, []cache.Entry)) *ThumbGrid {
	size := fyne.CurrentApp().Preferences().FloatWithFallback("ThumbSize", 160)
	g := &ThumbGrid{
		window:     window,
		store:      store,
		loaded:     map[int]bool{},
		pathIndex:  map[string]int{},
		onActivate: onActivate,
		cellSize:   float32(size),
		isActive:   true,
	}
	g.container = container.NewStack()
	g.rebuildGrid()
	return g
}

// SetActive controls whether the grid attempts to grab focus automatically.
func (g *ThumbGrid) SetActive(active bool) {
	g.mu.Lock()
	g.isActive = active
	g.mu.Unlock()
}

// focusGrid grabs keyboard focus so TypedRune (h/j/k/l, space) and TypedKey
// (arrows, enter) reach the GridWrap.
func (g *ThumbGrid) focusGrid() {
	g.mu.Lock()
	active := g.isActive
	g.mu.Unlock()

	if !active {
		return
	}

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

// ToggleFavoriteSelected fires the OnToggleFavorite callback for the
// currently-selected cell. The callback is responsible for persisting state
// and invoking UpdateEntry to refresh the cell.
func (g *ThumbGrid) ToggleFavoriteSelected() {
	g.mu.Lock()
	id := g.selectedIndex
	ok := id >= 0 && id < len(g.entries)
	var entry cache.Entry
	if ok {
		entry = g.entries[id]
	}
	g.mu.Unlock()
	if ok && g.OnToggleFavorite != nil {
		g.OnToggleFavorite(entry, id)
	}
}

// DeleteSelected fires the OnDelete callback for the currently-selected
// cell. The callback is responsible for confirmation, deletion and triggering
// a refresh.
func (g *ThumbGrid) DeleteSelected() {
	g.mu.Lock()
	id := g.selectedIndex
	ok := id >= 0 && id < len(g.entries)
	var entry cache.Entry
	if ok {
		entry = g.entries[id]
	}
	g.mu.Unlock()
	if ok && g.OnDelete != nil {
		g.OnDelete(entry, id)
	}
}

// UpdateEntry replaces the entry at the given path with e and refreshes the
// affected cell. No-op if the path isn't currently displayed. Caller must run
// on the Fyne main goroutine.
func (g *ThumbGrid) UpdateEntry(e cache.Entry) {
	g.mu.Lock()
	idx, ok := g.pathIndex[e.Path]
	if ok {
		g.entries[idx] = e
	}
	g.mu.Unlock()
	if ok {
		g.grid.RefreshItem(widget.GridWrapItemID(idx))
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

// PageJump moves the selection down or up by approximately one visible page,
// keeping one row of overlap for context.
func (g *ThumbGrid) PageJump(forward bool) {
	cols := g.ColumnCount()
	if g.grid == nil {
		return
	}
	h := g.grid.Size().Height
	cellH := g.cellSize + 8 // approx cellSize + padding
	rows := max(int(h/cellH), 1)
	if rows > 1 {
		rows--
	}
	delta := rows * cols
	if !forward {
		delta = -delta
	}
	g.MoveSelection(delta)
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

			// Float the play badge in the bottom-right corner using spacers.
			corner := container.NewVBox(layout.NewSpacer(),
				container.NewHBox(layout.NewSpacer(), badge),
			)

			// Favorite star, parked in the top-right corner. Hidden for
			// non-favorited entries.
			star := canvas.NewText("★", color.NRGBA{R: 0xff, G: 0xd7, B: 0x00, A: 0xff})
			star.TextSize = 18
			star.TextStyle = fyne.TextStyle{Bold: true}
			star.Hide()
			topRight := container.NewVBox(
				container.NewHBox(layout.NewSpacer(), star),
				layout.NewSpacer(),
			)

			stack := container.NewStack(paddedImg, container.NewPadded(corner), container.NewPadded(topRight))
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

			paddedImg := stack.Objects[0].(*fyne.Container)
			img := paddedImg.Objects[0].(*canvas.Image)
			badge := findBadge(stack.Objects[1].(*fyne.Container))
			star := findStar(stack.Objects[2].(*fyne.Container))

			// Always reflect the current entry's badge state — these can
			// change without the path changing (e.g. favorite toggle).
			if badge != nil {
				if entry.Type == scan.TypeVideo {
					badge.Show()
				} else {
					badge.Hide()
				}
			}
			if star != nil {
				if entry.Favorite {
					star.Show()
				} else {
					star.Hide()
				}
				star.Refresh()
			}

			// Selection-induced refreshes call UpdateItem on every visible cell.
			// If the cell is already bound to this entry, skip the image reset
			// so the thumbnail doesn't flash back to the placeholder.
			if tc.boundPath == entry.Path {
				return
			}
			tc.boundPath = entry.Path

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
	g.pathIndex = make(map[string]int, len(entries))
	for i, e := range entries {
		g.pathIndex[e.Path] = i
	}
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
	for _, e := range more {
		if _, ok := g.pathIndex[e.Path]; ok {
			continue
		}
		g.pathIndex[e.Path] = len(g.entries)
		g.entries = append(g.entries, e)
	}
	g.mu.Unlock()
	g.grid.Refresh()
}

// MergeEntries upserts entries by path: existing paths are updated in place,
// new paths are inserted in sorted position. Refreshes the grid once at the
// end. Caller must invoke on the Fyne main goroutine.
func (g *ThumbGrid) MergeEntries(more []cache.Entry) {
	if len(more) == 0 {
		return
	}
	g.mu.Lock()
	for _, e := range more {
		if idx, ok := g.pathIndex[e.Path]; ok {
			g.entries[idx] = e
			continue
		}
		ins := sort.Search(len(g.entries), func(i int) bool { return g.entries[i].Path >= e.Path })
		g.entries = append(g.entries, cache.Entry{})
		copy(g.entries[ins+1:], g.entries[ins:])
		g.entries[ins] = e
		for i := ins; i < len(g.entries); i++ {
			g.pathIndex[g.entries[i].Path] = i
		}
	}
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

// findStar walks the known cell structure to retrieve the favorite star.
func findStar(padded *fyne.Container) *canvas.Text {
	topRight, _ := padded.Objects[0].(*fyne.Container)
	if topRight == nil || len(topRight.Objects) < 1 {
		return nil
	}
	hbox, _ := topRight.Objects[0].(*fyne.Container)
	if hbox == nil || len(hbox.Objects) < 2 {
		return nil
	}
	star, _ := hbox.Objects[1].(*canvas.Text)
	return star
}
