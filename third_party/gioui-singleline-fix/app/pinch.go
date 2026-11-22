// SPDX-License-Identifier: Unlicense OR MIT

package app

import "gioui.org/f32"

// PinchEvent is one step of a touchpad pinch gesture, returned by
// [Window.Event]. Local fork addition: only the Wayland driver emits it, on
// compositors that implement zwp_pointer_gestures_v1.
type PinchEvent struct {
	Kind PinchKind
	// Position is the pointer position in window pixels.
	Position f32.Point
	// Scale is the zoom factor since the previous event of the gesture
	// (1 means no change); multiply successive values to get the total.
	Scale float32
	// Pan is how far the fingers' centre moved since the previous event, in
	// pixels.
	Pan f32.Point
}

// PinchKind is the phase of a [PinchEvent].
type PinchKind uint8

const (
	PinchBegin PinchKind = iota
	PinchUpdate
	PinchEnd
	// PinchCancel ends a gesture the compositor cancelled.
	PinchCancel
)

func (PinchEvent) ImplementsEvent() {}

// merge folds the update n into e, so updates queued between two reads of
// Window.Event arrive as one.
func (e PinchEvent) merge(n PinchEvent) PinchEvent {
	e.Position = n.Position
	e.Scale *= n.Scale
	e.Pan = e.Pan.Add(n.Pan)
	return e
}
