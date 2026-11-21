package ui

import (
	"archive/zip"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/layout"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/dns/photo-viewer/internal/fsutil"
	"github.com/dns/photo-viewer/internal/scan"
)

// Date probe and rewrite used by processBatch, indirected so tests can stub exiftool.
var (
	importDateProber   = scan.GetOldestMediaDate
	importSetMediaDate = scan.SetMediaDate
	// syncDir fsyncs a directory so its entries are durable; indirected so
	// tests can count Inbox fsyncs.
	syncDir = fsutil.SyncDir
)

// inboxProbe reports whether the Inbox holds at least one importable file,
// indirected so tests can stall the probe.
var inboxProbe = inboxHasFiles

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

	// Shared task plumbing (taskui.go): mu, statusMsg, log, progress, scope, invalidate, logList.
	taskView

	importDirs   []string
	zipFiles     []string
	statMoved    int64
	statSkipped  int64
	statErrors   int64
	running      bool
	starting     bool // a Start click's background inbox probe is in flight.
	finished     bool // at least one import has completed since the overlay opened
	deleteSource bool
	cancelImport context.CancelFunc

	// showGen invalidates a background importDirsProblem check started by
	// Show: a newer Show (or one with dirs unset) must not be overwritten by
	// an older check landing late (U5-08). Guarded by mu.
	showGen int

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
}

// sdMount tracks a removable device that the import view mounted itself.
// After the import completes (or the user closes the overlay) the mount is
// released via udisksctl so the user can safely pull the card.
type sdMount struct {
	DevicePath string
}

// NewImportView wires the import overlay.
func NewImportView(invalidate func()) *ImportView {
	v := &ImportView{}
	v.initTaskView(invalidate)
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
	// Invalidate any check a previous Show started, even when this one will
	// not launch a new one (dirs unset), so a late EvalSymlinks can't replace
	// the synchronous "Set InboxDir…" message below.
	v.showGen++
	gen := v.showGen
	if cfg.InboxDir == "" || cfg.OutboxDir == "" {
		v.statusMsg = "Set InboxDir and OutboxDir in " + configPath() + " before importing."
		v.mu.Unlock()
		return
	}
	// importDirsProblem resolves symlinks (EvalSymlinks) and can block on an
	// autofs mount or a spun-down disk, so it must not run on the frame
	// goroutine (U5-08). Show the checking line now and let the result land
	// asynchronously if this show is still the current one.
	inbox, outbox := cfg.InboxDir, cfg.OutboxDir
	v.statusMsg = "Checking import folders…"
	v.mu.Unlock()
	go func() {
		p := importDirsProblem(inbox, outbox)
		v.mu.Lock()
		if v.Open && v.showGen == gen && !v.running {
			if p != "" {
				v.statusMsg = p + " Fix it in Settings before importing."
			} else {
				v.statusMsg = fmt.Sprintf("Inbox: %s   Outbox: %s", inbox, outbox)
			}
		}
		v.mu.Unlock()
		if v.invalidate != nil {
			v.invalidate()
		}
	}()
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
	if GetConfig().ImportDeleteSource != deleteSource {
		UpdateConfig(func(c *Config) { c.ImportDeleteSource = deleteSource })
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
	// Written under mu so Show's background dir check (which reads it under
	// mu) can't see a torn/stale value.
	v.Open = false
	v.mu.Unlock()
	if len(mounts) > 0 {
		go v.unmountAll(mounts)
	}
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
		v.sdMounts = append(v.sdMounts, sdMount{DevicePath: dev.Path})
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

// Layout draws the overlay.
func (v *ImportView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	v.log.drain()

	if v.closeBtn.Clicked(gtx) {
		v.Close()
	}
	if v.startBtn.Clicked(gtx) {
		v.onStartClicked()
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
			go v.pickSources(true)
		}
	}
	if v.addZipBtn.Clicked(gtx) {
		if strings.TrimSpace(v.addPathEditor.Text()) != "" {
			v.commitAddPath(false)
		} else {
			go v.pickSources(false)
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
		val := v.deleteCheck.Value
		UpdateConfig(func(c *Config) { c.ImportDeleteSource = val })
		v.mu.Lock()
		v.deleteSource = v.deleteCheck.Value
		v.mu.Unlock()
	}

	// Background.
	drawBackground(gtx, th.Background)

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
	starting := v.starting
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
				return v.layoutStatus(gtx, th, statusMsg, running, finished, errs)
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
				return v.layoutButtons(gtx, th, running, starting)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(10)}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return v.layoutLog(gtx, th, logVisible, unit.Sp(14))
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

func (v *ImportView) layoutButtons(gtx layout.Context, th *Theme, running, starting bool) layout.Dimensions {
	startLabel := "Start Import"
	if running {
		startLabel = "Running…"
	} else if starting {
		startLabel = "Checking…"
	}
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Rigid(material.Button(th.Theme, &v.closeBtn, "Close (esc)").Layout),
		layout.Rigid(layout.Spacer{Width: unit.Dp(6)}.Layout),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			b := material.Button(th.Theme, &v.startBtn, startLabel)
			if running || starting {
				// Visually subdue the button while a run or a Start click's
				// probe is in flight; onStartClicked gates the click.
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
func (v *ImportView) layoutStatus(gtx layout.Context, th *Theme, msg string, running, finished bool, errs int64) layout.Dimensions {
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

// addSources appends each trimmed, non-empty path in paths that dst doesn't
// already hold, reporting whether anything was added. The caller holds v.mu.
func addSources(dst *[]string, paths ...string) bool {
	added := false
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if !slices.Contains(*dst, path) {
			*dst = append(*dst, path)
			added = true
		}
	}
	return added
}

// pickSources shells out to zenity for a folder picker (asDir) or a ZIP file
// picker, then adds the result to importDirs or zipFiles respectively. Runs on
// a goroutine since zenity blocks until the user dismisses the dialog. Falls
// back silently if zenity is missing — the user can still type a path into the
// editor.
func (v *ImportView) pickSources(asDir bool) {
	args := []string{"--file-selection", "--multiple", "--directory", "--title=Select folder(s) to import"}
	prefix := "[ERROR] Folder picker failed: "
	if !asDir {
		args = []string{"--file-selection", "--multiple", "--title=Select ZIP(s) to import", "--file-filter=ZIP archives | *.zip *.ZIP"}
		prefix = "[ERROR] File picker failed: "
	}
	out, err := runZenity(args...)
	if err != nil || out == "" {
		if err != nil {
			v.appendLog(prefix + err.Error() + " (type a path into the field instead)")
		}
		return
	}
	v.mu.Lock()
	dst := &v.importDirs
	if !asDir {
		dst = &v.zipFiles
	}
	added := addSources(dst, strings.Split(out, "|")...)
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
	dst := &v.importDirs
	if !asDir {
		dst = &v.zipFiles
	}
	addSources(dst, text)
}

// onStartClicked handles a Start Import click without blocking the frame
// goroutine: the inbox probe (a WalkDir that can cover a whole media-free tree
// or a slow mount) and importDirsProblem's EvalSymlinks run on a goroutine, and
// the button shows "Checking…" meanwhile (U3-06). A click while a probe or an
// import is in flight is ignored.
func (v *ImportView) onStartClicked() {
	v.mu.Lock()
	if v.running || v.starting {
		v.mu.Unlock()
		return
	}
	v.starting = true
	v.mu.Unlock()
	if v.invalidate != nil {
		v.invalidate()
	}
	go func() {
		v.requestStart()
		v.mu.Lock()
		v.starting = false
		v.mu.Unlock()
		if v.invalidate != nil {
			v.invalidate()
		}
	}()
}

// requestStart decides a Start click: resolve importDirsProblem once, probe the
// inbox only when it is configured and valid, then start or explain why not.
func (v *ImportView) requestStart() {
	cfg := GetConfig()
	problem := importDirsProblem(cfg.InboxDir, cfg.OutboxDir)
	inboxHas := cfg.InboxDir != "" && cfg.OutboxDir != "" && problem == "" && inboxProbe(cfg.InboxDir)
	if v.haveSomethingToImport(cfg, problem, inboxHas) {
		v.startImport(cfg)
	} else {
		v.explainCantStart(cfg, problem, inboxHas)
	}
}

// explainCantStart logs (and posts to the status line) why a Start Import
// click had no effect — usually because inbox/outbox are not configured, or
// no sources have been added. Without this feedback the button looks broken.
// cfg, problem and inboxHas are computed once by the Start-click handler and
// threaded in so this never re-reads config, re-runs importDirsProblem or
// re-walks the inbox (U3-06).
func (v *ImportView) explainCantStart(cfg Config, problem string, inboxHas bool) {
	if cfg.InboxDir == "" || cfg.OutboxDir == "" {
		msg := "Set Inbox and Outbox directories in Settings first (config: " + configPath() + ")."
		v.setStatus(msg)
		v.appendLog("[ERROR] " + msg)
		return
	}
	if problem != "" {
		msg := problem + " Fix it in Settings before importing."
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
// act on. problem and inboxHas are supplied by the caller (which already ran
// importDirsProblem and probed the inbox once) so the decision costs no extra
// walk or symlink resolution.
func (v *ImportView) haveSomethingToImport(cfg Config, problem string, inboxHas bool) bool {
	if cfg.InboxDir == "" || cfg.OutboxDir == "" {
		return false
	}
	if problem != "" {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.importDirs) > 0 || len(v.zipFiles) > 0 || inboxHas
}

// inboxHasFiles reports whether dir holds at least one importable media file.
// It walks with scan.WalkMediaPaths — the same rules as the indexer's walk —
// so hidden files and hidden directories (._* AppleDouble junk, .Trashes) don't
// count (U4-04). It short-circuits: the callback returns fs.SkipAll on the
// first hit, so a huge inbox costs O(1) in the common "has files" case instead
// of a full recursive count — this runs on the frame goroutine, so a stall here
// shows up as a dropped frame per Start click (U-10). Only DetectType !=
// TypeUnknown files count, matching what phase-2 processing actually files, so
// a stray .DS_Store in an otherwise-empty inbox doesn't claim "something to
// import".
func inboxHasFiles(dir string) bool {
	found := false
	_ = scan.WalkMediaPaths(context.Background(), dir, func(string, scan.MediaType) error {
		found = true
		return fs.SkipAll
	})
	return found
}

// startImport kicks off a background goroutine that runs the same three-phase
// flow as the Fyne import. Updates progressDone/progressMax/log atomically;
// the UI polls them at frame time. cfg is the already-validated config from the
// Start-click handler (requestStart), so no inbox/outbox checks run here.
func (v *ImportView) startImport(cfg Config) {
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
	if proc := v.scope.begin(ctx, ProcImport, "Import", func() {
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
		_ = scan.WalkMediaPaths(ctx, cfg.InboxDir, func(p string, _ scan.MediaType) error {
			inbox = append(inbox, p)
			return nil
		})
		if len(inbox) > 0 {
			v.setStatus(fmt.Sprintf("Processing %d files from Inbox…", len(inbox)))
			v.appendLog(fmt.Sprintf("Processing %d files already in Inbox...", len(inbox)))
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
		// Hidden files and hidden directories are skipped, like the indexer
		// (U4-04).
		_ = scan.WalkVisibleFiles(ctx, src, func(p string) error {
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
			if deleteSrc && fsutil.SameDevice(sd, cfg.OutboxDir) {
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
			batch := v.copyToInbox(ctx, files, cfg.InboxDir, deleteSrc)
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

// copyToInbox copies files into inboxDir and returns the Inbox copies. With
// deleteSrc the sources are removed only after ONE fsync of the Inbox directory
// makes every copy's entry durable (U4-05: it used to fsync the same directory
// once per file). The tail also runs after a cancel, for the files already
// copied; if the fsync fails every source is kept — the copies are already in
// the batch, so nothing is lost either way.
func (v *ImportView) copyToInbox(ctx context.Context, files []string, inboxDir string, deleteSrc bool) []string {
	var batch []string
	var copied []string
	for _, srcFile := range files {
		v.waitIfPaused()
		if ctx.Err() != nil {
			break
		}
		dest, err := fsutil.CopyUnique(ctx, srcFile, inboxDir, filepath.Base(srcFile))
		if err != nil {
			atomic.AddInt64(&v.statErrors, 1)
			v.appendLog("[ERROR] Copy failed for " + srcFile + ": " + err.Error())
			v.bumpProgress()
			continue
		}
		batch = append(batch, dest)
		copied = append(copied, srcFile)
		v.bumpProgress()
	}
	if deleteSrc && len(copied) > 0 {
		// CopyUnique fsynced each Inbox copy's data; one fsync of the Inbox
		// directory here makes every copy's entry durable before any source
		// (the only other copy) is removed.
		if err := syncDir(inboxDir); err != nil {
			atomic.AddInt64(&v.statErrors, 1)
			v.appendLog("[ERROR] Failed to fsync Inbox before deleting sources: " + err.Error() + " (sources kept)")
		} else {
			for _, srcFile := range copied {
				if err := os.Remove(srcFile); err != nil {
					atomic.AddInt64(&v.statErrors, 1)
					v.appendLog("[ERROR] Failed to delete source " + srcFile + ": " + err.Error())
				}
			}
		}
	}
	return batch
}

// processBatch is the Fyne flow: read EXIF, group by base name, move into the
// outbox under YYYY-MM-DD folders.
func (v *ImportView) processBatch(ctx context.Context, outboxDir string, entries []string) {
	if len(entries) == 0 {
		return
	}
	// The batch is visited twice — once to read every file's date, once to move
	// it — so the progress bar spans 2*len(entries), plus one extra tick per
	// file that needs its mtime fixed (U3-07). The date probes fork exiftool per
	// file (~50–200 ms each); running them strictly sequentially stalled a
	// multi-thousand-file card at 0% for minutes, so they go through a small
	// worker pool (mirroring internal/ui/organize.go's scan pass) that bumps the
	// bar as it reads.
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
	}
	results := make([]dateResult, len(entries))
	numWorkers := runtime.NumCPU()
	numWorkers = min(numWorkers, 4) // cap the concurrent exiftool forks
	// Bounded queue with a cancel-aware producer, the same shape as organize's
	// scan pass, so a cancel stops queuing at once (U3-04).
	jobs := make(chan int, numWorkers*2)
	var wg sync.WaitGroup
	for range numWorkers {
		wg.Go(func() {
			for idx := range jobs {
				v.waitIfPaused()
				if ctx.Err() != nil {
					return
				}
				oldest, _, mtimeOlder := importDateProber(entries[idx])
				results[idx] = dateResult{oldest: oldest, mtimeOlder: mtimeOlder}
				v.bumpProgress()
			}
		})
	}
	go func() {
		defer close(jobs)
		for i := range entries {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	// Fold per-file results in input order so the "oldest date wins per base
	// name" tie-break is deterministic.
	for i, src := range entries {
		r := results[i]
		if r.mtimeOlder {
			mtimeFixes = append(mtimeFixes, mtimeFix{path: src, when: r.oldest})
		}
		baseName := filepath.Base(src)
		base := strings.TrimSuffix(baseName, filepath.Ext(baseName))
		if existing, ok := baseDates[base]; !ok || r.oldest.Before(existing) {
			baseDates[base] = r.oldest
		}
	}
	if len(mtimeFixes) > 0 {
		// The mtime-fix pass gets its own slice of the bar so it doesn't freeze
		// mid-import while it shells out per file.
		v.setProgress(int64(len(entries)), int64(2*len(entries)+len(mtimeFixes)))
	}
	v.setStatus(fmt.Sprintf("Filing %d files into %s…", len(entries), filepath.Base(outboxDir)))
	if len(mtimeFixes) > 0 {
		// U5-06: each fix shells out to exiftool (~9 ms), so run them through
		// the same bounded, cancel-aware pool as the date-read pass above —
		// scan's exiftool pool has 4 daemons, so cap there. Errors and "ran"
		// flags are indexed by input position; the log lines are emitted in
		// input order after the pool drains so the log stays deterministic.
		fixErrs := make([]error, len(mtimeFixes))
		fixRan := make([]bool, len(mtimeFixes))
		fixWorkers := min(runtime.NumCPU(), 4)
		if len(mtimeFixes) < 4 {
			// A handful of fixes is cheaper serially than through the pool
			// (and keeps their progress ticks strictly ordered).
			fixWorkers = 1
		}
		fixJobs := make(chan int, fixWorkers*2)
		var fixWG sync.WaitGroup
		for range fixWorkers {
			fixWG.Go(func() {
				for idx := range fixJobs {
					v.waitIfPaused()
					if ctx.Err() != nil {
						return
					}
					fixRan[idx] = true
					fixErrs[idx] = importSetMediaDate(mtimeFixes[idx].path, mtimeFixes[idx].when)
					v.bumpProgress()
				}
			})
		}
		go func() {
			defer close(fixJobs)
			for i := range mtimeFixes {
				select {
				case fixJobs <- i:
				case <-ctx.Done():
					return
				}
			}
		}()
		fixWG.Wait()
		// A cancelled batch logs only the fixes that actually ran; the move
		// pass below still returns at its own ctx check.
		for i, fix := range mtimeFixes {
			if !fixRan[i] {
				continue
			}
			if err := fixErrs[i]; err != nil {
				v.appendLog("[WARN] Could not rewrite creation date on " + filepath.Base(fix.path) + ": " + err.Error())
			} else {
				v.appendLog("Rewrote creation date on " + filepath.Base(fix.path) + " to " + fix.when.Format("2006-01-02"))
			}
		}
		if ctx.Err() != nil {
			return
		}
	}

	// Cross-device moves are copies first: MoveUniqueDeferred fsyncs nothing
	// and removes no source, so the copied entries are batched here and their
	// destination directories fsynced once each before any source is unlinked
	// (U5-09). pending is flushed every 64 files, after the loop, and by the
	// deferred call on an early return, so a cancelled run still makes what it
	// copied durable and removes the sources.
	type pendingMove struct {
		src        string
		dest       string
		destDir    string
		baseName   string
		dateFolder string
	}
	var pending []pendingMove
	flush := func() {
		if len(pending) == 0 {
			return
		}
		byDir := make(map[string][]pendingMove)
		var dirOrder []string
		for _, pm := range pending {
			if _, ok := byDir[pm.destDir]; !ok {
				dirOrder = append(dirOrder, pm.destDir)
			}
			byDir[pm.destDir] = append(byDir[pm.destDir], pm)
		}
		for _, dir := range dirOrder {
			group := byDir[dir]
			if err := syncDir(dir); err != nil {
				// Without the directory fsync the new entries may not survive
				// a power loss, so keep every source of this directory.
				for _, pm := range group {
					atomic.AddInt64(&v.statErrors, 1)
					v.appendLog("[ERROR] Could not move " + pm.baseName + ": " + err.Error())
				}
				continue
			}
			for _, pm := range group {
				if err := os.Remove(pm.src); err != nil {
					atomic.AddInt64(&v.statErrors, 1)
					v.appendLog("[ERROR] Could not remove source " + pm.src + " after copying to " + pm.dest + ": " + err.Error())
					continue
				}
				atomic.AddInt64(&v.statMoved, 1)
				v.appendLog(fmt.Sprintf("[OK] Moved %s -> %s", pm.baseName, filepath.Join(pm.dateFolder, filepath.Base(pm.dest))))
			}
		}
		pending = pending[:0]
	}
	defer flush()

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
			v.bumpProgress()
			continue
		}
		dest := filepath.Join(destDir, baseName)
		if destInfo, err := os.Stat(dest); err == nil {
			// The source already IS the destination — an Inbox that is (or
			// contains) the Outbox, or an import folder inside the library.
			// SameContent would compare the file with itself and the duplicate
			// removal below would delete the only copy (U-17). SameFile also
			// matches hardlinks and symlinked dirs; skipping those just leaves
			// the source in place, which is always safe.
			if srcInfo, err := os.Stat(src); err == nil && os.SameFile(srcInfo, destInfo) {
				atomic.AddInt64(&v.statSkipped, 1)
				v.appendLog(fmt.Sprintf("[SKIP] %s is already filed in %s", baseName, dateFolder))
				v.bumpProgress()
				continue
			}
			same, _ := fsutil.SameContent(src, dest)
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
		}
		dest, copied, moveErr := fsutil.MoveUniqueDeferred(ctx, src, destDir, baseName)
		if moveErr != nil {
			atomic.AddInt64(&v.statErrors, 1)
			v.appendLog("[ERROR] Could not move " + baseName + ": " + moveErr.Error())
		} else if copied {
			// Cross-device: the bytes are in destDir but its entry isn't
			// durable yet and src is still the only other copy, so defer the
			// fsync + remove to flush.
			pending = append(pending, pendingMove{
				src:        src,
				dest:       dest,
				destDir:    destDir,
				baseName:   baseName,
				dateFolder: dateFolder,
			})
			if len(pending) >= 64 {
				flush()
			}
		} else {
			atomic.AddInt64(&v.statMoved, 1)
			v.appendLog(fmt.Sprintf("[OK] Moved %s -> %s", baseName, filepath.Join(dateFolder, filepath.Base(dest))))
		}
		v.bumpProgress()
	}
	flush()
}

func (v *ImportView) extractZipToInbox(ctx context.Context, zipPath, inboxDir string) []string {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		v.appendLog("[ERROR] Failed to open ZIP: " + err.Error())
		return nil
	}
	defer r.Close() //nolint:errcheck // read-only archive
	var extracted []string
	for _, f := range r.File {
		// Honour Pause / Cancel between archive entries so a multi-GB ZIP
		// stops promptly instead of extracting to completion after the user
		// hit Cancel. (WriteUnique below only publishes an entry via an
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
		if zipJunkName(f.Name) {
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
		// Crash-safe extract: WriteUnique streams into a temp, fsyncs, and
		// renames it onto a free name — so an interrupted extraction (cancel,
		// full disk, power loss) never leaves a truncated file that the next
		// import's inbox walk would file into the library as valid media.
		dest, err := fsutil.WriteUnique(ctx, inboxDir, filepath.Base(f.Name), rc)
		rc.Close()
		if err != nil {
			v.appendLog("[ERROR] Failed to extract file: " + err.Error())
			continue
		}
		extracted = append(extracted, dest)
	}
	return extracted
}

// zipJunkName reports archive entries that are hidden or macOS metadata: any
// path component that starts with "." (._IMG.jpg, .Trashes/…) or is __MACOSX.
func zipJunkName(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." {
			continue
		}
		if strings.HasPrefix(part, ".") || part == "__MACOSX" {
			return true
		}
	}
	return false
}
