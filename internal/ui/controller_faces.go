package ui

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/face"
)

// People virtual views. PeopleView is the overview (avatar grid, drawn by
// PeopleView in people.go instead of the photo grid); PersonViewPrefix+id and
// GroupViewPrefix+clusterID are normal photo grids of one person / one
// unnamed group, so the viewer, favorites, delete, export and selection work
// there unchanged.
const (
	PeopleView       = "\x00people"
	PersonViewPrefix = "\x00person:"
	GroupViewPrefix  = "\x00group:"
)

// isPeopleDir reports whether dir is one of the people sentinels.
func isPeopleDir(dir string) bool {
	return dir == PeopleView || strings.HasPrefix(dir, PersonViewPrefix) || strings.HasPrefix(dir, GroupViewPrefix)
}

// PersonDir / GroupDir build the sentinel for one person / group.
func PersonDir(id int64) string { return PersonViewPrefix + strconv.FormatInt(id, 10) }
func GroupDir(id int64) string  { return GroupViewPrefix + strconv.FormatInt(id, 10) }

// parseSentinelID returns the id suffix of a person/group sentinel.
func parseSentinelID(dir, prefix string) (int64, bool) {
	if !strings.HasPrefix(dir, prefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(dir, prefix), 10, 64)
	return id, err == nil && id > 0
}

// Strictness presets (Euclidean; the clusterer compares squares).
const (
	StrictnessStrict = "strict" // 0.50
	StrictnessNormal = "normal" // 0.60, dlib's default
	StrictnessLoose  = "loose"  // 0.65
)

// strictnessSq maps a preset to the squared threshold.
func strictnessSq(s string) float64 {
	switch s {
	case StrictnessStrict:
		return 0.50 * 0.50
	case StrictnessLoose:
		return 0.65 * 0.65
	}
	return face.MaxClusterSqDistance
}

// maxUndo caps the in-memory undo stack.
const maxUndo = 50

// toastFor is how long a people-op toast stays up.
const toastFor = 6 * time.Second

// newFacePipeline builds the pipeline (runs the 1–15 s helper probe). A var
// so tests can substitute a stub.
var newFacePipeline = func(idx *cache.Index, onChange face.OnClusterChange) *face.Pipeline {
	return face.NewPipeline(idx, onChange)
}

// faceState is the Controller's face-recognition state. mu guards every
// field below it; ops serialises people edits (each is a short DB
// transaction, run off the UI goroutine).
type faceState struct {
	ops sync.Mutex

	mu       sync.Mutex
	pipe     *face.Pipeline
	status   face.Status
	probed   bool
	probing  bool
	ctx      context.Context // parent of the workers; cancelled by StopFaces
	stop     context.CancelFunc
	pass     context.CancelFunc // in-flight detection pass
	passGen  int
	undo     []*cache.Undo
	toast    string
	toastEnd time.Time
	debounce *time.Timer

	// ver bumps on every change to people/groups so views reload lazily.
	ver atomic.Uint64
}

// FacesEnabled reports the settings toggle (on by default).
func FacesEnabled() bool { return !GetConfig().FacesDisabled }

// FaceStatus returns the last helper probe and whether one ran yet.
func (c *Controller) FaceStatus() (face.Status, bool) {
	c.faces.mu.Lock()
	defer c.faces.mu.Unlock()
	return c.faces.status, c.faces.probed
}

// PeopleVersion changes whenever people, groups or faces change.
func (c *Controller) PeopleVersion() uint64 { return c.faces.ver.Load() }

func (c *Controller) bumpPeople() {
	c.faces.ver.Add(1)
	if c.invalidate != nil {
		c.invalidate()
	}
}

// onClusterChange is the pipeline's callback; debounced so a detection pass
// creating hundreds of clusters repaints a few times a second at most.
func (c *Controller) onClusterChange() {
	c.faces.mu.Lock()
	defer c.faces.mu.Unlock()
	if c.faces.debounce != nil {
		return
	}
	c.faces.debounce = time.AfterFunc(500*time.Millisecond, func() {
		c.faces.mu.Lock()
		c.faces.debounce = nil
		c.faces.mu.Unlock()
		c.bumpPeople()
	})
}

// StartFaces probes the helper off the UI goroutine, starts the pipeline
// when it works and the toggle is on, then runs a detection pass. Safe to
// call repeatedly: an existing pipeline is reused.
func (c *Controller) StartFaces() {
	c.faces.mu.Lock()
	if c.faces.probing || !FacesEnabled() {
		c.faces.mu.Unlock()
		return
	}
	if c.faces.pipe != nil && c.faces.stop != nil {
		c.faces.mu.Unlock()
		c.DetectFaces()
		return
	}
	c.faces.probing = true
	c.faces.mu.Unlock()

	go func() {
		p := c.facePipe()
		if p == nil {
			p = newFacePipeline(c.index, c.onClusterChange)
		}
		st := face.Status{Working: p.Enabled()}
		if s, ok := face.CachedStatus(); ok {
			st = s
		}
		p.SetMaxSqDistance(strictnessSq(GetConfig().FaceStrictness))
		c.faces.mu.Lock()
		c.faces.pipe = p
		c.faces.status = st
		c.faces.probed = true
		c.faces.probing = false
		started := p.Enabled() && FacesEnabled()
		var ctx context.Context
		if started {
			c.faces.ctx, c.faces.stop = context.WithCancel(context.Background())
			ctx = c.faces.ctx
		}
		c.faces.mu.Unlock()
		if !started {
			c.bumpPeople()
			return
		}
		p.Start(ctx)
		c.bumpPeople()
		c.DetectFaces()
	}()
}

// RecheckFaces re-probes the helper (Settings → Recheck) and starts the
// pipeline if it just became available.
func (c *Controller) RecheckFaces() {
	go func() {
		c.faces.mu.Lock()
		p := c.faces.pipe
		c.faces.mu.Unlock()
		var st face.Status
		if p != nil {
			st = p.Recheck()
		} else {
			st = face.RefreshStatus()
		}
		c.faces.mu.Lock()
		c.faces.status, c.faces.probed = st, true
		c.faces.mu.Unlock()
		c.bumpPeople()
		if st.Working && FacesEnabled() {
			c.StartFaces()
		}
	}()
}

// StopFaces cancels the detection pass and stops the workers (window close,
// toggle off). In-flight detections are abandoned, not drained.
func (c *Controller) StopFaces() {
	c.faces.mu.Lock()
	if c.faces.pass != nil {
		c.faces.pass()
		c.faces.pass = nil
	}
	stop, p := c.faces.stop, c.faces.pipe
	c.faces.stop, c.faces.ctx = nil, nil
	c.faces.mu.Unlock()
	if stop != nil {
		stop()
	}
	if p != nil {
		p.Stop()
	}
}

// facePipe returns the pipeline (nil before the first probe finished).
func (c *Controller) facePipe() *face.Pipeline {
	c.faces.mu.Lock()
	defer c.faces.mu.Unlock()
	return c.faces.pipe
}

// cancelFacePass stops an in-flight detection pass.
func (c *Controller) cancelFacePass() {
	c.faces.mu.Lock()
	if c.faces.pass != nil {
		c.faces.pass()
		c.faces.pass = nil
	}
	c.faces.mu.Unlock()
}

// DetectFaces starts a background pass that feeds every face candidate's
// thumbnail to the pipeline (generating missing thumbnails), shown as a
// cancellable "Detecting faces" process-bar task. Already-fresh files are
// skipped by the pipeline's freshness mirror, so a re-run is cheap.
func (c *Controller) DetectFaces() { go c.detectFaces(context.Background()) }

// detectFaces is the blocking pass; returns false when it was cancelled or
// the pipeline is not running.
func (c *Controller) detectFaces(parent context.Context) bool {
	c.faces.mu.Lock()
	p, running := c.faces.pipe, c.faces.stop != nil
	if c.faces.pass != nil {
		c.faces.pass()
	}
	ctx, cancel := context.WithCancel(parent)
	c.faces.pass = cancel
	c.faces.passGen++
	gen := c.faces.passGen
	c.faces.mu.Unlock()
	defer func() {
		cancel()
		c.faces.mu.Lock()
		if c.faces.passGen == gen {
			c.faces.pass = nil
		}
		c.faces.mu.Unlock()
		c.bumpPeople()
	}()
	if p == nil || !running || !p.Enabled() {
		return false
	}

	var cands []cache.Entry
	c.index.ForEachEntry(func(e cache.Entry) bool {
		if face.Candidate(e.Type) {
			cands = append(cands, e)
		}
		return ctx.Err() == nil
	})
	proc := c.processes.Begin(ctx, ProcFaces, "Detecting faces", cancel, true)
	defer proc.End()
	proc.SetTotal(int64(len(cands)))
	for n, e := range cands {
		proc.Wait()
		if ctx.Err() != nil {
			return false
		}
		proc.SetStatus("Faces: " + strconv.Itoa(n) + " / " + strconv.Itoa(len(cands)) + " photos")
		thumb, err := c.store.Path(e)
		if err == nil && thumb != "" {
			if info, err := os.Stat(thumb); err == nil {
				// Blocking submit: this goroutine is the pass, so back-pressure
				// paces it to the workers instead of dropping jobs.
				if !p.SubmitBlocking(ctx, face.Job{Entry: e, ThumbPath: thumb, ThumbMod: info.ModTime().Unix()}) && ctx.Err() == nil && !p.Enabled() {
					return false
				}
			}
		}
		proc.SetDone(int64(n + 1))
	}
	return ctx.Err() == nil
}

// afterRebuildFaces runs once a Rebuild's rescan committed: re-detect every
// photo (Clear dropped all face rows, the tags survived and re-attach on
// write), Regroup so names re-link to the new clusters, then sweep tags whose
// file is gone and not in the trash.
func (c *Controller) afterRebuildFaces(ctx context.Context) {
	if p := c.facePipe(); p != nil {
		p.InvalidateClusters() // freshness mirror described the wiped index
		if c.detectFaces(ctx) {
			c.RegroupFaces()
		}
	}
	if ctx.Err() != nil {
		return
	}
	n, err := c.index.SweepOrphanTags(cache.TrashOriginalPaths(c.trashDir))
	if err != nil {
		log.Printf("rebuild: sweep face tags: %v", err)
	} else if n > 0 {
		log.Printf("rebuild: swept %d face tags of vanished files", n)
	}
}

// RegroupFaces re-clusters every stored face (People → Regroup, strictness
// change, after Rebuild) as a cancellable process-bar task.
func (c *Controller) RegroupFaces() {
	ctx, cancel := context.WithCancel(context.Background())
	proc := c.processes.Begin(ctx, ProcFaces, "Regrouping faces", cancel, false)
	go func() {
		defer cancel()
		defer proc.End()
		opt := face.RegroupOptions{
			MaxSq: strictnessSq(GetConfig().FaceStrictness),
			Progress: func(done, total int) {
				proc.SetTotal(int64(total))
				proc.SetDone(int64(done))
			},
		}
		c.faces.ops.Lock()
		var err error
		if p := c.facePipe(); p != nil {
			err = p.Regroup(ctx, opt)
		} else {
			err = face.Regroup(ctx, c.index, opt)
		}
		c.faces.ops.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			c.surfaceError("Regroup faces", err)
		}
		c.afterPeopleChange()
	}()
}

// SetFaceStrictness persists the preset and regroups with it.
func (c *Controller) SetFaceStrictness(s string) {
	UpdateConfig(func(cfg *Config) { cfg.FaceStrictness = s })
	if p := c.facePipe(); p != nil {
		p.SetMaxSqDistance(strictnessSq(s))
	}
	c.RegroupFaces()
}

// SetFacesEnabled flips the settings toggle.
func (c *Controller) SetFacesEnabled(on bool) {
	UpdateConfig(func(cfg *Config) { cfg.FacesDisabled = !on })
	if on {
		c.StartFaces()
	} else {
		c.StopFaces()
	}
	c.bumpPeople()
}

// ForgetFaces wipes the machine face layer and, when alsoPeople, every
// person and tag as well (Settings → Forget all face data).
func (c *Controller) ForgetFaces(alsoPeople bool) {
	c.cancelFacePass()
	go func() {
		c.faces.ops.Lock()
		err := c.mutate(func(idx *cache.Index) error { return idx.ForgetFaces(alsoPeople) })
		c.faces.ops.Unlock()
		if p := c.facePipe(); p != nil {
			p.InvalidateClusters()
		}
		c.faces.mu.Lock()
		c.faces.undo = nil
		c.faces.mu.Unlock()
		if err != nil {
			c.surfaceError("Forget face data", err)
		}
		c.afterPeopleChange()
	}()
}

// mutate runs fn through the pipeline's serialisation point when there is
// one, directly otherwise.
func (c *Controller) mutate(fn func(*cache.Index) error) error {
	if p := c.facePipe(); p != nil {
		return p.Mutate(fn)
	}
	return fn(c.index)
}

// faceMaxSq is the active strictness threshold.
func (c *Controller) faceMaxSq() float64 { return strictnessSq(GetConfig().FaceStrictness) }

// PeopleOp runs one people/tag edit off the UI goroutine through the
// serialisation point, pushes its undo, shows a toast and refreshes the
// views. fn returns the undo record (nil for none) and the toast text.
// done, when set, runs afterwards with the error (still off the UI
// goroutine; it must only set fields and invalidate).
func (c *Controller) PeopleOp(fn func(idx *cache.Index) (*cache.Undo, string, error), done func(error)) {
	go func() {
		c.faces.ops.Lock()
		var undo *cache.Undo
		var msg string
		err := c.mutate(func(idx *cache.Index) error {
			var err error
			undo, msg, err = fn(idx)
			return err
		})
		c.faces.ops.Unlock()
		if err == nil && undo != nil {
			c.faces.mu.Lock()
			c.faces.undo = append(c.faces.undo, undo)
			if len(c.faces.undo) > maxUndo {
				c.faces.undo = c.faces.undo[len(c.faces.undo)-maxUndo:]
			}
			c.faces.mu.Unlock()
		}
		if err == nil && msg != "" {
			c.setToast(msg)
		}
		var exists *cache.ErrNameExists
		if err != nil && !errors.As(err, &exists) {
			c.surfaceError("People", err)
		}
		c.afterPeopleChange()
		if done != nil {
			done(err)
		}
	}()
}

// UndoPeople reverts the last people edit (Ctrl+Z in the People views).
func (c *Controller) UndoPeople() {
	c.faces.mu.Lock()
	n := len(c.faces.undo)
	if n == 0 {
		c.faces.mu.Unlock()
		c.setToast("Nothing to undo")
		return
	}
	u := c.faces.undo[n-1]
	c.faces.undo = c.faces.undo[:n-1]
	c.faces.mu.Unlock()
	go func() {
		c.faces.ops.Lock()
		err := c.mutate(func(idx *cache.Index) error { return idx.ApplyUndo(u) })
		c.faces.ops.Unlock()
		if err != nil {
			c.surfaceError("Undo", err)
		} else {
			c.setToast("Undone: " + u.Label)
		}
		c.afterPeopleChange()
	}()
}

// CanUndoPeople reports whether the undo stack is non-empty.
func (c *Controller) CanUndoPeople() bool {
	c.faces.mu.Lock()
	defer c.faces.mu.Unlock()
	return len(c.faces.undo) > 0
}

func (c *Controller) setToast(msg string) {
	c.faces.mu.Lock()
	c.faces.toast, c.faces.toastEnd = msg, time.Now().Add(toastFor)
	c.faces.mu.Unlock()
	if c.invalidate != nil {
		c.invalidate()
		time.AfterFunc(toastFor+50*time.Millisecond, c.invalidate)
	}
}

// Toast returns the current people toast, if one is showing.
func (c *Controller) Toast() (string, bool) {
	c.faces.mu.Lock()
	defer c.faces.mu.Unlock()
	if c.faces.toast == "" || time.Now().After(c.faces.toastEnd) {
		return "", false
	}
	return c.faces.toast, true
}

// afterPeopleChange bumps the people version and re-queries the grid when a
// person/group grid is showing (its membership may have changed) plus the
// sidebar People count.
func (c *Controller) afterPeopleChange() {
	c.bumpPeople()
	dir := c.activeDir()
	if isPeopleDir(dir) {
		c.scheduleRefreshRecount(dir)
	} else {
		c.mu.Lock()
		c.countsValid = false
		c.mu.Unlock()
		c.scheduleRefresh(dir)
	}
}

// Retag is the Review-faces / viewer tag action: record verdict for faces
// and re-place them (see face.Pipeline.Retag). msg is the toast.
func (c *Controller) Retag(faceIDs []int64, verdict cache.Verdict, personID int64, msg string) {
	if len(faceIDs) == 0 {
		return
	}
	c.PeopleOp(func(idx *cache.Index) (*cache.Undo, string, error) {
		u, err := face.Retag(idx, c.faceMaxSq(), faceIDs, verdict, personID)
		return u, msg, err
	}, nil)
}

// faceCountLabel is "1 face" / "N faces".
func faceCountLabel(n int) string {
	if n == 1 {
		return "1 face"
	}
	return strconv.Itoa(n) + " faces"
}

// facesRunning reports whether the pipeline's workers are started.
func (c *Controller) facesRunning() bool {
	c.faces.mu.Lock()
	defer c.faces.mu.Unlock()
	return c.faces.pipe != nil && c.faces.stop != nil
}
