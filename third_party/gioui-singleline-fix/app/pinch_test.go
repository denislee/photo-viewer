// SPDX-License-Identifier: Unlicense OR MIT

package app

import (
	"testing"

	"gioui.org/f32"
)

func TestPinchUpdatesCoalesce(t *testing.T) {
	w := new(Window)
	w.processEvent(PinchEvent{Kind: PinchBegin, Scale: 1})
	w.processEvent(PinchEvent{Kind: PinchUpdate, Scale: 2, Pan: f32.Pt(1, 2)})
	w.processEvent(PinchEvent{Kind: PinchUpdate, Scale: 1.5, Pan: f32.Pt(3, 4)})
	w.processEvent(PinchEvent{Kind: PinchEnd, Scale: 1})
	var got []PinchEvent
	for {
		e, ok := w.nextEvent()
		if !ok {
			break
		}
		if p, ok := e.(PinchEvent); ok {
			got = append(got, p)
		}
	}
	if len(got) != 3 || got[0].Kind != PinchBegin || got[2].Kind != PinchEnd {
		t.Fatalf("events = %+v", got)
	}
	if u := got[1]; u.Kind != PinchUpdate || u.Scale != 3 || u.Pan != f32.Pt(4, 6) {
		t.Fatalf("merged update = %+v", u)
	}
}
