package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/dns/photo-viewer/internal/cache"
)

type SidebarTree struct {
	widget.Tree
	OnTab      func()
	OnEnterOrL func()
}

func (s *SidebarTree) TypedKey(e *fyne.KeyEvent) {
	if e.Name == fyne.KeyTab {
		if s.OnTab != nil {
			s.OnTab()
		}
		return
	}
	if e.Name == fyne.KeyReturn || e.Name == fyne.KeyEnter {
		s.Tree.TypedKey(e)
		if s.OnEnterOrL != nil {
			s.OnEnterOrL()
		}
		return
	}
	s.Tree.TypedKey(e)
}

func (s *SidebarTree) TypedRune(r rune) {
	if r == 'l' {
		if s.OnEnterOrL != nil {
			s.OnEnterOrL()
		}
		return
	}
	s.Tree.TypedRune(r)
}

// NewSidebar builds a directory tree rooted at libraryRoot. The tree only
// shows directories. onSelect is called with the absolute path each time the
// user clicks a node.
func NewSidebar(libraryRoot string, idx *cache.Index, onSelect func(path string)) *SidebarTree {
	var mu sync.Mutex
	childCache := make(map[string][]widget.TreeNodeID)
	statCache := make(map[string]bool)
	countCache := make(map[string]int)

	tree := &SidebarTree{}
	tree.ChildUIDs = func(uid widget.TreeNodeID) []widget.TreeNodeID {
		dir := nodePath(libraryRoot, uid)
		mu.Lock()
		if c, ok := childCache[dir]; ok {
			mu.Unlock()
			return c
		}
		mu.Unlock()

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

		mu.Lock()
		childCache[dir] = children
		mu.Unlock()
		return children
	}
	tree.IsBranch = func(uid widget.TreeNodeID) bool {
		p := nodePath(libraryRoot, uid)
		mu.Lock()
		if b, ok := statCache[p]; ok {
			mu.Unlock()
			return b
		}
		mu.Unlock()

		info, err := os.Stat(p)
		b := err == nil && info.IsDir()

		mu.Lock()
		statCache[p] = b
		mu.Unlock()
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
		label := row.Objects[1].(*widget.Label)
		var text string
		var p string

		if uid == "" {
			text = filepath.Base(libraryRoot)
			p = libraryRoot
		} else {
			text = filepath.Base(uid)
			p = nodePath(libraryRoot, uid)
		}

		mu.Lock()
		count, ok := countCache[p]
		mu.Unlock()

		if !ok {
			count = idx.CountDir(p)
			mu.Lock()
			countCache[p] = count
			mu.Unlock()
		}

		if count > 0 {
			label.SetText(fmt.Sprintf("%s (%d)", text, count))
		} else {
			label.SetText(text)
		}
	}
	
	tree.ExtendBaseWidget(tree)

	tree.OnSelected = func(uid widget.TreeNodeID) {
		onSelect(nodePath(libraryRoot, uid))
	}
	tree.OpenBranch("")
	return tree
}

func nodePath(root, uid string) string {
	if uid == "" {
		return root
	}
	return filepath.Join(root, uid)
}
