package ui

import (
	"fmt"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/dns/photo-viewer/internal/cache"
)

// PeopleList shows the face clusters in the sidebar. It refreshes itself by
// re-querying the index whenever Refresh() is called from the controller.
type PeopleList struct {
	idx        *cache.Index
	loadAvatar func(c cache.Cluster) fyne.Resource // controller-owned (needs cacheDir + thumb store)
	onSelect   func(clusterID int64, label string)
	onMenu     func(clusterID int64, current string, abs fyne.Position)

	header *widget.Label
	list   *widget.List

	mu       sync.Mutex
	clusters []cache.Cluster
}

// NewPeopleList builds the People panel.
//
//   - loadAvatar is called on a worker goroutine to produce the per-cluster
//     icon (cropped sample face). May return nil to fall back to a generic
//     icon.
//   - onSelect fires when the user picks a row.
//   - onMenu fires on right-click; the controller shows a popup menu with
//     Rename / Merge actions.
func NewPeopleList(
	idx *cache.Index,
	loadAvatar func(cache.Cluster) fyne.Resource,
	onSelect func(int64, string),
	onMenu func(int64, string, fyne.Position),
) *PeopleList {
	p := &PeopleList{
		idx:        idx,
		loadAvatar: loadAvatar,
		onSelect:   onSelect,
		onMenu:     onMenu,
		header:     widget.NewLabelWithStyle("People", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	}
	p.list = widget.NewList(
		func() int {
			p.mu.Lock()
			defer p.mu.Unlock()
			return len(p.clusters)
		},
		func() fyne.CanvasObject {
			return newPeopleRow()
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			p.mu.Lock()
			if id < 0 || id >= len(p.clusters) {
				p.mu.Unlock()
				return
			}
			c := p.clusters[id]
			p.mu.Unlock()
			row := obj.(*peopleRow)
			row.bind(c, p.onMenu)
			// Avatar load is async: the row carries an "expected"
			// id so a stale callback for a recycled row doesn't
			// overwrite the correct image.
			expected := c.ID
			go func() {
				var res fyne.Resource
				if p.loadAvatar != nil {
					res = p.loadAvatar(c)
				}
				if res == nil {
					return
				}
				fyne.Do(func() { row.setAvatar(expected, res) })
			}()
		},
	)
	p.list.OnSelected = func(id widget.ListItemID) {
		p.mu.Lock()
		if id < 0 || id >= len(p.clusters) {
			p.mu.Unlock()
			return
		}
		c := p.clusters[id]
		p.mu.Unlock()
		label := c.Label.String
		if !c.Label.Valid || label == "" {
			label = fmt.Sprintf("Person #%d", c.ID)
		}
		if p.onSelect != nil {
			p.onSelect(c.ID, label)
		}
	}
	p.Refresh()
	return p
}

// Widget returns the renderable container for the panel.
func (p *PeopleList) Widget() fyne.CanvasObject {
	return container.NewBorder(p.header, nil, nil, nil, p.list)
}

// Refresh re-reads the cluster list from the index. Safe to call from any
// goroutine.
func (p *PeopleList) Refresh() {
	clusters := p.idx.AllClusters()
	p.mu.Lock()
	p.clusters = clusters
	p.mu.Unlock()
	count := len(clusters)
	fyne.Do(func() {
		if count == 0 {
			p.header.SetText("People")
		} else {
			p.header.SetText(fmt.Sprintf("People (%d)", count))
		}
		p.list.Refresh()
	})
}

// peopleRow is the per-cluster widget. We use a custom widget so we can
// catch right-clicks for the menu without a context-menu library.
type peopleRow struct {
	widget.BaseWidget

	avatar     *canvas.Image
	fallback   *widget.Icon
	avatarBox  *fyne.Container
	label      *widget.Label
	cluster    cache.Cluster
	onMenu     func(int64, string, fyne.Position)
	currentRes fyne.Resource
}

func newPeopleRow() *peopleRow {
	r := &peopleRow{
		fallback: widget.NewIcon(theme.AccountIcon()),
		label:    widget.NewLabel(""),
	}
	r.avatar = &canvas.Image{FillMode: canvas.ImageFillContain}
	r.avatar.SetMinSize(fyne.NewSize(32, 32))
	r.avatar.Hide()
	r.avatarBox = container.NewStack(r.fallback, r.avatar)
	r.ExtendBaseWidget(r)
	return r
}

func (r *peopleRow) bind(c cache.Cluster, onMenu func(int64, string, fyne.Position)) {
	r.cluster = c
	r.onMenu = onMenu
	name := c.Label.String
	if !c.Label.Valid || name == "" {
		name = fmt.Sprintf("Unnamed (%d)", c.Count)
	} else {
		name = fmt.Sprintf("%s (%d)", name, c.Count)
	}
	r.label.SetText(name)
	// Reset avatar so a recycled row doesn't show the previous person.
	r.currentRes = nil
	r.avatar.Resource = nil
	r.avatar.Hide()
	r.fallback.Show()
	r.Refresh()
}

func (r *peopleRow) setAvatar(expectedID int64, res fyne.Resource) {
	if r.cluster.ID != expectedID {
		return // row was recycled to a different cluster
	}
	r.currentRes = res
	r.avatar.Resource = res
	r.avatar.Show()
	r.fallback.Hide()
	r.avatar.Refresh()
}

func (r *peopleRow) CreateRenderer() fyne.WidgetRenderer {
	row := container.NewHBox(r.avatarBox, r.label)
	return widget.NewSimpleRenderer(row)
}

// TappedSecondary handles right-click and forwards to the menu callback.
func (r *peopleRow) TappedSecondary(e *fyne.PointEvent) {
	if r.onMenu == nil {
		return
	}
	current := r.cluster.Label.String
	r.onMenu(r.cluster.ID, current, e.AbsolutePosition)
}

