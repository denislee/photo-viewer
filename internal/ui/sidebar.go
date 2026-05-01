package ui

import (
	"os"
	"path/filepath"
	"sort"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// NewSidebar builds a directory tree rooted at libraryRoot. The tree only
// shows directories. onSelect is called with the absolute path each time the
// user clicks a node.
func NewSidebar(libraryRoot string, onSelect func(path string)) *widget.Tree {
	tree := widget.NewTree(
		func(uid widget.TreeNodeID) []widget.TreeNodeID {
			dir := nodePath(libraryRoot, uid)
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
			return children
		},
		func(uid widget.TreeNodeID) bool {
			info, err := os.Stat(nodePath(libraryRoot, uid))
			return err == nil && info.IsDir()
		},
		func(branch bool) fyne.CanvasObject {
			return container.NewHBox(
				widget.NewIcon(theme.FolderIcon()),
				widget.NewLabel("placeholder"),
			)
		},
		func(uid widget.TreeNodeID, branch bool, obj fyne.CanvasObject) {
			row := obj.(*fyne.Container)
			label := row.Objects[1].(*widget.Label)
			if uid == "" {
				label.SetText(filepath.Base(libraryRoot))
				return
			}
			label.SetText(filepath.Base(uid))
		},
	)
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
