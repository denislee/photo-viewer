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

type OrganizeView struct {
	Open    bool
	OnClose func()

	mu           sync.Mutex
	mismatched   []MismatchedVideo
	scanning     bool
	running      bool
	progressDone int64
	progressMax  int64
	statusMsg    string
	logVisible   []string
	logBuf       []string
	cancelFn     context.CancelFunc

	closeBtn    widget.Clickable
	cancelBtn   widget.Clickable
	organizeBtn widget.Clickable
	logList     widget.List

	invalidate func()
}

func NewOrganizeView(invalidate func()) *OrganizeView {
	v := &OrganizeView{invalidate: invalidate}
	v.logList.Axis = layout.Vertical
	v.logList.ScrollToEnd = true
	return v
}

func (v *OrganizeView) Show(idx *cache.Index, root string) {
	v.Open = true
	v.mu.Lock()
	v.mismatched = nil
	v.scanning = true
	v.running = false
	v.statusMsg = "Preparing library scan..."
	v.logVisible = nil
	v.logBuf = nil
	
	ctx, cancel := context.WithCancel(context.Background())
	v.cancelFn = cancel
	v.mu.Unlock()
	
	go v.scanForMismatched(ctx, idx, root)
}

func (v *OrganizeView) Close() {
	v.mu.Lock()
	if v.cancelFn != nil {
		v.cancelFn()
		v.cancelFn = nil
	}
	v.mu.Unlock()
	v.Open = false
	if v.OnClose != nil {
		v.OnClose()
	}
}

func (v *OrganizeView) scanForMismatched(ctx context.Context, idx *cache.Index, root string) {
	allEntries := idx.All()
	var videos []cache.Entry
	for _, e := range allEntries {
		if e.Type == scan.TypeVideo {
			videos = append(videos, e)
		}
	}

	total := len(videos)
	atomic.StoreInt64(&v.progressMax, int64(total))
	atomic.StoreInt64(&v.progressDone, 0)
	
	v.setStatus(fmt.Sprintf("Scanning %d videos for mismatched dates...", total))

	// Use a worker pool to speed up metadata extraction (exiftool is slow)
	jobs := make(chan cache.Entry, total)
	results := make(chan MismatchedVideo, total)
	var wg sync.WaitGroup

	numWorkers := runtime.NumCPU()
	if numWorkers > 4 {
		numWorkers = 4 // Don't overwhelm the system with too many exiftool processes
	}

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range jobs {
				if ctx.Err() != nil {
					return
				}
				date := scan.GetMediaDate(e.Path)
				if !scan.SameDateFolder(e.Path, date) {
					results <- MismatchedVideo{Entry: e, ExpectedDate: date}
				}
				atomic.AddInt64(&v.progressDone, 1)
				if atomic.LoadInt64(&v.progressDone)%10 == 0 {
					if v.invalidate != nil {
						v.invalidate()
					}
				}
			}
		}()
	}

	for _, v := range videos {
		jobs <- v
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	var mismatched []MismatchedVideo
	for res := range results {
		mismatched = append(mismatched)
		mismatched = append(mismatched, res)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	v.mismatched = mismatched
	v.scanning = false
	v.cancelFn = nil
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
	v.mu.Unlock()
	if v.invalidate != nil {
		v.invalidate()
	}
}

func (v *OrganizeView) startOrganize(root string) {
	v.mu.Lock()
	if v.running || len(v.mismatched) == 0 {
		v.mu.Unlock()
		return
	}
	v.running = true
	mismatched := append([]MismatchedVideo(nil), v.mismatched...)
	ctx, cancel := context.WithCancel(context.Background())
	v.cancelFn = cancel
	v.mu.Unlock()

	atomic.StoreInt64(&v.progressDone, 0)
	atomic.StoreInt64(&v.progressMax, int64(len(mismatched)))

	go func() {
		defer func() {
			v.mu.Lock()
			v.running = false
			v.cancelFn = nil
			v.mu.Unlock()
			if v.invalidate != nil {
				v.invalidate()
			}
		}()

		for _, m := range mismatched {
			if ctx.Err() != nil {
				v.appendLog("Organization cancelled.")
				v.setStatus("Cancelled.")
				return
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
			
			// Handle collisions
			if _, err := os.Stat(dest); err == nil {
				ext := filepath.Ext(baseName)
				base := strings.TrimSuffix(baseName, ext)
				dest = filepath.Join(destDir, fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext))
			}

			if err := os.Rename(m.Entry.Path, dest); err != nil {
				v.appendLog(fmt.Sprintf("[ERROR] Move %s: %v", baseName, err))
			} else {
				v.appendLog(fmt.Sprintf("[OK] Moved %s -> %s", baseName, dateFolder))
			}
			v.bumpProgress()
		}

		v.mu.Lock()
		v.statusMsg = "Organization complete."
		v.mismatched = nil
		v.mu.Unlock()
	}()
}

func (v *OrganizeView) appendLog(msg string) {
	v.mu.Lock()
	v.logBuf = append(v.logBuf, msg)
	v.mu.Unlock()
	if v.invalidate != nil {
		v.invalidate()
	}
}

func (v *OrganizeView) bumpProgress() {
	atomic.AddInt64(&v.progressDone, 1)
	if v.invalidate != nil {
		v.invalidate()
	}
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
		if v.cancelFn != nil {
			v.cancelFn()
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
	progressDone := atomic.LoadInt64(&v.progressDone)
	progressMax := atomic.LoadInt64(&v.progressMax)
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
