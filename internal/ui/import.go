package ui

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/scan"
)

// ImportView is the modal "import media" overlay. It mirrors the Fyne
// Controller.runImportFromDirs flow:
//   - phase 1: extract user-supplied ZIPs into the inbox
//   - phase 2: process anything currently in the inbox as one batch
//   - phase 3: walk each user-supplied directory; copy and process each
//     subdirectory as its own batch.
//
// File-dialog gaps are filled with text inputs since Gio has no native
// folder/file picker.
type ImportView struct {
	Open    bool
	OnClose func()

	mu           sync.Mutex
	importDirs   []string
	zipFiles     []string
	statusMsg    string
	statMoved    int64
	statSkipped  int64
	statErrors   int64
	running      bool
	finished     bool // at least one import has completed since the overlay opened
	deleteSource bool
	cancelImport context.CancelFunc

	// Shared modal plumbing (see taskui.go): the bounded worker→UI log buffer
	// and the done/max progress state that used to be hand-rolled fields here.
	log      taskLog
	progress progressModel

	// SD-card picker state. sdShown toggles the inline device list under the
	// add row; sdDevices is the last lsblk snapshot; sdError surfaces a
	// detection or mount failure. sdMounts records devices we mounted
	// ourselves so they can be unmounted automatically when the import
	// finishes (or the overlay is closed).
	sdShown    bool
	sdDevices  []removableDevice
	sdError    string
	sdScanning bool
	sdBusyDev  string // device path currently being mounted, "" otherwise
	sdMounts   []sdMount
	sdBtns     []widget.Clickable

	// UI state.
	closeBtn      widget.Clickable
	startBtn      widget.Clickable
	cancelBtn     widget.Clickable
	addDirBtn     widget.Clickable
	addZipBtn     widget.Clickable
	addSDBtn      widget.Clickable
	sdRefreshBtn  widget.Clickable
	sdHideBtn     widget.Clickable
	deleteCheck   widget.Bool
	addPathEditor widget.Editor
	logList       widget.List

	invalidate func()

	// scope lets the import publish progress to the main-screen process bar so
	// the user can pause / resume / cancel without opening the modal. It owns the
	// registry pointer and the live-Process slot (see taskui.go).
	scope procScope
}

// sdMount tracks a removable device that the import view mounted itself.
// After the import completes (or the user closes the overlay) the mount is
// released via udisksctl so the user can safely pull the card.
type sdMount struct {
	DevicePath string
	MountPath  string
}

// SetProcessRegistry wires the import view to the main-screen process
// bar so each Start Import publishes progress, pause, and cancel
// controls outside the modal.
func (v *ImportView) SetProcessRegistry(r *ProcessRegistry) { v.scope.reg = r }

// NewImportView wires the import overlay.
func NewImportView(invalidate func()) *ImportView {
	v := &ImportView{invalidate: invalidate}
	v.logList.Axis = layout.Vertical
	// Stick the log to the bottom so newly-appended lines stay visible. Once
	// the user manually scrolls up, BeforeEnd flips and Gio stops auto-pinning.
	v.logList.ScrollToEnd = true
	v.addPathEditor.SingleLine = true
	v.addPathEditor.Submit = true
	return v
}

// Show opens the overlay. If an import is already running (because a
// previous Close just hid the window) the existing state — log, progress
// counters, SD mounts — is preserved so the user can keep watching it.
// Otherwise state is reset and the inbox/outbox status line refreshed from
// config.
func (v *ImportView) Show() {
	v.mu.Lock()
	if v.running {
		v.Open = true
		v.mu.Unlock()
		return
	}
	v.Open = true
	v.importDirs = nil
	v.zipFiles = nil
	v.log.reset()
	v.progress.reset()
	v.statMoved = 0
	v.statSkipped = 0
	v.statErrors = 0
	v.running = false
	v.finished = false
	v.sdShown = false
	v.sdDevices = nil
	v.sdError = ""
	v.sdScanning = false
	v.sdBusyDev = ""
	v.sdMounts = nil
	v.sdBtns = nil
	cfg := GetConfig()
	v.deleteCheck.Value = cfg.ImportDeleteSource
	v.deleteSource = v.deleteCheck.Value
	if cfg.InboxDir == "" || cfg.OutboxDir == "" {
		v.statusMsg = "Set InboxDir and OutboxDir in " + configPath() + " before importing."
	} else {
		v.statusMsg = fmt.Sprintf("Inbox: %s   Outbox: %s", cfg.InboxDir, cfg.OutboxDir)
	}
	v.mu.Unlock()
}

// ShowForDevice opens the overlay and queues the given removable device as
// the sole import source, optionally toggling "delete source after import"
// to the requested value. Used by the SD-card prompt to skip the manual
// "click SD Card → click device" dance.
func (v *ImportView) ShowForDevice(dev removableDevice, deleteSource bool) {
	v.Show()
	v.mu.Lock()
	v.deleteCheck.Value = deleteSource
	v.deleteSource = deleteSource
	v.mu.Unlock()
	cfg := GetConfig()
	if cfg.ImportDeleteSource != deleteSource {
		cfg.ImportDeleteSource = deleteSource
		_ = SaveConfig(cfg)
	}
	go v.handleSDDeviceClick(dev)
}

// Close hides the overlay. If an import is currently running it keeps going
// in the background — the user can reopen the overlay via the toolbar to
// watch progress or cancel — and SD-card unmounts are deferred to the
// import's natural completion. When the overlay is closed without a running
// import, any mounts that the view itself created (e.g. the user added an
// SD card but never hit Start) are unmounted now so cards can be pulled.
func (v *ImportView) Close() {
	v.mu.Lock()
	running := v.running
	var mounts []sdMount
	if !running {
		mounts = v.sdMounts
		v.sdMounts = nil
	}
	v.mu.Unlock()
	if len(mounts) > 0 {
		go v.unmountAll(mounts)
	}
	v.Open = false
	if v.OnClose != nil {
		v.OnClose()
	}
}

func (v *ImportView) unmountAll(mounts []sdMount) {
	for _, m := range mounts {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := unmountDevice(ctx, m.DevicePath); err != nil {
			v.appendLog("[WARN] Could not unmount " + m.DevicePath + ": " + err.Error())
		} else {
			v.appendLog("Unmounted " + m.DevicePath + ".")
		}
		cancel()
	}
}

// refreshSDDevices runs lsblk and updates the picker state. Safe to call from
// any goroutine; the UI redraws once the result lands.
func (v *ImportView) refreshSDDevices() {
	v.mu.Lock()
	if v.sdScanning {
		v.mu.Unlock()
		return
	}
	v.sdScanning = true
	v.sdError = ""
	v.mu.Unlock()
	if v.invalidate != nil {
		v.invalidate()
	}
	devices, err := listRemovableDevices()
	v.mu.Lock()
	v.sdScanning = false
	if err != nil {
		v.sdError = err.Error()
		v.sdDevices = nil
	} else {
		v.sdError = ""
		v.sdDevices = devices
	}
	v.mu.Unlock()
	if v.invalidate != nil {
		v.invalidate()
	}
}

// handleSDDeviceClick mounts the selected device (if not already mounted) and
// adds the mount path to importDirs. Devices we mounted ourselves are tracked
// in sdMounts so they get unmounted after the import finishes.
func (v *ImportView) handleSDDeviceClick(dev removableDevice) {
	v.mu.Lock()
	if v.sdBusyDev != "" {
		v.mu.Unlock()
		return
	}
	v.sdBusyDev = dev.Path
	v.sdError = ""
	v.mu.Unlock()
	if v.invalidate != nil {
		v.invalidate()
	}

	mountPath := dev.Mountpoint
	mountedByUs := false
	if mountPath == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		path, err := mountDevice(ctx, dev.Path)
		cancel()
		if err != nil {
			v.mu.Lock()
			v.sdBusyDev = ""
			v.sdError = err.Error()
			v.mu.Unlock()
			v.appendLog("[ERROR] Mount " + dev.Path + ": " + err.Error())
			if v.invalidate != nil {
				v.invalidate()
			}
			return
		}
		mountPath = path
		mountedByUs = true
		v.appendLog("Mounted " + dev.Path + " at " + mountPath + ".")
	} else {
		v.appendLog("Using already-mounted " + dev.Path + " at " + mountPath + ".")
	}

	v.mu.Lock()
	v.sdBusyDev = ""
	already := slices.Contains(v.importDirs, mountPath)
	if !already {
		v.importDirs = append(v.importDirs, mountPath)
	}
	if mountedByUs {
		v.sdMounts = append(v.sdMounts, sdMount{DevicePath: dev.Path, MountPath: mountPath})
	}
	// Refresh the device's mount state in the picker.
	for i := range v.sdDevices {
		if v.sdDevices[i].Path == dev.Path {
			v.sdDevices[i].Mountpoint = mountPath
		}
	}
	v.mu.Unlock()
	if v.invalidate != nil {
		v.invalidate()
	}
}

// setStatus replaces the status line shown above the progress bar. Safe to
// call from any goroutine; the UI redraws on the next frame.
func (v *ImportView) setStatus(msg string) {
	v.mu.Lock()
	v.statusMsg = msg
	proc := v.scope.currentLocked()
	v.mu.Unlock()
	if proc != nil {
		// SetStatus notifies through the registry coalescer; skip the direct
		// invalidate to avoid a redundant second wakeup for the same change.
		proc.SetStatus(msg)
		return
	}
	v.scheduleInvalidate()
}

// waitIfPaused blocks while the import is paused via the process bar.
// Workers call this at loop boundaries; if no process is registered
// (e.g. tests) it's a no-op.
func (v *ImportView) waitIfPaused() {
	v.mu.Lock()
	proc := v.scope.currentLocked()
	v.mu.Unlock()
	if proc != nil {
		proc.Wait()
	}
}

// scheduleInvalidate wakes the Gio frame loop through the process registry's
// ~30Hz coalescer when one is wired up. Import's per-file paths (appendLog,
// setProgress, bumpProgress) fire once per file — thousands/sec during a
// same-device rename — so calling v.invalidate directly from them would peg a
// core repainting a log that scrolls far faster than the eye can read. The
// registry coalesces to ~30Hz with a guaranteed trailing flush, so the final
// log line / progress value still lands. Falls back to the raw invalidate when
// no registry is set (e.g. tests, or one-shot messages before an import
// starts) — those paths aren't in a per-file storm.
func (v *ImportView) scheduleInvalidate() {
	if v.scope.reg != nil {
		v.scope.reg.notify()
		return
	}
	if v.invalidate != nil {
		v.invalidate()
	}
}

func (v *ImportView) appendLog(msg string) {
	v.log.append(msg)
	v.scheduleInvalidate()
}

// Layout draws the overlay.
func (v *ImportView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	v.log.drain()

	if v.closeBtn.Clicked(gtx) {
		v.Close()
	}
	if v.startBtn.Clicked(gtx) && !v.runningSnapshot() {
		// Probe the inbox once, cheaply: inboxHasFiles short-circuits on the
		// first media file instead of counting every leftover (U-10 — this runs
		// on the frame goroutine). Only meaningful once Inbox is configured; both
		// callees gate on that first. Thread the result into both so a Start on
		// an empty inbox doesn't walk it twice.
		cfg := GetConfig()
		inboxHas := cfg.InboxDir != "" && inboxHasFiles(cfg.InboxDir)
		if v.haveSomethingToImport(cfg, inboxHas) {
			v.startImport()
		} else {
			v.explainCantStart(cfg, inboxHas)
		}
	}
	if v.cancelBtn.Clicked(gtx) {
		v.mu.Lock()
		c := v.cancelImport
		v.mu.Unlock()
		if c != nil {
			c()
		}
	}
	if v.addDirBtn.Clicked(gtx) {
		if strings.TrimSpace(v.addPathEditor.Text()) != "" {
			v.commitAddPath(true)
		} else {
			go v.pickFolder()
		}
	}
	if v.addZipBtn.Clicked(gtx) {
		if strings.TrimSpace(v.addPathEditor.Text()) != "" {
			v.commitAddPath(false)
		} else {
			go v.pickZip()
		}
	}
	if v.addSDBtn.Clicked(gtx) {
		v.mu.Lock()
		v.sdShown = true
		v.sdError = ""
		v.mu.Unlock()
		go v.refreshSDDevices()
	}
	if v.sdRefreshBtn.Clicked(gtx) {
		go v.refreshSDDevices()
	}
	if v.sdHideBtn.Clicked(gtx) {
		v.mu.Lock()
		v.sdShown = false
		v.mu.Unlock()
	}
	v.mu.Lock()
	devSnapshot := append([]removableDevice(nil), v.sdDevices...)
	if len(v.sdBtns) < len(devSnapshot) {
		v.sdBtns = append(v.sdBtns, make([]widget.Clickable, len(devSnapshot)-len(v.sdBtns))...)
	}
	v.mu.Unlock()
	for i := range devSnapshot {
		if v.sdBtns[i].Clicked(gtx) {
			dev := devSnapshot[i]
			go v.handleSDDeviceClick(dev)
		}
	}
	for {
		ev, ok := v.addPathEditor.Update(gtx)
		if !ok {
			break
		}
		if _, ok := ev.(widget.SubmitEvent); ok {
			text := strings.TrimSpace(v.addPathEditor.Text())
			if strings.HasSuffix(strings.ToLower(text), ".zip") {
				v.commitAddPath(false)
			} else {
				v.commitAddPath(true)
			}
		}
	}
	if v.deleteCheck.Update(gtx) {
		c := GetConfig()
		c.ImportDeleteSource = v.deleteCheck.Value
		_ = SaveConfig(c)
		v.mu.Lock()
		v.deleteSource = v.deleteCheck.Value
		v.mu.Unlock()
	}

	// Background.
	totalW := gtx.Constraints.Max.X
	totalH := gtx.Constraints.Max.Y
	bg := image.Rectangle{Max: image.Pt(totalW, totalH)}
	clipArea := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.Background}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	clipArea.Pop()

	logVisible := v.log.lines()
	progressDone, progressMax := v.progress.load()
	v.mu.Lock()
	statusMsg := v.statusMsg
	importDirs := append([]string(nil), v.importDirs...)
	zipFiles := append([]string(nil), v.zipFiles...)
	moved := atomic.LoadInt64(&v.statMoved)
	skipped := atomic.LoadInt64(&v.statSkipped)
	errs := atomic.LoadInt64(&v.statErrors)
	running := v.running
	finished := v.finished
	sdShown := v.sdShown
	sdError := v.sdError
	sdScanning := v.sdScanning
	sdBusyDev := v.sdBusyDev
	v.mu.Unlock()

	pad := layout.Inset{Top: unit.Dp(12), Bottom: unit.Dp(12), Left: unit.Dp(12), Right: unit.Dp(12)}
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				title := material.H6(th.Theme, "Import")
				title.Color = th.Foreground
				return title.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutStatus(gtx, th, statusMsg, running, finished, moved, skipped, errs)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutAddRow(gtx, th)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if !sdShown {
					return layout.Dimensions{}
				}
				return layout.Inset{Top: unit.Dp(8)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return v.layoutSDPicker(gtx, th, devSnapshot, sdError, sdScanning, sdBusyDev)
				})
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutSources(gtx, th, importDirs, zipFiles)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				cb := material.CheckBox(th.Theme, &v.deleteCheck, "Delete source files after import")
				cb.Color = th.Foreground
				return cb.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layoutProgressBar(gtx, th, progressDone, progressMax, true)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutCounters(gtx, th, progressDone, progressMax, moved, skipped, errs)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return v.layoutButtons(gtx, th, running)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return v.layoutLog(gtx, th, logVisible)
			}),
		)
	})
}

func (v *ImportView) layoutAddRow(gtx layout.Context, th *Theme) layout.Dimensions {
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			v.addPathEditor.SingleLine = true
			ed := material.Editor(th.Theme, &v.addPathEditor, "Path to source dir or .zip…")
			ed.Color = th.Foreground
			ed.HintColor = th.Muted
			return drawEditorBox(gtx, th.CellBG, layout.UniformInset(unit.Dp(8)), ed.Layout)
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
		layout.Rigid(material.Button(th.Theme, &v.addDirBtn, "Add Folder").Layout),
		layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
		layout.Rigid(material.Button(th.Theme, &v.addZipBtn, "Add ZIP").Layout),
		layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
		layout.Rigid(material.Button(th.Theme, &v.addSDBtn, "SD Card").Layout),
	)
}

// layoutSDPicker renders the inline removable-device picker shown after the
// user clicks the "SD Card" button. Each device is a clickable row; clicking
// one mounts it (in the background) and adds the mount point as an import
// source. The header row exposes Refresh and Hide.
func (v *ImportView) layoutSDPicker(gtx layout.Context, th *Theme, devices []removableDevice, errMsg string, scanning bool, busyDev string) layout.Dimensions {
	header := func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				msg := "Removable devices — click one to mount and add as a source."
				if scanning {
					msg = "Scanning for removable devices…"
				} else if errMsg != "" {
					msg = "Error: " + errMsg
				} else if len(devices) == 0 {
					msg = "No removable devices found. Insert an SD card or USB drive and click Refresh."
				}
				lbl := material.Label(th.Theme, unit.Sp(12), msg)
				lbl.Color = th.Foreground
				lbl.MaxLines = 2
				return lbl.Layout(gtx)
			}),
			layout.Rigid(material.Button(th.Theme, &v.sdRefreshBtn, "Refresh").Layout),
			layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
			layout.Rigid(material.Button(th.Theme, &v.sdHideBtn, "Hide").Layout),
		)
	}
	rows := []layout.FlexChild{layout.Rigid(header)}
	for i, dev := range devices {
		if i >= len(v.sdBtns) {
			break
		}
		btn := &v.sdBtns[i]
		dev := dev
		label := dev.Display()
		if dev.Path == busyDev {
			label = "⏳ " + label + " — mounting…"
		} else {
			label = "💾 " + label
		}
		rows = append(rows,
			layout.Rigid(layout.Spacer{Height: unit.Dp(4)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				b := material.Button(th.Theme, btn, label)
				b.Background = th.CellBG
				b.Color = th.Foreground
				return b.Layout(gtx)
			}),
		)
	}
	return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
	})
}

func (v *ImportView) layoutSources(gtx layout.Context, th *Theme, dirs, zips []string) layout.Dimensions {
	if len(dirs) == 0 && len(zips) == 0 {
		lbl := material.Label(th.Theme, unit.Sp(12), "No sources added. The inbox alone is also a valid source.")
		lbl.Color = th.Foreground
		return lbl.Layout(gtx)
	}
	rows := []layout.FlexChild{}
	for _, p := range dirs {
		text := "📁 " + p
		rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(12), text)
			lbl.Color = th.Foreground
			lbl.MaxLines = 1
			return lbl.Layout(gtx)
		}))
	}
	for _, p := range zips {
		text := "🗜  " + p
		rows = append(rows, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(12), text)
			lbl.Color = th.Foreground
			lbl.MaxLines = 1
			return lbl.Layout(gtx)
		}))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, rows...)
}

func (v *ImportView) layoutButtons(gtx layout.Context, th *Theme, running bool) layout.Dimensions {
	startLabel := "Start Import"
	if running {
		startLabel = "Running…"
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(material.Button(th.Theme, &v.closeBtn, "Close (esc)").Layout),
		layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			b := material.Button(th.Theme, &v.startBtn, startLabel)
			if running {
				// Visually subdue the button while a run is in flight; the
				// click handler in Layout already gates on runningSnapshot.
				b.Background = th.CellBG
				b.Color = th.Muted
			}
			return b.Layout(gtx)
		}),
		layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			b := material.Button(th.Theme, &v.cancelBtn, "Cancel")
			if !running {
				b.Background = th.CellBG
				b.Color = th.Muted
			}
			return b.Layout(gtx)
		}),
	)
}

// layoutStatus draws the prominent status row at the top of the overlay. It
// shows a colored dot indicating idle/running/done state and the latest phase
// message. Multi-line so a long path doesn't get truncated.
func (v *ImportView) layoutStatus(gtx layout.Context, th *Theme, msg string, running, finished bool, moved, skipped, errs int64) layout.Dimensions {
	prefix := "● Idle  "
	color := th.Muted
	if running {
		prefix = "● Running  "
		color = th.Accent
	} else if finished {
		if errs > 0 {
			prefix = "● Finished (with errors)  "
		} else {
			prefix = "● Finished  "
		}
		color = th.Foreground
	}
	_ = moved
	_ = skipped
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(12), prefix)
			lbl.Color = color
			lbl.Font.Weight = 700
			return lbl.Layout(gtx)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			lbl := material.Label(th.Theme, unit.Sp(12), msg)
			lbl.Color = th.Foreground
			lbl.MaxLines = 2
			return lbl.Layout(gtx)
		}),
	)
}

// layoutCounters draws the live counts under the progress bar so the user
// can tell at a glance whether anything is happening and what the outcome
// has been so far.
func (v *ImportView) layoutCounters(gtx layout.Context, th *Theme, done, max, moved, skipped, errs int64) layout.Dimensions {
	var msg string
	if max > 0 {
		msg = fmt.Sprintf("Batch: %d / %d   •   Moved: %d   •   Skipped: %d   •   Errors: %d",
			done, max, moved, skipped, errs)
	} else if moved+skipped+errs > 0 {
		msg = fmt.Sprintf("Moved: %d   •   Skipped: %d   •   Errors: %d", moved, skipped, errs)
	} else {
		msg = "No batch in progress."
	}
	lbl := material.Label(th.Theme, unit.Sp(11), msg)
	if errs > 0 {
		lbl.Color = th.Foreground
	} else {
		lbl.Color = th.Muted
	}
	return lbl.Layout(gtx)
}

func (v *ImportView) layoutLog(gtx layout.Context, th *Theme, lines []string) layout.Dimensions {
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
			lbl := material.Label(th.Theme, unit.Sp(14), lines[i])
			lbl.Color = th.Foreground
			lbl.MaxLines = 1
			return lbl.Layout(gtx)
		})
	})
}

// pickFolder shells out to zenity for a folder picker, then adds the result
// to importDirs. Runs on a goroutine since zenity blocks until the user
// dismisses the dialog. Falls back silently if zenity is missing — the user
// can still type a path into the editor.
func (v *ImportView) pickFolder() {
	out, err := runZenity("--file-selection", "--multiple", "--directory", "--title=Select folder(s) to import")
	if err != nil || out == "" {
		if err != nil {
			v.appendLog("[ERROR] Folder picker failed: " + err.Error() + " (type a path into the field instead)")
		}
		return
	}
	paths := strings.Split(out, "|")
	v.mu.Lock()
	added := false
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !slices.Contains(v.importDirs, path) {
			v.importDirs = append(v.importDirs, path)
			added = true
		}
	}
	v.mu.Unlock()
	if added && v.invalidate != nil {
		v.invalidate()
	}
}

// pickZip shells out to zenity for a ZIP file picker.
func (v *ImportView) pickZip() {
	out, err := runZenity("--file-selection", "--multiple", "--title=Select ZIP(s) to import", "--file-filter=ZIP archives | *.zip *.ZIP")
	if err != nil || out == "" {
		if err != nil {
			v.appendLog("[ERROR] File picker failed: " + err.Error() + " (type a path into the field instead)")
		}
		return
	}
	paths := strings.Split(out, "|")
	v.mu.Lock()
	added := false
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !slices.Contains(v.zipFiles, path) {
			v.zipFiles = append(v.zipFiles, path)
			added = true
		}
	}
	v.mu.Unlock()
	if added && v.invalidate != nil {
		v.invalidate()
	}
}

// runZenity invokes zenity with the given args and returns the trimmed stdout.
// Exit code 1 is "user cancelled" — return empty string with no error.
func runZenity(args ...string) (string, error) {
	cmd := exec.Command("zenity", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// commitAddPath consumes the editor input. asDir=true treats it as a folder,
// asDir=false treats it as a ZIP file.
func (v *ImportView) commitAddPath(asDir bool) {
	text := strings.TrimSpace(v.addPathEditor.Text())
	if text == "" {
		return
	}
	v.addPathEditor.SetText("")
	v.mu.Lock()
	defer v.mu.Unlock()
	if asDir {
		if slices.Contains(v.importDirs, text) {
			return
		}
		v.importDirs = append(v.importDirs, text)
	} else {
		if slices.Contains(v.zipFiles, text) {
			return
		}
		v.zipFiles = append(v.zipFiles, text)
	}
}

func (v *ImportView) runningSnapshot() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.running
}

// explainCantStart logs (and posts to the status line) why a Start Import
// click had no effect — usually because inbox/outbox are not configured, or
// no sources have been added. Without this feedback the button looks broken.
// cfg and inboxHas are computed once by the Start-click handler and threaded in
// so this never re-reads config or re-walks the inbox (U-10).
func (v *ImportView) explainCantStart(cfg Config, inboxHas bool) {
	if cfg.InboxDir == "" || cfg.OutboxDir == "" {
		msg := "Set Inbox and Outbox directories in Settings first (config: " + configPath() + ")."
		v.setStatus(msg)
		v.appendLog("[ERROR] " + msg)
		return
	}
	v.mu.Lock()
	hasDirs := len(v.importDirs) > 0
	hasZips := len(v.zipFiles) > 0
	v.mu.Unlock()
	if !hasDirs && !hasZips && !inboxHas {
		msg := "Nothing to import — add a folder, ZIP, or SD card and try again."
		v.setStatus(msg)
		v.appendLog("[ERROR] " + msg)
	}
}

// haveSomethingToImport reports whether a Start Import click has anything to
// act on. cfg and inboxHas are supplied by the caller (which already probed the
// inbox once via inboxHasFiles) so the ≥1-file decision costs no extra walk.
func (v *ImportView) haveSomethingToImport(cfg Config, inboxHas bool) bool {
	if cfg.InboxDir == "" || cfg.OutboxDir == "" {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.importDirs) > 0 || len(v.zipFiles) > 0 || inboxHas
}

// inboxHasFiles reports whether dir holds at least one importable media file.
// It short-circuits: the WalkDir callback returns fs.SkipAll on the first hit,
// so a huge inbox costs O(1) in the common "has files" case instead of a full
// recursive count — this runs on the frame goroutine, so a stall here shows up
// as a dropped frame per Start click (U-10). Only DetectType != TypeUnknown
// files count, matching what phase-2 processing actually files, so a stray
// .DS_Store in an otherwise-empty inbox doesn't claim "something to import".
func inboxHasFiles(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if scan.DetectType(p) != scan.TypeUnknown {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// startImport kicks off a background goroutine that runs the same three-phase
// flow as the Fyne import. Updates progressDone/progressMax/log atomically;
// the UI polls them at frame time.
func (v *ImportView) startImport() {
	cfg := GetConfig()
	if cfg.InboxDir == "" || cfg.OutboxDir == "" {
		v.appendLog("[ERROR] InboxDir/OutboxDir not configured.")
		return
	}

	v.mu.Lock()
	if v.running {
		v.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	v.cancelImport = cancel
	v.running = true
	v.finished = false
	importDirs := append([]string(nil), v.importDirs...)
	zipFiles := append([]string(nil), v.zipFiles...)
	deleteSrc := v.deleteSource
	// Register with the process bar so the user can pause / cancel the import
	// even when the modal is closed. Begin only touches the registry's own
	// locks (the cancel callback runs later, on user click), so it is safe to
	// call while holding v.mu — and attachLocked keeps the one proc-slot write
	// guarded like every other access.
	if proc := v.scope.begin(ProcImport, "Import", func() {
		v.mu.Lock()
		c := v.cancelImport
		v.mu.Unlock()
		if c != nil {
			c()
		}
	}); proc != nil {
		v.scope.attachLocked(proc, nil)
	}
	v.mu.Unlock()
	atomic.StoreInt64(&v.statMoved, 0)
	atomic.StoreInt64(&v.statSkipped, 0)
	atomic.StoreInt64(&v.statErrors, 0)
	v.progress.reset()

	go func() {
		defer cancel()
		defer func() {
			v.mu.Lock()
			v.running = false
			v.finished = true
			v.cancelImport = nil
			// Unmount any devices we mounted ourselves now that we're done
			// reading from them. The user can then safely pull the card.
			mounts := v.sdMounts
			v.sdMounts = nil
			proc := v.scope.currentLocked()
			v.scope.detachLocked(nil)
			v.mu.Unlock()
			if proc != nil {
				proc.End()
			}
			if len(mounts) > 0 {
				v.unmountAll(mounts)
			}
			if v.invalidate != nil {
				v.invalidate()
			}
		}()
		v.runImport(ctx, cfg, importDirs, zipFiles, deleteSrc)
	}()
}

// runImport mirrors the Fyne flow: extract zips → process inbox → walk dirs.
func (v *ImportView) runImport(ctx context.Context, cfg Config, importDirs, zipFiles []string, deleteSrc bool) {
	v.setStatus("Starting…")
	v.appendLog("Starting import to " + cfg.OutboxDir)

	// Phase 1: extract zips into the inbox.
	for _, z := range zipFiles {
		if ctx.Err() != nil {
			break
		}
		v.setStatus("Extracting ZIP: " + filepath.Base(z))
		v.appendLog("Extracting " + z + " ...")
		extracted := v.extractZipToInbox(ctx, z, cfg.InboxDir)
		v.appendLog(fmt.Sprintf("Extracted %d media files from %s.", len(extracted), filepath.Base(z)))
	}

	// Phase 2: process whatever's in the inbox.
	if ctx.Err() == nil {
		v.setStatus("Scanning Inbox…")
		var inbox []string
		_ = filepath.WalkDir(cfg.InboxDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if scan.DetectType(p) != scan.TypeUnknown {
				inbox = append(inbox, p)
			}
			return nil
		})
		if len(inbox) > 0 {
			v.setStatus(fmt.Sprintf("Processing %d files from Inbox…", len(inbox)))
			v.appendLog(fmt.Sprintf("Processing %d files already in Inbox...", len(inbox)))
			v.setProgress(0, int64(len(inbox)))
			v.processBatch(ctx, cfg.OutboxDir, inbox)
		} else {
			v.appendLog("Inbox is empty — nothing to process.")
		}
	}

	// Phase 3: walk source directories one subdir at a time.
	for _, src := range importDirs {
		if ctx.Err() != nil {
			break
		}
		v.setStatus("Scanning " + src + " …")
		v.appendLog("Scanning " + src + " ...")
		subdirFiles := map[string][]string{}
		var subdirOrder []string
		var zipsInDir []string
		_ = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if strings.EqualFold(filepath.Ext(p), ".zip") {
				zipsInDir = append(zipsInDir, p)
				return nil
			}
			if scan.DetectType(p) == scan.TypeUnknown {
				return nil
			}
			parent := filepath.Dir(p)
			if _, ok := subdirFiles[parent]; !ok {
				subdirOrder = append(subdirOrder, parent)
			}
			subdirFiles[parent] = append(subdirFiles[parent], p)
			return nil
		})

		totalFound := 0
		for _, files := range subdirFiles {
			totalFound += len(files)
		}
		if totalFound == 0 && len(zipsInDir) == 0 {
			v.appendLog("[WARN] No supported media or ZIPs found under " + src + ".")
		} else {
			v.appendLog(fmt.Sprintf("Found %d media files (and %d ZIPs) across %d subfolders in %s.",
				totalFound, len(zipsInDir), len(subdirOrder), src))
		}

		for _, z := range zipsInDir {
			if ctx.Err() != nil {
				break
			}
			v.setStatus("Extracting ZIP: " + filepath.Base(z))
			v.appendLog("Extracting " + z + " ...")
			extracted := v.extractZipToInbox(ctx, z, cfg.InboxDir)
			if len(extracted) == 0 {
				continue
			}
			v.setStatus(fmt.Sprintf("Processing %d files from %s…", len(extracted), filepath.Base(z)))
			v.setProgress(0, int64(len(extracted)))
			v.processBatch(ctx, cfg.OutboxDir, extracted)
			if deleteSrc {
				if err := os.Remove(z); err != nil {
					atomic.AddInt64(&v.statErrors, 1)
					v.appendLog("[ERROR] Failed to delete source ZIP " + z + ": " + err.Error())
				}
			}
		}

		for _, sd := range subdirOrder {
			if ctx.Err() != nil {
				break
			}
			files := subdirFiles[sd]
			// Fast path: when the source subdirectory and the Outbox live on
			// the same filesystem AND the user wants the source gone, skip
			// the Inbox copy entirely — processBatch will os.Rename each
			// source straight into Outbox/YYYY-MM-DD/, which is atomic and
			// free on the same device.
			if deleteSrc && sameDevice(sd, cfg.OutboxDir) {
				v.setStatus(fmt.Sprintf("Filing %d files from %s directly into %s…", len(files), sd, filepath.Base(cfg.OutboxDir)))
				v.appendLog(fmt.Sprintf("Same-device shortcut: moving %d files from %s straight to Outbox (no Inbox copy).", len(files), sd))
				// processBatch owns the progress bar (it spans a date-read pass
				// and a move pass), so no setProgress here.
				v.processBatch(ctx, cfg.OutboxDir, files)
				continue
			}
			v.setStatus(fmt.Sprintf("Copying %d files from %s…", len(files), sd))
			v.appendLog(fmt.Sprintf("Copying %d files from %s to Inbox...", len(files), sd))
			// This Inbox copy moves every byte off the card, so it's the longest
			// phase on a slow SD reader. Own the progress bar across it — before
			// U-09 the loop never touched progressDone/progressMax, so the bar
			// held the previous batch's terminal value (a stale "100%") until
			// processBatch reset it, reading as a multi-minute hang. bumpProgress
			// runs once per file (success or copy error) so the bar climbs
			// monotonically to len(files); processBatch re-owns the bar below.
			v.setProgress(0, int64(len(files)))
			var batch []string
			for _, srcFile := range files {
				v.waitIfPaused()
				if ctx.Err() != nil {
					break
				}
				dest := uniqueInboxPath(cfg.InboxDir, filepath.Base(srcFile))
				if err := copyFile(srcFile, dest); err != nil {
					atomic.AddInt64(&v.statErrors, 1)
					v.appendLog("[ERROR] Copy failed for " + srcFile + ": " + err.Error())
					v.bumpProgress()
					continue
				}
				batch = append(batch, dest)
				if deleteSrc {
					// copyFile fsynced the Inbox copy's data; fsync the Inbox
					// directory too so its entry is durable before we delete the
					// source (the only other copy). On sync failure, keep the
					// source rather than risk zero copies — the Inbox copy is
					// already queued in batch, so no bytes are lost either way.
					if err := syncDir(filepath.Dir(dest)); err != nil {
						atomic.AddInt64(&v.statErrors, 1)
						v.appendLog("[ERROR] Failed to fsync Inbox before deleting source " + srcFile + ": " + err.Error())
					} else if err := os.Remove(srcFile); err != nil {
						atomic.AddInt64(&v.statErrors, 1)
						v.appendLog("[ERROR] Failed to delete source " + srcFile + ": " + err.Error())
					}
				}
				v.bumpProgress()
			}
			if len(batch) == 0 {
				continue
			}
			v.setStatus(fmt.Sprintf("Filing %d files into %s…", len(batch), filepath.Base(cfg.OutboxDir)))
			// processBatch owns the progress bar across its date-read + move
			// passes, so no setProgress here.
			v.processBatch(ctx, cfg.OutboxDir, batch)
		}
	}

	moved := atomic.LoadInt64(&v.statMoved)
	skipped := atomic.LoadInt64(&v.statSkipped)
	errs := atomic.LoadInt64(&v.statErrors)
	if ctx.Err() != nil {
		summary := fmt.Sprintf("Cancelled. Moved %d • skipped %d • errors %d.", moved, skipped, errs)
		v.appendLog(summary)
		v.setStatus(summary)
	} else if moved == 0 && skipped == 0 && errs == 0 {
		summary := "Finished — nothing to import. Add a folder, ZIP, or SD card and try again."
		v.appendLog(summary)
		v.setStatus(summary)
	} else {
		summary := fmt.Sprintf("Done. Moved %d • skipped %d (already in Outbox) • errors %d.", moved, skipped, errs)
		v.appendLog(summary)
		v.setStatus(summary)
	}
}

// processBatch is the Fyne flow: read EXIF, group by base name, move into the
// outbox under YYYY-MM-DD folders.
func (v *ImportView) processBatch(ctx context.Context, outboxDir string, entries []string) {
	if len(entries) == 0 {
		return
	}
	// The batch is visited twice — once to read every file's date, once to move
	// it — so the progress bar spans 2*len(entries). The date probes fork
	// exiftool per file (~50–200 ms each); running them strictly sequentially
	// stalled a multi-thousand-file card at 0% for minutes, so they go through a
	// small worker pool (mirroring internal/ui/organize.go's scan pass) that
	// bumps the bar as it reads.
	v.setProgress(0, int64(2*len(entries)))
	v.setStatus(fmt.Sprintf("Reading dates from %d files…", len(entries)))

	baseDates := make(map[string]time.Time)
	// Files whose mtime predates their declared EXIF/metadata date — rewrite
	// the metadata to mtime after we've decided the destination so subsequent
	// scans see consistent dates.
	type mtimeFix struct {
		path string
		when time.Time
	}
	var mtimeFixes []mtimeFix

	// Phase 1: read every file's date concurrently. Results are indexed by
	// input position so the fold below stays deterministic.
	type dateResult struct {
		oldest     time.Time
		mtimeOlder bool
		ok         bool
	}
	results := make([]dateResult, len(entries))
	jobs := make(chan int, len(entries))
	numWorkers := runtime.NumCPU()
	numWorkers = min(numWorkers, 4) // cap the concurrent exiftool forks
	var wg sync.WaitGroup
	for range numWorkers {
		wg.Go(func() {
			for idx := range jobs {
				v.waitIfPaused()
				if ctx.Err() != nil {
					return
				}
				oldest, _, mtimeOlder := scan.GetOldestMediaDate(entries[idx])
				results[idx] = dateResult{oldest: oldest, mtimeOlder: mtimeOlder, ok: true}
				v.bumpProgress()
			}
		})
	}
	for i := range entries {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	// Fold per-file results in input order so the "oldest date wins per base
	// name" tie-break is deterministic.
	for i, src := range entries {
		r := results[i]
		if !r.ok {
			continue
		}
		if r.mtimeOlder {
			mtimeFixes = append(mtimeFixes, mtimeFix{path: src, when: r.oldest})
		}
		baseName := filepath.Base(src)
		base := strings.TrimSuffix(baseName, filepath.Ext(baseName))
		if existing, ok := baseDates[base]; !ok || r.oldest.Before(existing) {
			baseDates[base] = r.oldest
		}
	}
	v.setStatus(fmt.Sprintf("Filing %d files into %s…", len(entries), filepath.Base(outboxDir)))
	for _, fix := range mtimeFixes {
		if ctx.Err() != nil {
			break
		}
		if err := scan.SetMediaDate(fix.path, fix.when); err != nil {
			v.appendLog("[WARN] Could not rewrite creation date on " + filepath.Base(fix.path) + ": " + err.Error())
		} else {
			v.appendLog("Rewrote creation date on " + filepath.Base(fix.path) + " to " + fix.when.Format("2006-01-02"))
		}
	}

	for _, src := range entries {
		v.waitIfPaused()
		if ctx.Err() != nil {
			return
		}
		baseName := filepath.Base(src)
		base := strings.TrimSuffix(baseName, filepath.Ext(baseName))
		dateFolder := baseDates[base].Format("2006-01-02")
		destDir := filepath.Join(outboxDir, dateFolder)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			atomic.AddInt64(&v.statErrors, 1)
			v.appendLog(fmt.Sprintf("[ERROR] mkdir %s: %v", destDir, err))
			continue
		}
		dest := filepath.Join(destDir, baseName)
		if _, err := os.Stat(dest); err == nil {
			same, _ := sameContent(src, dest)
			if same {
				if err := os.Remove(src); err != nil {
					atomic.AddInt64(&v.statErrors, 1)
					v.appendLog("[ERROR] Could not remove duplicate " + baseName + ": " + err.Error())
				} else {
					atomic.AddInt64(&v.statSkipped, 1)
					v.appendLog(fmt.Sprintf("[SKIP] %s already in %s (identical)", baseName, dateFolder))
				}
				v.bumpProgress()
				continue
			}
			ext := filepath.Ext(baseName)
			dest = filepath.Join(destDir, fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext))
		}
		moveErr := os.Rename(src, dest)
		if moveErr != nil && errors.Is(moveErr, syscall.EXDEV) {
			// os.Rename can't cross filesystems (EXDEV): when the inbox and
			// outbox live on different mounts, every move would otherwise
			// fail and the files would stay stuck in the inbox. Fall back to
			// a crash-safe copy + remove so they still land in the library.
			// Gate strictly on EXDEV so genuine errors (permission denied,
			// disk full) still surface as failures instead of silent copies.
			if moveErr = copyFile(src, dest); moveErr == nil {
				// Fsync the destination directory so the copy's directory entry
				// is durable before we unlink the source — otherwise a power
				// loss could lose the entry even though copyFile synced the
				// data, leaving zero copies of the file.
				if moveErr = syncDir(filepath.Dir(dest)); moveErr == nil {
					moveErr = os.Remove(src)
				}
			}
		}
		if moveErr != nil {
			atomic.AddInt64(&v.statErrors, 1)
			v.appendLog("[ERROR] Could not move " + baseName + ": " + moveErr.Error())
		} else {
			atomic.AddInt64(&v.statMoved, 1)
			v.appendLog(fmt.Sprintf("[OK] Moved %s -> %s", baseName, filepath.Join(dateFolder, filepath.Base(dest))))
		}
		v.bumpProgress()
	}
}

func (v *ImportView) extractZipToInbox(ctx context.Context, zipPath, inboxDir string) []string {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		v.appendLog("[ERROR] Failed to open ZIP: " + err.Error())
		return nil
	}
	defer r.Close()
	var extracted []string
	for _, f := range r.File {
		// Honour Pause / Cancel between archive entries so a multi-GB ZIP
		// stops promptly instead of extracting to completion after the user
		// hit Cancel. (writeFileDurable below only publishes an entry via an
		// atomic rename after fsync, so an interrupted io.Copy leaves at most a
		// .tmp — never a truncated file at the final name — and an early return
		// here leaves nothing behind.)
		v.waitIfPaused()
		if ctx.Err() != nil {
			return extracted
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if scan.DetectType(f.Name) == scan.TypeUnknown {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			v.appendLog("[ERROR] Failed to open file in ZIP: " + err.Error())
			continue
		}
		dest := uniqueInboxPath(inboxDir, filepath.Base(f.Name))
		// Crash-safe extract: writeFileDurable streams into dest+".tmp",
		// fsyncs, and atomically renames — so an interrupted extraction (cancel,
		// full disk, power loss) leaves at most the .tmp, never a truncated file
		// at dest that the next import's inbox walk would file into the library
		// as valid media.
		err = writeFileDurable(dest, rc)
		rc.Close()
		if err != nil {
			v.appendLog("[ERROR] Failed to extract file: " + err.Error())
			continue
		}
		extracted = append(extracted, dest)
	}
	return extracted
}

func (v *ImportView) setProgress(done, max int64) {
	v.progress.set(done, max)
	v.mu.Lock()
	proc := v.scope.currentLocked()
	v.mu.Unlock()
	if proc != nil {
		// proc.SetDone/SetTotal already route through the registry's ~30Hz
		// coalescer, so no direct v.invalidate() here — that would re-add the
		// per-file storm this initiative removes.
		proc.SetDone(done)
		proc.SetTotal(max)
		return
	}
	v.scheduleInvalidate()
}

func (v *ImportView) bumpProgress() {
	v.progress.bump()
	v.mu.Lock()
	proc := v.scope.currentLocked()
	v.mu.Unlock()
	if proc != nil {
		// proc.AddDone already coalesces the redraw through the registry; a
		// direct v.invalidate() per file is exactly the storm we're killing.
		proc.AddDone(1)
		return
	}
	v.scheduleInvalidate()
}

// File helpers (mirrored from internal/ui/import.go).

func uniqueInboxPath(dir, base string) string {
	p := filepath.Join(dir, base)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s_%d%s", name, i, ext))
		if _, err := os.Stat(c); os.IsNotExist(err) {
			return c
		}
	}
}

// sameDevice reports whether two paths live on the same filesystem. Used to
// decide whether os.Rename can move a file between them atomically. Any stat
// failure returns false so callers fall back to the safe copy path.
func sameDevice(a, b string) bool {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false
	}
	return sa.Dev == sb.Dev
}

// copyFile copies src to dst crash-safely: it streams into a sibling
// dst+".tmp", fsyncs it to stable storage, and only then renames it into
// place. This closes two data-safety holes. First, a yanked SD card or full
// disk mid-copy leaves at most the .tmp (removed here on any error), never a
// truncated file at dst — otherwise the next import's inbox walk would file
// that partial file into the library as if it were valid. Second, the Sync
// before the rename backstops "Delete source after import": the caller's
// os.Remove(src) must not run while dst's bytes are still only in the page
// cache, or a power loss would destroy the sole copy. (dst's .tmp is a real
// *os.File, so plain io.Copy still gets the kernel copy_file_range/sendfile
// fast path — no pooled buffer needed.)
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFileDurable(dst, in)
}

// syncDir fsyncs a directory so a rename/create within it is itself durable.
// copyFile/writeFileDurable fsync the file's *data* before the rename, but the
// rename only adds a directory entry — on a crash that entry can still be lost
// even though the bytes survived. When copyFile is used as a *move* (the source
// is unlinked afterwards), that lost entry would leave zero copies, so callers
// fsync the destination directory before dropping the source. Mirrors
// internal/export/favorites.go's syncDir.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeFileDurable streams r into dst crash-safely: it writes into a sibling
// dst+".tmp", fsyncs it to stable storage, and only then atomically renames it
// into place. It is the writer half of copyFile, shared so any producer of new
// inbox bytes (a plain file copy, a ZIP entry) gets the same guarantee: an
// interrupted write — yanked card, full disk, power loss, cancel — leaves at
// most the .tmp (removed here on any error), never a truncated file at the final
// name that the next import's inbox walk would file into the library as valid
// media. The Sync before the rename also backstops "Delete source after
// import": a caller must not os.Remove the source while dst's bytes are still
// only in the page cache. (dst's .tmp is a real *os.File, so plain io.Copy keeps
// the kernel copy_file_range/sendfile fast path — no pooled buffer needed.)
func writeFileDurable(dst string, r io.Reader) error {
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		os.Remove(tmp) // best-effort: never leave a partial temp behind
		return err
	}
	// A failed Sync is exactly the "bytes still only in the page cache" case,
	// so treat it as a copy failure: drop the temp and report the error so no
	// caller deletes the source believing the copy is durable.
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Rename is atomic within a filesystem: dst either doesn't exist or is the
	// fully-written, fsynced file — never a half-copy.
	return os.Rename(tmp, dst)
}

// sameContent reports whether two files hold identical bytes. It short-circuits
// cheaply on a size mismatch, then compares the contents block-by-block and
// early-exits on the first differing byte (U-12) — the previous implementation
// SHA-256'd both files in full, always reading both to EOF even when they
// differed in the first block, which made re-importing an already-filed card
// pay two full-file reads per duplicate just to skip it. Any stat/open/read
// error is surfaced to the caller unchanged.
func sameContent(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if ai.Size() != bi.Size() {
		return false, nil
	}

	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()

	// Read fixed-size blocks from both files in lockstep. io.ReadFull fills the
	// whole buffer (so blocks stay aligned across the two files despite short OS
	// reads) except at end-of-file, where it returns io.EOF (block boundary) or
	// io.ErrUnexpectedEOF (short final block). Because the sizes already match,
	// the two streams end together.
	const block = 64 * 1024
	bufA := make([]byte, block)
	bufB := make([]byte, block)
	for {
		na, ea := io.ReadFull(fa, bufA)
		nb, eb := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		if ea != nil || eb != nil {
			// End of at least one file, or a genuine read error. Surface any
			// error that isn't an expected EOF; otherwise both files are
			// exhausted with every block equal, so they're identical.
			for _, e := range []error{ea, eb} {
				if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
					return false, e
				}
			}
			return true, nil
		}
	}
}
