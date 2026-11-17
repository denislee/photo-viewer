package jpegturbo

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

func encodeJPEG(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPickScale(t *testing.T) {
	factors := [][2]int{{1, 1}, {7, 8}, {3, 4}, {5, 8}, {1, 2}, {3, 8}, {1, 4}, {1, 8}, {2, 1}}
	cases := []struct {
		w, h, minW, minH int
		num, denom       int
	}{
		{4032, 3024, 0, 0, 1, 1},
		{4032, 3024, 1440, 1080, 3, 8}, // 1512x1134
		{4032, 3024, 504, 378, 1, 8},
		{4032, 3024, 505, 0, 1, 4},
		{4032, 3024, 5000, 5000, 1, 1}, // never upscale
		{4032, 3024, 256, 256, 1, 8},
	}
	for _, c := range cases {
		n, d := pickScale(factors, c.w, c.h, c.minW, c.minH)
		if n*c.denom != c.num*d {
			t.Errorf("pickScale(%dx%d, min %dx%d) = %d/%d, want %d/%d", c.w, c.h, c.minW, c.minH, n, d, c.num, c.denom)
		}
	}
}

func TestDecode(t *testing.T) {
	if !Available() {
		t.Skip("built without libjpeg-turbo")
	}
	data := encodeJPEG(t, 200, 120)
	cfg, err := DecodeConfig(data)
	if err != nil || cfg.Width != 200 || cfg.Height != 120 {
		t.Fatalf("DecodeConfig = %+v, %v", cfg, err)
	}
	img, err := Decode(data, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if img.Rect != image.Rect(0, 0, 200, 120) {
		t.Fatalf("full decode bounds %v", img.Rect)
	}
	ref, _ := jpeg.Decode(bytes.NewReader(data))
	r1, g1, b1, _ := ref.At(100, 60).RGBA()
	c := img.RGBAAt(100, 60)
	if diff(r1>>8, c.R) > 4 || diff(g1>>8, c.G) > 4 || diff(b1>>8, c.B) > 4 || c.A != 0xff {
		t.Fatalf("pixel %v differs from image/jpeg (%d,%d,%d)", c, r1>>8, g1>>8, b1>>8)
	}

	small, err := Decode(data, 50, 30)
	if err != nil {
		t.Fatal(err)
	}
	if small.Rect != image.Rect(0, 0, 50, 30) {
		t.Fatalf("1/4 decode bounds %v, want 50x30", small.Rect)
	}

	if _, err := Decode([]byte("not a jpeg"), 0, 0); err == nil {
		t.Fatal("garbage decoded without error")
	}
	if _, err := Decode(data[:len(data)/3], 0, 0); err != nil {
		t.Logf("truncated JPEG: %v (acceptable)", err)
	}
}

func TestDecodeFit(t *testing.T) {
	if !Available() {
		t.Skip("built without libjpeg-turbo")
	}
	data := encodeJPEG(t, 400, 300)
	calls := 0
	var gotW, gotH int
	img, cfg, err := DecodeFit(data, func(w, h int) (int, int) {
		calls++
		gotW, gotH = w, h
		return 100, 75
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("fit called %d times, want 1", calls)
	}
	if gotW != 400 || gotH != 300 {
		t.Fatalf("fit called with %dx%d, want 400x300", gotW, gotH)
	}
	if cfg != (Config{Width: 400, Height: 300}) {
		t.Fatalf("cfg = %+v, want {400 300}", cfg)
	}
	if b := img.Bounds(); b.Dx() < 100 || b.Dy() < 75 || b.Dx() >= 400 {
		t.Fatalf("scaled decode bounds %v, want width in [100,400) and height >= 75", b)
	}

	full, cfg, err := DecodeFit(data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != (Config{Width: 400, Height: 300}) {
		t.Fatalf("nil fit cfg = %+v, want {400 300}", cfg)
	}
	if full.Rect != image.Rect(0, 0, 400, 300) {
		t.Fatalf("nil fit bounds %v, want 400x300", full.Rect)
	}
}

func diff(a uint32, b uint8) int {
	d := int(a) - int(b)
	if d < 0 {
		return -d
	}
	return d
}

func BenchmarkDecode12MP(b *testing.B) {
	if !Available() {
		b.Skip("built without libjpeg-turbo")
	}
	data := encodeJPEG(b, 4032, 3024)
	for _, sc := range []struct {
		name       string
		minW, minH int
	}{{"full", 0, 0}, {"1080p", 1440, 1080}} {
		b.Run(sc.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := Decode(data, sc.minW, sc.minH); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("image-jpeg", func(b *testing.B) {
		for b.Loop() {
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})
}
