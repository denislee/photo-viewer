package ui

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

type MismatchedVideo struct {
	Entry        cache.Entry
	ExpectedDate time.Time
}

// mediaDateProber is the metadata-date lookup used by the Organize scan,
// indirected through a package var so tests can substitute a counting stub and
// assert the persisted media_date cache spares the second scan from re-probing
// (U-14). Production always uses scan.GetMediaDate.
var mediaDateProber = scan.GetMediaDate

type OrganizeView struct {
	Open    bool
	OnClose func()

	mu           sync.Mutex
	mismatched   []MismatchedVideo
	scanning     bool
	running      bool
	progressDone atomic.Int64
	progressMax  atomic.Int64
	statusMsg    string
	logVisible   []string
	logBuf       []string
	// Separate cancel funcs for the two passes: the metadata scan and the move.
	// Keeping them apart guarantees the Cancel button (and the process bar)
	// aborts the pass that is actually running — a re-entrant scan can never
	// clobber the cancel of an in-flight move, and vice-versa.
	scanCancel context.CancelFunc
	moveCancel context.CancelFunc

	closeBtn    widget.Clickable
	cancelBtn   widget.Clickable
	organizeBtn widget.Clickable
	logList     widget.List

	invalidate func()

	// applyMove keeps the index + thumbnail store consistent after each on-disk
	// rename in the move pass; refreshActive repaints the grid once at the end.
	// Both are wired to the controller in window.go (mirroring how the
	// duplicates view is wired to the delete path).
	applyMove     func(old, new string) error
	refreshActive func()

	processes *ProcessRegistry
	proc      *Process
}

func NewOrganizeView(invalidate func()) *OrganizeView {
	v := &OrganizeView{invalidate: invalidate}
	v.logList.Axis = layout.Vertical
	v.logList.ScrollToEnd = true
	return v
}

// SetProcessRegistry wires the organize view to the main-screen process
// bar so the scan + move passes show up with pause / cancel controls.
func (v *OrganizeView) SetProcessRegistry(r *ProcessRegistry) { v.processes = r }

// SetMover wires the per-rename index/thumbnail bookkeeping callback. Without
// it the move pass would still rename files, but the grid would show broken
// entries (and regenerate thumbnails) until a manual rebuild.
func (v *OrganizeView) SetMover(f func(old, new string) error) { v.applyMove = f }

// SetRefresh wires the callback that repaints the grid from the index once the
// whole move pass finishes.
func (v *OrganizeView) SetRefresh(f func()) { v.refreshActive = f }

func (v *OrganizeView) Show(idx *cache.Index, root string) {
	v.mu.Lock()
	// Running-guard: if a scan or move pass is already in flight (e.g. the user
	// reopened the modal from the process bar), just re-surface the existing
	// state instead of kicking off a SECOND concurrent exiftool scan. A second
	// scan would orphan the first scan's cancel func and, worse, if the move
	// pass is running it would replace the move's cancel so the Cancel button
	// would abort the scan instead of the in-flight move. Mirrors the
	// ImportView / DuplicatesView running-guards.
	if v.scanning || v.running {
		v.Open = true
		v.mu.Unlock()
		return
	}
	v.Open = true
	v.mismatched = nil
	v.scanning = true
	v.running = false
	v.statusMsg = "Preparing library scan..."
	v.logVisible = nil
	v.logBuf = nil

	ctx, cancel := context.WithCancel(context.Background())
	v.scanCancel = cancel
	v.mu.Unlock()

	go v.scanForMismatched(ctx, idx, root)
}

func (v *OrganizeView) Close() {
	// Closing the modal only hides it — any scan or move pass keeps running in
	// the background and stays visible (with a Cancel control) in the process
	// bar, matching the import flow. Reopening via the process bar re-attaches
	// to the same pass thanks to the running-guard in Show. Cancellation is an
	// explicit user action (the Cancel button or the process bar), never a
	// side effect of closing the window — which is what lets a long move
	// continue safely while the user browses.
	v.Open = false
	if v.OnClose != nil {
		v.OnClose()
	}
}

func (v *OrganizeView) scanForMismatched(ctx context.Context, idx *cache.Index, _ string) {
	var proc *Process
	if v.processes != nil {
		proc = v.processes.Begin(ProcOrganize, "Organize: scan", func() {
			v.mu.Lock()
			c := v.scanCancel
			v.mu.Unlock()
			if c != nil {
				c()
			}
		}, true)
		proc.SetStatus("Scanning videos…")
		v.mu.Lock()
		v.proc = proc
		v.mu.Unlock()
		defer func() {
			v.mu.Lock()
			v.proc = nil
			v.mu.Unlock()
			proc.End()
		}()
	}

	total := idx.CountByType(scan.TypeVideo)
	v.progressMax.Store(int64(total))
	v.progressDone.Store(0)
	if proc != nil {
		proc.SetTotal(int64(total))
	}

	v.setStatus(fmt.Sprintf("Scanning %d videos for mismatched dates...", total))

	// Use a worker pool to speed up metadata extraction (exiftool is slow). The
	// videos are streamed straight out of SQLite (ListByType pushes the type
	// filter into the query and pages internally) instead of loading the whole
	// index into RAM and filtering — a large library no longer spikes memory
	// just to enumerate a handful of clips. The jobs channel is only lightly
	// buffered so the streaming producer back-pressures against the slow workers
	// rather than buffering every video up front; results is likewise bounded.
	numWorkers := min(runtime.NumCPU(), 4) // Don't overwhelm the system with too many exiftool processes
	jobs := make(chan cache.Entry, numWorkers*2)
	results := make(chan MismatchedVideo, numWorkers)
	var wg sync.WaitGroup

	for range numWorkers {
		wg.Go(func() {
			for e := range jobs {
				if proc != nil {
					proc.Wait()
				}
				if ctx.Err() != nil {
					return
				}
				// Prefer the media date persisted on a previous open. It's keyed
				// to the file's size/mtime (ReconcileBatch NULLs it on change), so
				// a hit is always fresh for the current bytes and spares the
				// ~50–200ms exiftool round-trip. On a miss (never probed, or an
				// edited file whose cache was invalidated) probe once and persist
				// so later opens skip it (U-14).
				date, cached, err := idx.ProbedMediaDate(e.Path)
				if err != nil || !cached {
					date = mediaDateProber(e.Path)
					_ = idx.SetProbedMediaDate(e.Path, date)
				}
				if !scan.SameDateFolder(e.Path, date) {
					results <- MismatchedVideo{Entry: e, ExpectedDate: date}
				}
				v.progressDone.Add(1)
				if proc != nil {
					// proc.AddDone already coalesces the redraw through the
					// registry's ~30Hz throttle (trailing flush included), so no
					// modulo-10 direct v.invalidate() here — that per-item tick was
					// exactly the storm import's coalescer removed (U-11).
					proc.AddDone(1)
				} else {
					v.scheduleInvalidate()
				}
			}
		})
	}

	// Producer: stream the video rows into the bounded jobs channel on its own
	// goroutine. The select keeps it cancel-safe — once the workers have all
	// returned on ctx cancel nobody drains jobs, so an unguarded send would
	// block forever (and leak this goroutine); the ctx.Done() arm stops the
	// stream and closes jobs instead.
	go func() {
		idx.ListByType(scan.TypeVideo, func(e cache.Entry) bool {
			select {
			case jobs <- e:
				return true
			case <-ctx.Done():
				return false
			}
		})
		close(jobs)
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	var mismatched []MismatchedVideo
	for res := range results {
		mismatched = append(mismatched, res)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.mismatched = mismatched
	v.scanning = false
	v.scanCancel = nil
	if ctx.Err() != nil {
		v.statusMsg = "Scan cancelled."
	} else if len(mismatched) == 0 {
		v.statusMsg = "No mismatched videos found."
	} else {
		v.statusMsg = fmt.Sprintf("Found %d videos with mismatched dates.", len(mismatched))
	}
	if v.invalidate != nil {
		v.invalidate()
	}
}

func (v *OrganizeView) setStatus(msg string) {
	v.mu.Lock()
	v.statusMsg = msg
	proc := v.proc
	v.mu.Unlock()
	if proc != nil {
		proc.SetStatus(msg)
	}
	if v.invalidate != nil {
		v.invalidate()
	}
}

func (v *OrganizeView) startOrganize(root string) {
	v.mu.Lock()
	if v.running || v.scanning || len(v.mismatched) == 0 {
		v.mu.Unlock()
		return
	}
	v.running = true
	mismatched := append([]MismatchedVideo(nil), v.mismatched...)
	ctx, cancel := context.WithCancel(context.Background())
	v.moveCancel = cancel
	v.mu.Unlock()

	v.progressDone.Store(0)
	v.progressMax.Store(int64(len(mismatched)))

	go func() {
		defer cancel()
		var proc *Process
		if v.processes != nil {
			proc = v.processes.Begin(ProcOrganize, "Organize: move", func() {
				v.mu.Lock()
				c := v.moveCancel
				v.mu.Unlock()
				if c != nil {
					c()
				}
			}, true)
			proc.SetStatus(fmt.Sprintf("Moving %d videos…", len(mismatched)))
			proc.SetTotal(int64(len(mismatched)))
			v.mu.Lock()
			v.proc = proc
			v.mu.Unlock()
		}
		movedAny := false
		defer func() {
			v.mu.Lock()
			v.running = false
			v.moveCancel = nil
			v.proc = nil
			v.mu.Unlock()
			if proc != nil {
				proc.End()
			}
			// One refresh at the end (not per file): the index rows were already
			// relocated in lockstep with each rename by applyMove, so a single
			// coalesced repaint surfaces the whole batch. Skipped when nothing
			// moved so a cancelled/no-op pass doesn't churn the grid.
			if movedAny && v.refreshActive != nil {
				v.refreshActive()
			}
			if v.invalidate != nil {
				v.invalidate()
			}
		}()

		for _, m := range mismatched {
			if proc != nil {
				proc.Wait()
			}
			if ctx.Err() != nil {
				v.appendLog("Organization cancelled.")
				v.setStatus("Cancelled.")
				// break, not return, so the deferred end-of-pass refresh still
				// surfaces the files that were moved before the cancel.
				break
			}
			dateFolder := m.ExpectedDate.Format("2006-01-02")
			destDir := filepath.Join(root, dateFolder)

			if err := os.MkdirAll(destDir, 0755); err != nil {
				v.appendLog(fmt.Sprintf("[ERROR] mkdir %s: %v", destDir, err))
				v.bumpProgress()
				continue
			}

			baseName := filepath.Base(m.Entry.Path)
			dest := filepath.Join(destDir, baseName)

			// Handle collisions. The organize move stays within the library
			// root, so source and destination are always on the same
			// filesystem — os.Rename is atomic here and the EXDEV copy fallback
			// that pv-organize needs doesn't apply.
			if _, err := os.Stat(dest); err == nil {
				ext := filepath.Ext(baseName)
				base := strings.TrimSuffix(baseName, ext)
				dest = filepath.Join(destDir, fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext))
			}

			if err := os.Rename(m.Entry.Path, dest); err != nil {
				v.appendLog(fmt.Sprintf("[ERROR] Move %s: %v", baseName, err))
			} else {
				// Relocate the index row + thumbnail so the grid resolves the
				// entry at its new path instead of showing a broken row and
				// re-decoding the video thumbnail from scratch.
				if v.applyMove != nil {
					if err := v.applyMove(m.Entry.Path, dest); err != nil {
						v.appendLog(fmt.Sprintf("[WARN] Index update after moving %s: %v", baseName, err))
					}
				}
				movedAny = true
				v.appendLog(fmt.Sprintf("[OK] Moved %s -> %s", baseName, dateFolder))
			}
			v.bumpProgress()
		}

		v.mu.Lock()
		// Preserve the "Cancelled." status set above; only declare completion
		// when the pass ran to the end.
		if ctx.Err() == nil {
			v.statusMsg = "Organization complete."
		}
		v.mismatched = nil
		v.mu.Unlock()
	}()
}

// scheduleInvalidate wakes the Gio frame loop through the process registry's
// ~30Hz coalescer when one is wired up. Organize's per-file paths (appendLog,
// bumpProgress, the scan-pass tick) fire once per file — thousands/sec during a
// few-thousand-file move — so calling v.invalidate directly from them would peg
// a core repainting a log that scrolls far faster than the eye can read. The
// registry coalesces to ~30Hz with a guaranteed trailing flush, so the final
// log line / progress value still lands. Falls back to the raw invalidate when
// no registry is set (e.g. tests) — those paths aren't in a per-file storm.
// Mirrors ImportView.scheduleInvalidate.
func (v *OrganizeView) scheduleInvalidate() {
	if v.processes != nil {
		v.processes.notify()
		return
	}
	if v.invalidate != nil {
		v.invalidate()
	}
}

func (v *OrganizeView) appendLog(msg string) {
	v.mu.Lock()
	v.logBuf = append(v.logBuf, msg)
	// Cap the pending buffer like import's appendLog does, so a huge move pass
	// whose modal is never laid out (drainLog never runs) can't grow logBuf
	// without bound.
	if len(v.logBuf) > 500 {
		v.logBuf = v.logBuf[len(v.logBuf)-500:]
	}
	v.mu.Unlock()
	v.scheduleInvalidate()
}

func (v *OrganizeView) bumpProgress() {
	v.progressDone.Add(1)
	v.mu.Lock()
	proc := v.proc
	v.mu.Unlock()
	if proc != nil {
		// proc.AddDone already coalesces the redraw through the registry; a
		// direct v.invalidate() per file is exactly the storm we're killing
		// (mirrors ImportView.bumpProgress, U-11).
		proc.AddDone(1)
		return
	}
	v.scheduleInvalidate()
}

func (v *OrganizeView) drainLog() {
	v.mu.Lock()
	if len(v.logBuf) > 0 {
		v.logVisible = append(v.logVisible, v.logBuf...)
		v.logBuf = v.logBuf[:0]
		if len(v.logVisible) > 1000 {
			v.logVisible = v.logVisible[len(v.logVisible)-1000:]
		}
	}
	v.mu.Unlock()
}

func (v *OrganizeView) Layout(gtx layout.Context, th *Theme, root string) layout.Dimensions {
	v.drainLog()

	if v.closeBtn.Clicked(gtx) {
		v.Close()
	}
	if v.cancelBtn.Clicked(gtx) {
		v.mu.Lock()
		// Cancel whichever pass is actually in flight. The scan and move keep
		// separate cancel funcs, so cancelling one never aborts the other.
		if v.scanning && v.scanCancel != nil {
			v.scanCancel()
		} else if v.running && v.moveCancel != nil {
			v.moveCancel()
		}
		v.mu.Unlock()
	}
	if v.organizeBtn.Clicked(gtx) {
		v.startOrganize(root)
	}

	v.mu.Lock()
	statusMsg := v.statusMsg
	scanning := v.scanning
	running := v.running
	mismatchedCount := len(v.mismatched)
	progressDone := v.progressDone.Load()
	progressMax := v.progressMax.Load()
	logVisible := v.logVisible
	v.mu.Unlock()

	// Background.
	totalW := gtx.Constraints.Max.X
	totalH := gtx.Constraints.Max.Y
	bg := image.Rectangle{Max: image.Pt(totalW, totalH)}
	clipArea := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.Background}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	clipArea.Pop()

	pad := layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(12), Left: unit.Dp(12), Right: unit.Dp(12)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				title := material.H6(th.Theme, "Organize Videos")
				title.Color = th.Foreground
				return title.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(14), statusMsg)
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if scanning || running {
					return v.layoutProgress(gtx, th, progressDone, progressMax)
				}
				return layout.Dimensions{}
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if scanning || running {
							return material.Button(th.Theme, &v.cancelBtn, "Cancel").Layout(gtx)
						}
						return material.Button(th.Theme, &v.closeBtn, "Close").Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if scanning || running || mismatchedCount == 0 {
							return layout.Dimensions{}
						}
						return material.Button(th.Theme, &v.organizeBtn, fmt.Sprintf("Move %d Videos", mismatchedCount)).Layout(gtx)
					}),
				)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return v.layoutLog(gtx, th, logVisible)
			}),
		)
	})
}

func (v *OrganizeView) layoutProgress(gtx layout.Context, th *Theme, done, max int64) layout.Dimensions {
	w := gtx.Constraints.Max.X
	barH := gtx.Dp(unit.Dp(6))
	bg := image.Rect(0, 0, w, barH)
	clipBg := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	clipBg.Pop()
	if max > 0 {
		frac := float32(done) / float32(max)
		fg := image.Rect(0, 0, int(float32(w)*frac), barH)
		clipFg := clip.Rect(fg).Push(gtx.Ops)
		paint.ColorOp{Color: th.Accent}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		clipFg.Pop()
	}
	return layout.Dimensions{Size: image.Pt(w, barH)}
}

func (v *OrganizeView) layoutLog(gtx layout.Context, th *Theme, lines []string) layout.Dimensions {
	w := gtx.Constraints.Max.X
	h := gtx.Constraints.Max.Y
	bg := image.Rect(0, 0, w, h)
	ca := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	ca.Pop()
	pad := layout.Inset{Top: unit.Dp(6), Bottom: unit.Dp(6), Left: unit.Dp(8), Right: unit.Dp(8)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return v.logList.Layout(gtx, len(lines), func(gtx layout.Context, i int) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(12), lines[i])
			lbl.Color = th.Foreground
			lbl.MaxLines = 1
			return lbl.Layout(gtx)
		})
	})
}
