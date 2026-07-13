package video

import "testing"

// alphaPrefilled reports whether every 4th byte (the alpha column of a
// tightly-packed RGBA) is 0xff and the header is consistent for w×h.
func alphaPrefilled(t *testing.T, p *Player, w, h int) {
	t.Helper()
	if p.buf == nil {
		t.Fatalf("buf is nil")
	}
	if got := p.buf.Rect.Dx(); got != w {
		t.Fatalf("width = %d, want %d", got, w)
	}
	if got := p.buf.Rect.Dy(); got != h {
		t.Fatalf("height = %d, want %d", got, h)
	}
	if got := p.buf.Stride; got != 4*w {
		t.Fatalf("stride = %d, want %d", got, 4*w)
	}
	if got := len(p.buf.Pix); got != 4*w*h {
		t.Fatalf("len(Pix) = %d, want %d", got, 4*w*h)
	}
	for i := 3; i < len(p.buf.Pix); i += 4 {
		if p.buf.Pix[i] != 0xff {
			t.Fatalf("alpha byte at %d = %#x, want 0xff", i, p.buf.Pix[i])
		}
	}
}

// TestEnsureBufReuseAndGrow exercises the buffer-management helper without a
// live mpv context: it verifies the header/stride/alpha invariants and that
// the backing Pix array is reused when a new size fits existing capacity
// (shrink then grow), and only reallocated when the size exceeds capacity.
func TestEnsureBufReuseAndGrow(t *testing.T) {
	p := &Player{}

	// Fresh allocation.
	p.ensureBuf(640, 480)
	alphaPrefilled(t, p, 640, 480)
	big := &p.buf.Pix[0]
	bigCap := cap(p.buf.Pix)

	// No-op when the size is unchanged: same header pointer, still valid.
	prev := p.buf
	p.ensureBuf(640, 480)
	if p.buf != prev {
		t.Fatalf("ensureBuf reallocated header for an unchanged size")
	}

	// Shrink: must reuse the same backing array (capacity is preserved).
	p.ensureBuf(320, 240)
	alphaPrefilled(t, p, 320, 240)
	if &p.buf.Pix[0] != big {
		t.Fatalf("shrink reallocated the backing array; want reuse")
	}
	if cap(p.buf.Pix) != bigCap {
		t.Fatalf("shrink changed capacity: got %d want %d", cap(p.buf.Pix), bigCap)
	}

	// Grow back within the retained capacity: still the same backing array.
	p.ensureBuf(640, 480)
	alphaPrefilled(t, p, 640, 480)
	if &p.buf.Pix[0] != big {
		t.Fatalf("grow-within-capacity reallocated the backing array; want reuse")
	}

	// Grow beyond capacity: must allocate a fresh, larger array.
	p.ensureBuf(1920, 1080)
	alphaPrefilled(t, p, 1920, 1080)
	if &p.buf.Pix[0] == big {
		t.Fatalf("grow-beyond-capacity reused the too-small backing array")
	}
	if cap(p.buf.Pix) < 4*1920*1080 {
		t.Fatalf("grown capacity %d too small for %d bytes", cap(p.buf.Pix), 4*1920*1080)
	}
}

// TestReleaseBufFreesBuffer covers the buffer-freeing that Stop performs:
// releaseBuf drops the reference so a window-sized RGBA is not retained for
// the rest of the session after playback stops. Headless — no mpv needed.
func TestReleaseBufFreesBuffer(t *testing.T) {
	p := &Player{}
	p.ensureBuf(1920, 1080)
	if p.buf == nil {
		t.Fatalf("ensureBuf did not allocate a buffer")
	}
	p.releaseBuf()
	if p.buf != nil {
		t.Fatalf("releaseBuf did not free the buffer")
	}
	// A subsequent ensureBuf must allocate fresh (no stale capacity to reuse).
	p.ensureBuf(64, 64)
	alphaPrefilled(t, p, 64, 64)
}
