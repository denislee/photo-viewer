package ui

import (
	"image"
	"sync"
	"time"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// SDCardWatcher polls lsblk on a fixed interval and reports newly-attached
// removable devices via a queue of pending prompts. Devices present at the
// time the watcher starts are seeded into `known` so the user isn't pestered
// about drives that were already plugged in. A device the user dismisses is
// suppressed until it's physically unplugged.
//
// The watcher always polls; auto-detect just gates whether new attachments
// surface as prompts. Polling regardless lets the watcher track plug/unplug
// state continuously so toggling the setting on doesn't immediately enqueue
// every currently-present device.
type SDCardWatcher struct {
	invalidate func()
	interval   time.Duration

	mu        sync.Mutex
	running   bool
	stopCh    chan struct{}
	known     map[string]bool
	dismissed map[string]bool
	pending   []removableDevice
}

// NewSDCardWatcher constructs a watcher. invalidate is called whenever a new
// device shows up so the UI can repaint the prompt.
func NewSDCardWatcher(invalidate func()) *SDCardWatcher {
	return &SDCardWatcher{
		invalidate: invalidate,
		interval:   3 * time.Second,
		known:      make(map[string]bool),
		dismissed:  make(map[string]bool),
	}
}

// Start kicks off the polling goroutine. Seeds `known` with devices currently
// attached so the watcher only fires on subsequent insertions. Safe to call
// more than once — extra calls are no-ops.
func (w *SDCardWatcher) Start() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.stopCh = make(chan struct{})
	stop := w.stopCh
	w.mu.Unlock()

	if devs, err := listRemovableDevices(); err == nil {
		w.mu.Lock()
		for _, d := range devs {
			w.known[d.Path] = true
		}
		w.mu.Unlock()
	}

	go func() {
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				w.tick()
			}
		}
	}()
}

// Stop terminates the polling goroutine.
func (w *SDCardWatcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.running {
		return
	}
	close(w.stopCh)
	w.stopCh = nil
	w.running = false
}

func (w *SDCardWatcher) tick() {
	devs, err := listRemovableDevices()
	if err != nil {
		return
	}
	enabled := GetConfig().SDCardAutoDetect
	w.mu.Lock()
	seen := make(map[string]bool, len(devs))
	var newDevs []removableDevice
	for _, d := range devs {
		seen[d.Path] = true
		if !w.known[d.Path] && !w.dismissed[d.Path] {
			newDevs = append(newDevs, d)
		}
	}
	for p := range w.known {
		if !seen[p] {
			delete(w.known, p)
		}
	}
	for p := range w.dismissed {
		if !seen[p] {
			delete(w.dismissed, p)
		}
	}
	for _, d := range newDevs {
		w.known[d.Path] = true
		if enabled {
			w.pending = append(w.pending, d)
		}
	}
	hasNew := enabled && len(newDevs) > 0
	w.mu.Unlock()
	if hasNew && w.invalidate != nil {
		w.invalidate()
	}
}

// Pending returns the next pending device, or false if none.
func (w *SDCardWatcher) Pending() (removableDevice, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return removableDevice{}, false
	}
	return w.pending[0], true
}

// Consume pops and returns the front pending device. The device path is
// suppressed until unplugged so dismissing or acting on a prompt doesn't
// immediately re-trigger it on the next tick.
func (w *SDCardWatcher) Consume() (removableDevice, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return removableDevice{}, false
	}
	d := w.pending[0]
	w.pending = w.pending[1:]
	w.dismissed[d.Path] = true
	return d, true
}

// SDPromptView is the small modal that appears when the watcher detects a
// newly-attached mass-storage device. It offers three actions: Import, Import
// + delete source after copy, and Ignore.
type SDPromptView struct {
	Open   bool
	Device removableDevice

	OnImport  func(dev removableDevice, deleteSource bool)
	OnDismiss func()

	importBtn       widget.Clickable
	importDeleteBtn widget.Clickable
	ignoreBtn       widget.Clickable
}

// Show populates the prompt with the given device and reveals the modal.
func (v *SDPromptView) Show(dev removableDevice) {
	v.Device = dev
	v.Open = true
}

// Close hides the prompt.
func (v *SDPromptView) Close() {
	v.Open = false
	if v.OnDismiss != nil {
		v.OnDismiss()
	}
}

// Layout draws the prompt as a centered card-style modal.
func (v *SDPromptView) Layout(gtx layout.Context, th *Theme) layout.Dimensions {
	if v.importBtn.Clicked(gtx) {
		dev := v.Device
		v.Open = false
		if v.OnImport != nil {
			v.OnImport(dev, false)
		}
	}
	if v.importDeleteBtn.Clicked(gtx) {
		dev := v.Device
		v.Open = false
		if v.OnImport != nil {
			v.OnImport(dev, true)
		}
	}
	if v.ignoreBtn.Clicked(gtx) {
		v.Close()
	}

	// Dim the background so it reads as a modal.
	totalW := gtx.Constraints.Max.X
	totalH := gtx.Constraints.Max.Y
	bg := image.Rectangle{Max: image.Pt(totalW, totalH)}
	clipArea := clip.Rect(bg).Push(gtx.Ops)
	paint.ColorOp{Color: th.Background}.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
	clipArea.Pop()

	pad := layout.UniformInset(unit.Dp(24))
	return pad.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.H6(th.Theme, "Removable device detected")
				lbl.Color = th.Foreground
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(8)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(13), "💾  "+v.Device.Display())
				lbl.Color = th.Foreground
				lbl.MaxLines = 3
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(6)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				lbl := material.Label(th.Theme, unit.Sp(12),
					"Would you like to import media from this device? "+
						"Choosing \"Import & delete\" will remove each source file from the card "+
						"only after it has been successfully copied into your library.")
				lbl.Color = th.Muted
				lbl.MaxLines = 6
				return lbl.Layout(gtx)
			}),
			layout.Rigid(layout.Spacer{Height: unit.Dp(16)}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(th.Theme, &v.importBtn, "Import")
						b.Background = th.Accent
						return b.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(th.Theme, &v.importDeleteBtn, "Import && delete source")
						b.Background = th.Destructive
						return b.Layout(gtx)
					}),
					layout.Rigid(layout.Spacer{Width: unit.Dp(8)}.Layout),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						b := material.Button(th.Theme, &v.ignoreBtn, "Ignore")
						b.Background = th.CellBG
						b.Color = th.Foreground
						return b.Layout(gtx)
					}),
				)
			}),
		)
	})
}
