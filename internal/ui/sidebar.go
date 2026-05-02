package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/dns/photo-viewer/internal/cache"
)

const yearUIDPrefix = "year:"

// SidebarMode controls what the sidebar tree shows: a filesystem listing or a
// flat list of years derived from entry mtimes.
type SidebarMode string

const (
	SidebarModeFolders SidebarMode = "folders"
	SidebarModeYears   SidebarMode = "years"
)

// Sidebar groups the directory/year tree with the small header bar that lets
// the user pick the grouping mode. The Controller talks to this wrapper so the
// tree, its filter state, and the mode toggle stay in sync.
type Sidebar struct {
	tree    *SidebarTree
	root    *fyne.Container
	modeSel *widget.Select
}

// Tree returns the underlying tree widget so callers can focus it or wire up
// keyboard callbacks.
func (s *Sidebar) Tree() *SidebarTree { return s.tree }

// Widget returns the renderable container (header + tree).
func (s *Sidebar) Widget() fyne.CanvasObject { return s.root }

// SetFilter updates the active media-type filter so per-node counts (and the
// year listing) reflect the same rules as the grid.
func (s *Sidebar) SetFilter(filter string, showRAW bool) {
	s.tree.setFilter(filter, showRAW)
}

// SetMode switches between folders and years. Safe to call at any time; the
// tree refreshes on the main goroutine.
func (s *Sidebar) SetMode(mode SidebarMode) {
	if s.modeSel != nil {
		switch mode {
		case SidebarModeYears:
			s.modeSel.SetSelected("Years")
		default:
			s.modeSel.SetSelected("Folders")
		}
	}
	s.tree.setMode(mode)
}

// SidebarTree is the keyboard-navigable tree backing the sidebar. It supports
// two modes (folders / years) and caches per-node entry counts so SQL isn't
// hit on every render.
type SidebarTree struct {
	widget.Tree
	OnTab        func()
	OnEnterOrL   func()
	currentID    widget.TreeNodeID
	libraryRoot  string
	idx          *cache.Index
	onSelectDir  func(string)
	onSelectYear func(int)

	mu         sync.Mutex
	mode       SidebarMode
	filter     string
	showRAW    bool
	childCache map[string][]widget.TreeNodeID
	statCache  map[string]bool
	countCache map[string]int
	years      []cache.YearStat
}

func (s *SidebarTree) AcceptsTab() bool { return true }

func (s *SidebarTree) TypedKey(e *fyne.KeyEvent) {
	switch e.Name {
	case fyne.KeyTab:
		if s.OnTab != nil {
			s.OnTab()
		}
		return
	case fyne.KeyReturn, fyne.KeyEnter:
		if s.OnEnterOrL != nil {
			s.OnEnterOrL()
		}
		return
	case fyne.KeyDown:
		s.moveBy(1)
		return
	case fyne.KeyUp:
		s.moveBy(-1)
		return
	}
	s.Tree.TypedKey(e)
}

func (s *SidebarTree) TypedRune(r rune) {
	switch r {
	case 'l':
		if s.OnEnterOrL != nil {
			s.OnEnterOrL()
		}
	case 'j':
		s.moveBy(1)
	case 'k':
		s.moveBy(-1)
	case 'h':
		if s.currentMode() == SidebarModeYears {
			return
		}
		if s.currentID != "" && s.IsBranch != nil && s.IsBranch(s.currentID) && s.IsBranchOpen(s.currentID) {
			s.CloseBranch(s.currentID)
			s.Refresh()
			return
		}
		parent := filepath.Dir(string(s.currentID))
		if parent == "." || parent == string(filepath.Separator) {
			return
		}
		s.Select(widget.TreeNodeID(parent))
	default:
		s.Tree.TypedRune(r)
	}
}

func (s *SidebarTree) visibleIDs() []widget.TreeNodeID {
	var ids []widget.TreeNodeID
	var walk func(uid widget.TreeNodeID)
	walk = func(uid widget.TreeNodeID) {
		if s.ChildUIDs == nil {
			return
		}
		for _, c := range s.ChildUIDs(uid) {
			ids = append(ids, c)
			if s.IsBranch != nil && s.IsBranch(c) && s.IsBranchOpen(c) {
				walk(c)
			}
		}
	}
	walk("")
	return ids
}

func (s *SidebarTree) moveBy(delta int) {
	ids := s.visibleIDs()
	if len(ids) == 0 {
		return
	}
	idx := -1
	for i, id := range ids {
		if id == s.currentID {
			idx = i
			break
		}
	}
	next := idx + delta
	if next < 0 {
		next = 0
	}
	if next >= len(ids) {
		next = len(ids) - 1
	}
	s.Select(ids[next])
}

// PageJump moves the selection down or up by approximately one visible page,
// keeping one row of overlap for context.
func (s *SidebarTree) PageJump(forward bool) {
	h := s.Size().Height
	rowH := float32(28)
	rows := max(int(h/rowH), 1)
	if rows > 1 {
		rows--
	}
	if !forward {
		rows = -rows
	}
	s.moveBy(rows)
}

func (s *SidebarTree) currentMode() SidebarMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// setFilter clears the count cache and refreshes visible nodes so labels pick
// up the new filter. If the tree is in years mode, the year list is also
// recomputed (filter affects per-year totals and the set of years shown).
func (s *SidebarTree) setFilter(filter string, showRAW bool) {
	s.mu.Lock()
	if s.filter == filter && s.showRAW == showRAW {
		s.mu.Unlock()
		return
	}
	s.filter = filter
	s.showRAW = showRAW
	s.countCache = make(map[string]int)
	if s.mode == SidebarModeYears {
		s.years = s.idx.Years(filter, showRAW)
	}
	s.mu.Unlock()
	fyne.Do(func() { s.Refresh() })
}

// setMode swaps between folders and years and refreshes the tree.
func (s *SidebarTree) setMode(mode SidebarMode) {
	s.mu.Lock()
	if s.mode == mode {
		s.mu.Unlock()
		return
	}
	s.mode = mode
	if mode == SidebarModeYears {
		s.years = s.idx.Years(s.filter, s.showRAW)
	}
	s.mu.Unlock()
	fyne.Do(func() {
		s.UnselectAll()
		s.Refresh()
	})
}

// NewSidebar builds the sidebar pane: a Folders/Years selector on top of a
// keyboard-navigable tree. onSelectDir fires for folder picks; onSelectYear
// fires for year picks.
func NewSidebar(libraryRoot string, idx *cache.Index, onSelectDir func(path string), onSelectYear func(year int)) *Sidebar {
	tree := &SidebarTree{
		libraryRoot:  libraryRoot,
		idx:          idx,
		onSelectDir:  onSelectDir,
		onSelectYear: onSelectYear,
		mode:         SidebarModeFolders,
		filter:       "All",
		showRAW:      true,
		childCache:   make(map[string][]widget.TreeNodeID),
		statCache:    make(map[string]bool),
		countCache:   make(map[string]int),
	}

	tree.ChildUIDs = func(uid widget.TreeNodeID) []widget.TreeNodeID {
		if tree.currentMode() == SidebarModeYears {
			if uid != "" {
				return nil
			}
			tree.mu.Lock()
			years := tree.years
			tree.mu.Unlock()
			ids := make([]widget.TreeNodeID, 0, len(years))
			for _, y := range years {
				ids = append(ids, yearUIDPrefix+strconv.Itoa(y.Year))
			}
			return ids
		}

		dir := nodePath(libraryRoot, uid)
		tree.mu.Lock()
		if c, ok := tree.childCache[dir]; ok {
			tree.mu.Unlock()
			return c
		}
		tree.mu.Unlock()

		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		var children []widget.TreeNodeID
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if len(name) > 0 && name[0] == '.' {
				continue
			}
			children = append(children, filepath.Join(uid, name))
		}
		sort.Strings(children)

		tree.mu.Lock()
		tree.childCache[dir] = children
		tree.mu.Unlock()
		return children
	}

	tree.IsBranch = func(uid widget.TreeNodeID) bool {
		if tree.currentMode() == SidebarModeYears {
			return false
		}
		p := nodePath(libraryRoot, uid)
		tree.mu.Lock()
		if b, ok := tree.statCache[p]; ok {
			tree.mu.Unlock()
			return b
		}
		tree.mu.Unlock()

		info, err := os.Stat(p)
		b := err == nil && info.IsDir()

		tree.mu.Lock()
		tree.statCache[p] = b
		tree.mu.Unlock()
		return b
	}

	tree.CreateNode = func(branch bool) fyne.CanvasObject {
		return container.NewHBox(
			widget.NewIcon(theme.FolderIcon()),
			widget.NewLabel("placeholder"),
		)
	}

	tree.UpdateNode = func(uid widget.TreeNodeID, branch bool, obj fyne.CanvasObject) {
		row := obj.(*fyne.Container)
		icon := row.Objects[0].(*widget.Icon)
		label := row.Objects[1].(*widget.Label)

		if year, ok := strings.CutPrefix(string(uid), yearUIDPrefix); ok {
			icon.SetResource(theme.HistoryIcon())
			tree.mu.Lock()
			var count int
			for _, y := range tree.years {
				if strconv.Itoa(y.Year) == year {
					count = y.Count
					break
				}
			}
			tree.mu.Unlock()
			if count > 0 {
				label.SetText(fmt.Sprintf("%s (%d)", year, count))
			} else {
				label.SetText(year)
			}
			return
		}

		icon.SetResource(theme.FolderIcon())
		var text, p string
		if uid == "" {
			text = filepath.Base(libraryRoot)
			p = libraryRoot
		} else {
			text = filepath.Base(string(uid))
			p = nodePath(libraryRoot, uid)
		}

		tree.mu.Lock()
		count, ok := tree.countCache[p]
		filter := tree.filter
		showRAW := tree.showRAW
		tree.mu.Unlock()

		if !ok {
			count = idx.CountDirFiltered(p, filter, showRAW)
			tree.mu.Lock()
			tree.countCache[p] = count
			tree.mu.Unlock()
		}

		if count > 0 {
			label.SetText(fmt.Sprintf("%s (%d)", text, count))
		} else {
			label.SetText(text)
		}
	}

	tree.ExtendBaseWidget(tree)

	tree.OnSelected = func(uid widget.TreeNodeID) {
		tree.currentID = uid
		if yearStr, ok := strings.CutPrefix(string(uid), yearUIDPrefix); ok {
			y, err := strconv.Atoi(yearStr)
			if err == nil && tree.onSelectYear != nil {
				tree.onSelectYear(y)
			}
			return
		}
		if tree.onSelectDir != nil {
			tree.onSelectDir(nodePath(libraryRoot, uid))
		}
	}
	tree.OpenBranch("")

	modeSel := widget.NewSelect([]string{"Folders", "Years"}, nil)
	modeSel.SetSelected("Folders")
	s := &Sidebar{tree: tree, modeSel: modeSel}
	modeSel.OnChanged = func(v string) {
		switch v {
		case "Years":
			tree.setMode(SidebarModeYears)
		default:
			tree.setMode(SidebarModeFolders)
		}
	}

	header := container.NewBorder(nil, nil, widget.NewLabel("Group:"), nil, modeSel)
	s.root = container.NewBorder(header, nil, nil, nil, tree)
	return s
}

func nodePath(root, uid string) string {
	if uid == "" {
		return root
	}
	return filepath.Join(root, uid)
}
