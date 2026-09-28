package ui

import (
	"image"
	"log"
	"sync"
	"time"

	"gioui.org/layout"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

// SDCardWatcher reports newly-attached removable devices via a queue of
// pending prompts while auto-detect is enabled. Devices present at the time the
// watcher starts (or the toggle is first enabled) are seeded into `known` so
// the user isn't pestered about drives already plugged in. A device the user
// dismisses is suppressed until it is physically unplugged.
//
// On Linux it listens for block-device uevents (see blockEvents) and runs
// lsblk only when one arrives, so an idle session forks nothing. The ticker
// then just watches the auto-detect toggle: the first tick after it is enabled
// re-seeds `known` from the current device list (no prompts), and insertions
// after that surface normally. Where uevents are unavailable (other platforms,
// a sandbox without netlink) the ticker falls back to running lsblk itself,
// as the watcher always did before. While auto-detect is off, no lsblk forks
// are issued either way.
type SDCardWatcher struct {
	invalidate func()
	interval   time.Duration
	// settle delays the rescan after a uevent so a burst (disk, then each
	// partition, then udev's copies of each) costs one lsblk, run after udev
	// has recorded the filesystem type.
	settle time.Duration

	// Seams for tests; NewSDCardWatcher wires the real implementations.
	list    func() ([]removableDevice, error)
	events  func(stop <-chan struct{}) (<-chan struct{}, error)
	enabled func() bool

	mu          sync.Mutex
	running     bool
	stopCh      chan struct{}
	known       map[string]bool
	dismissed   map[string]bool
	pending     []removableDevice
	prevEnabled bool // whether auto-detect was on during the previous check
}

// NewSDCardWatcher constructs a watcher. invalidate is called whenever a new
// device shows up so the UI can repaint the prompt.
func NewSDCardWatcher(invalidate func()) *SDCardWatcher {
	return &SDCardWatcher{
		invalidate: invalidate,
		interval:   3 * time.Second,
		settle:     500 * time.Millisecond,
		list:       listRemovableDevices,
		events:     blockEvents,
		enabled:    func() bool { return GetConfig().SDCardAutoDetect },
		known:      make(map[string]bool),
		dismissed:  make(map[string]bool),
	}
}

// Start kicks off the watcher goroutine. Seeds `known` with devices currently
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

	if devs, err := w.list(); err == nil {
		w.mu.Lock()
		for _, d := range devs {
			w.known[d.Path] = true
		}
		w.prevEnabled = true // seeding done; the next scan may enqueue
		w.mu.Unlock()
	}

	events, err := w.events(stop)
	poll := err != nil
	if poll {
		log.Printf("sd-card watcher: no block-device events (%v); polling lsblk every %s", err, w.interval)
	}

	go func() {
		t := time.NewTicker(w.interval)
		defer t.Stop()
		var settled <-chan time.Time
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				w.tick(poll)
			case _, ok := <-events:
				if !ok {
					log.Printf("sd-card watcher: block-device events stopped; polling lsblk every %s", w.interval)
					events, poll = nil, true
					continue
				}
				if settled == nil {
					settled = time.After(w.settle)
				}
			case <-settled:
				settled = nil
				w.onEvent()
			}
		}
	}()
}

// Stop terminates the watcher goroutine and its event source.
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

// tick runs every interval. It tracks the auto-detect toggle, re-seeding when
// it has just been turned on, and in polling mode also rescans.
func (w *SDCardWatcher) tick(poll bool) {
	enabled := w.enabled()

	w.mu.Lock()
	prev := w.prevEnabled
	w.prevEnabled = enabled
	w.mu.Unlock()

	if enabled && (poll || !prev) {
		w.scan(prev)
	}
}

// onEvent rescans after a burst of block-device uevents has settled.
func (w *SDCardWatcher) onEvent() {
	if !w.enabled() {
		return
	}
	w.mu.Lock()
	prev := w.prevEnabled
	w.prevEnabled = true
	w.mu.Unlock()
	w.scan(prev)
}

// scan lists devices and diffs them against `known`. With prev false (the
// first scan after auto-detect was enabled) it only re-seeds; otherwise every
// new, non-dismissed device is queued as a prompt. Unplugged devices are
// forgotten, which also lifts their dismissal.
func (w *SDCardWatcher) scan(prev bool) {
	devs, err := w.list()
	if err != nil {
		return
	}
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
		if prev {
			w.pending = append(w.pending, d)
		}
	}
	hasNew := prev && len(newDevs) > 0
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
