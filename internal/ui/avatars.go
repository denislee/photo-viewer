package ui

import (
	"container/list"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"sync"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/face"
)

// avatarCache loads face-crop avatars (face.EnsureThumbs crops from the
// photo's grid thumbnail into <cacheDir>/faces/) off the UI goroutine and
// keeps the decoded ops in a small LRU keyed by face id. Face ids are never
// reused (AUTOINCREMENT), so a cached crop can't show the wrong face after a
// re-detection.
type avatarCache struct {
	idx        *cache.Index
	store      *cache.ThumbStore
	cacheDir   string
	invalidate func()

	mu      sync.Mutex
	items   map[int64]*list.Element
	lru     *list.List
	pending map[int64]bool
	failed  map[int64]bool
	jobs    chan cache.Face
}

type avatarItem struct {
	id   int64
	op   paint.ImageOp
	size image.Point
}

const (
	avatarCacheCap = 600
	avatarWorkers  = 2
)

func newAvatarCache(idx *cache.Index, store *cache.ThumbStore, cacheDir string, invalidate func()) *avatarCache {
	a := &avatarCache{
		idx: idx, store: store, cacheDir: cacheDir, invalidate: invalidate,
		items: map[int64]*list.Element{}, lru: list.New(),
		pending: map[int64]bool{}, failed: map[int64]bool{},
		jobs: make(chan cache.Face, 256),
	}
	for range avatarWorkers {
		go a.worker()
	}
	return a
}

// Get returns the avatar for f, queueing a load when it isn't cached yet.
func (a *avatarCache) Get(f cache.Face) (paint.ImageOp, image.Point, bool) {
	if a == nil || f.ID == 0 {
		return paint.ImageOp{}, image.Point{}, false
	}
	a.mu.Lock()
	if el, ok := a.items[f.ID]; ok {
		a.lru.MoveToFront(el)
		it := el.Value.(*avatarItem)
		a.mu.Unlock()
		return it.op, it.size, true
	}
	if a.pending[f.ID] || a.failed[f.ID] {
		a.mu.Unlock()
		return paint.ImageOp{}, image.Point{}, false
	}
	a.pending[f.ID] = true
	a.mu.Unlock()
	select {
	case a.jobs <- f:
	default: // queue full: retry on a later frame
		a.mu.Lock()
		delete(a.pending, f.ID)
		a.mu.Unlock()
	}
	return paint.ImageOp{}, image.Point{}, false
}

func (a *avatarCache) worker() {
	for f := range a.jobs {
		op, sz, ok := a.load(f)
		a.mu.Lock()
		delete(a.pending, f.ID)
		if ok {
			a.items[f.ID] = a.lru.PushFront(&avatarItem{id: f.ID, op: op, size: sz})
			for a.lru.Len() > avatarCacheCap {
				old := a.lru.Back()
				a.lru.Remove(old)
				delete(a.items, old.Value.(*avatarItem).id)
			}
		} else {
			a.failed[f.ID] = true
		}
		a.mu.Unlock()
		if ok && a.invalidate != nil {
			a.invalidate()
		}
	}
}

func (a *avatarCache) load(f cache.Face) (paint.ImageOp, image.Point, bool) {
	e, ok := a.idx.GetEntry(f.Path)
	if !ok {
		return paint.ImageOp{}, image.Point{}, false
	}
	src, err := a.store.Path(e)
	if err != nil || src == "" {
		return paint.ImageOp{}, image.Point{}, false
	}
	paths, errs := face.EnsureThumbs(a.cacheDir, src, []face.FaceCrop{{ID: f.ID, BBox: f.BBox}})
	if errs[0] != nil {
		return paint.ImageOp{}, image.Point{}, false
	}
	fh, err := os.Open(paths[0])
	if err != nil {
		return paint.ImageOp{}, image.Point{}, false
	}
	defer fh.Close()
	img, err := jpeg.Decode(fh)
	if err != nil {
		return paint.ImageOp{}, image.Point{}, false
	}
	return paint.NewImageOp(img), img.Bounds().Size(), true
}

// drawAvatar paints the avatar of f into a size×size square at the current
// offset, clipped to a circle when round, with a placeholder disc while it
// loads. Returns the square's dimensions.
func drawAvatar(gtx layout.Context, th *Theme, ac *avatarCache, f cache.Face, size int, round bool) layout.Dimensions {
	sq := image.Rectangle{Max: image.Pt(size, size)}
	var shape clip.Op
	if round {
		shape = clip.Ellipse(sq).Op(gtx.Ops)
	} else {
		shape = clip.Rect(sq).Op()
	}
	st := shape.Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	if op, sz, ok := ac.Get(f); ok {
		drawFitted(gtx, op, sz, sq)
	} else {
		// "?" placeholder disc tint.
		paint.ColorOp{Color: color.NRGBA{R: 0x3a, G: 0x3a, B: 0x44, A: 0xff}}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
	}
	st.Pop()
	return layout.Dimensions{Size: sq.Max}
}
