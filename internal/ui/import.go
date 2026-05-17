package ui

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
	logVisible   []string
	progressDone int64
	progressMax  int64
	statMoved    int64
	statSkipped  int64
	statErrors   int64
	running      bool
	finished     bool // at least one import has completed since the overlay opened
	deleteSource bool
	cancelImport context.CancelFunc

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

	// Buffered log lines from worker goroutines, drained into logVisible by
	// the UI thread once per frame to avoid mutex thrash.
	logBuf  []string
	logFull strings.Builder

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

	// processes lets the import publish progress to the main-screen
	// process bar so the user can pause / resume / cancel without
	// opening the modal.
	processes *ProcessRegistry
	proc      *Process
}

const (
	importLogVisibleLines = 1000
)

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
func (v *ImportView) SetProcessRegistry(r *ProcessRegistry) { v.processes = r }

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
	v.logVisible = nil
	v.logBuf = nil
	v.logFull.Reset()
	v.progressDone = 0
	v.progressMax = 0
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
	v.deleteCheck.Value = GetConfig().ImportDeleteSource
	v.deleteSource = v.deleteCheck.Value
	cfg := GetConfig()
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
	already := false
	for _, p := range v.importDirs {
		if p == mountPath {
			already = true
			break
		}
	}
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
	proc := v.proc
	v.mu.Unlock()
	if proc != nil {
		proc.SetStatus(msg)
	}
	if v.invalidate != nil {
		v.invalidate()
	}
}

// waitIfPaused blocks while the import is paused via the process bar.
// Workers call this at loop boundaries; if no process is registered
// (e.g. tests) it's a no-op.
func (v *ImportView) waitIfPaused() {
	v.mu.Lock()
	proc := v.proc
	v.mu.Unlock()
	if proc != nil {
		proc.Wait()
	}
}

func (v *ImportView) appendLog(msg string) {
	v.mu.Lock()
	v.logBuf = append(v.logBuf, msg)
	v.logFull.WriteString(msg)
	v.logFull.WriteByte('\n')
	v.mu.Unlock()
	if v.invalidate != nil {
		v.invalidate()
	}
}

// drainLog moves buffered lines into logVisible (capped). Called from the UI
// goroutine so logVisible can be read without locking during render.
func (v *ImportView) drainLog() {
	v.mu.Lock()
	if len(v.logBuf) > 0 {
		v.logVisible = append(v.logVisible, v.logBuf...)
		v.logBuf = v.logBuf[:0]
		if len(v.logVisible) > importLogVisibleLines {
			v.logVisible = v.logVisible[len(v.logVisible)-importLogVisibleLines:]
		}
	}
	v.mu.Unlock()
}

// Layout draws the overlay.
func (v *ImportView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	v.drainLog()

	if v.closeBtn.Clicked(gtx) {
		v.Close()
	}
	if v.startBtn.Clicked(gtx) && !v.runningSnapshot() {
		if v.haveSomethingToImport() {
			v.startImport()
		} else {
			v.explainCantStart()
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

	v.mu.Lock()
	statusMsg := v.statusMsg
	importDirs := append([]string(nil), v.importDirs...)
	zipFiles := append([]string(nil), v.zipFiles...)
	logVisible := v.logVisible
	progressDone := atomic.LoadInt64(&v.progressDone)
	progressMax := atomic.LoadInt64(&v.progressMax)
	moved := atomic.LoadInt64(&v.statMoved)
	skipped := atomic.LoadInt64(&v.statSkipped)
	errs := atomic.LoadInt64(&v.statErrors)
	running := v.running
	finished := v.finished
	sdShown := v.sdShown
	sdDevices := append([]removableDevice(nil), v.sdDevices...)
	sdError := v.sdError
	sdScanning := v.sdScanning
	sdBusyDev := v.sdBusyDev
	// Resize the per-row clickables to match the device count.
	if len(v.sdBtns) < len(v.sdDevices) {
		v.sdBtns = append(v.sdBtns, make([]widget.Clickable, len(v.sdDevices)-len(v.sdBtns))...)
	}
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
					return v.layoutSDPicker(gtx, th, sdDevices, sdError, sdScanning, sdBusyDev)
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
				return v.layoutProgress(gtx, th, progressDone, progressMax)
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

func (v *ImportView) layoutProgress(gtx layout.Context, th *Theme, done, max int64) layout.Dimensions {
	w := gtx.Constraints.Max.X
	barH := gtx.Dp(unit.Dp(6))
	bg := image.Rect(0, 0, w, barH)
	clipBg := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.CellBG}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	clipBg.Pop()
	if max > 0 {
		frac := float32(done) / float32(max)
		if frac > 1 {
			frac = 1
		}
		fg := image.Rect(0, 0, int(float32(w)*frac), barH)
		clipFg := clip.Rect(fg).Push(gtx.Ops)
		paint.ColorOp{Color: th.Accent}.Add(gtx.Ops)
		paint.PaintOp{}.Add(gtx.Ops)
		clipFg.Pop()
	}
	return layout.Dimensions{Size: image.Pt(w, barH)}
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
		exists := false
		for _, p := range v.importDirs {
			if p == path {
				exists = true
				break
			}
		}
		if !exists {
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
		exists := false
		for _, p := range v.zipFiles {
			if p == path {
				exists = true
				break
			}
		}
		if !exists {
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
		for _, p := range v.importDirs {
			if p == text {
				return
			}
		}
		v.importDirs = append(v.importDirs, text)
	} else {
		for _, p := range v.zipFiles {
			if p == text {
				return
			}
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
func (v *ImportView) explainCantStart() {
	cfg := GetConfig()
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
	if !hasDirs && !hasZips && inboxFileCount(cfg.InboxDir) == 0 {
		msg := "Nothing to import — add a folder, ZIP, or SD card and try again."
		v.setStatus(msg)
		v.appendLog("[ERROR] " + msg)
	}
}

func (v *ImportView) haveSomethingToImport() bool {
	cfg := GetConfig()
	if cfg.InboxDir == "" || cfg.OutboxDir == "" {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.importDirs) > 0 || len(v.zipFiles) > 0 || inboxFileCount(cfg.InboxDir) > 0
}

func inboxFileCount(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		n++
		return nil
	})
	return n
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
	v.mu.Unlock()
	atomic.StoreInt64(&v.statMoved, 0)
	atomic.StoreInt64(&v.statSkipped, 0)
	atomic.StoreInt64(&v.statErrors, 0)
	atomic.StoreInt64(&v.progressDone, 0)
	atomic.StoreInt64(&v.progressMax, 0)

	// Register with the process bar so the user can pause / cancel
	// the import even when the modal is closed.
	if v.processes != nil {
		v.proc = v.processes.Begin(ProcImport, "Import", func() {
			v.mu.Lock()
			c := v.cancelImport
			v.mu.Unlock()
			if c != nil {
				c()
			}
		}, true)
	}

	go func() {
		defer func() {
			v.mu.Lock()
			v.running = false
			v.finished = true
			v.cancelImport = nil
			// Unmount any devices we mounted ourselves now that we're done
			// reading from them. The user can then safely pull the card.
			mounts := v.sdMounts
			v.sdMounts = nil
			proc := v.proc
			v.proc = nil
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
		extracted := v.extractZipToInbox(z, cfg.InboxDir)
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
			extracted := v.extractZipToInbox(z, cfg.InboxDir)
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
				v.setProgress(0, int64(len(files)))
				v.processBatch(ctx, cfg.OutboxDir, files)
				continue
			}
			v.setStatus(fmt.Sprintf("Copying %d files from %s…", len(files), sd))
			v.appendLog(fmt.Sprintf("Copying %d files from %s to Inbox...", len(files), sd))
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
					continue
				}
				batch = append(batch, dest)
				if deleteSrc {
					if err := os.Remove(srcFile); err != nil {
						atomic.AddInt64(&v.statErrors, 1)
						v.appendLog("[ERROR] Failed to delete source " + srcFile + ": " + err.Error())
					}
				}
			}
			if len(batch) == 0 {
				continue
			}
			v.setStatus(fmt.Sprintf("Filing %d files into %s…", len(batch), filepath.Base(cfg.OutboxDir)))
			v.setProgress(0, int64(len(batch)))
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
	baseDates := make(map[string]time.Time)
	// Files whose mtime predates their declared EXIF/metadata date — rewrite
	// the metadata to mtime after we've decided the destination so subsequent
	// scans see consistent dates.
	type mtimeFix struct {
		path string
		when time.Time
	}
	var mtimeFixes []mtimeFix
	for _, src := range entries {
		v.waitIfPaused()
		if ctx.Err() != nil {
			return
		}
		oldest, _, mtimeOlder := scan.GetOldestMediaDate(src)
		if mtimeOlder {
			mtimeFixes = append(mtimeFixes, mtimeFix{path: src, when: oldest})
		}
		baseName := filepath.Base(src)
		base := strings.TrimSuffix(baseName, filepath.Ext(baseName))
		if existing, ok := baseDates[base]; !ok || oldest.Before(existing) {
			baseDates[base] = oldest
		}
	}
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
		if err := os.Rename(src, dest); err != nil {
			atomic.AddInt64(&v.statErrors, 1)
			v.appendLog("[ERROR] Could not move " + baseName + ": " + err.Error())
		} else {
			atomic.AddInt64(&v.statMoved, 1)
			v.appendLog(fmt.Sprintf("[OK] Moved %s -> %s", baseName, filepath.Join(dateFolder, filepath.Base(dest))))
		}
		v.bumpProgress()
	}
}

func (v *ImportView) extractZipToInbox(zipPath, inboxDir string) []string {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		v.appendLog("[ERROR] Failed to open ZIP: " + err.Error())
		return nil
	}
	defer r.Close()
	var extracted []string
	for _, f := range r.File {
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
		out, err := os.Create(dest)
		if err != nil {
			v.appendLog("[ERROR] Failed to create file: " + err.Error())
			rc.Close()
			continue
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			v.appendLog("[ERROR] Failed to extract file: " + err.Error())
			os.Remove(dest)
			continue
		}
		extracted = append(extracted, dest)
	}
	return extracted
}

func (v *ImportView) setProgress(done, max int64) {
	atomic.StoreInt64(&v.progressDone, done)
	atomic.StoreInt64(&v.progressMax, max)
	v.mu.Lock()
	proc := v.proc
	v.mu.Unlock()
	if proc != nil {
		proc.SetDone(done)
		proc.SetTotal(max)
	}
	if v.invalidate != nil {
		v.invalidate()
	}
}

func (v *ImportView) bumpProgress() {
	atomic.AddInt64(&v.progressDone, 1)
	v.mu.Lock()
	proc := v.proc
	v.mu.Unlock()
	if proc != nil {
		proc.AddDone(1)
	}
	if v.invalidate != nil {
		v.invalidate()
	}
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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

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
	ha, err := sha256File(a)
	if err != nil {
		return false, err
	}
	hb, err := sha256File(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
