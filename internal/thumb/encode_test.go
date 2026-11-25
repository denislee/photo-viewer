package thumb

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"testing"

	"golang.org/x/image/draw"
)

func TestEncodeJPEGDecodes(t *testing.T) {
	const w, h = 96, 64
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	nrgba := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{R: uint8(x * 2), G: uint8(y * 3), B: uint8(x + y), A: 0xff}
			rgba.SetRGBA(x, y, c)
			nrgba.SetNRGBA(x, y, color.NRGBA(c))
		}
	}
	for _, tc := range []struct {
		name string
		img  image.Image
	}{
		{"rgba", rgba},
		{"nrgba", nrgba},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := EncodeJPEG(&buf, tc.img, 82); err != nil {
				t.Fatalf("EncodeJPEG: %v", err)
			}
			dec, err := jpeg.Decode(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if b := dec.Bounds(); b != image.Rect(0, 0, w, h) {
				t.Fatalf("decoded bounds %v, want %dx%d", b, w, h)
			}
		})
	}
}

func TestScaleCatmullRomMatchesKernelScale(t *testing.T) {
	const sw, sh, dw, dh = 300, 200, 150, 100
	rng := rand.New(rand.NewSource(1))
	src := image.NewRGBA(image.Rect(0, 0, sw, sh))
	for i := range src.Pix {
		src.Pix[i] = uint8(rng.Intn(256))
	}
	// Opaque alpha so both paths take the same fast path.
	for i := 3; i < len(src.Pix); i += 4 {
		src.Pix[i] = 0xff
	}

	want := image.NewRGBA(image.Rect(0, 0, dw, dh))
	draw.CatmullRom.Scale(want, want.Rect, src, src.Bounds(), draw.Src, nil)

	// Twice: the second run must reuse the cached scaler and still match.
	for i := range 2 {
		got := image.NewRGBA(image.Rect(0, 0, dw, dh))
		ScaleCatmullRom(got, got.Rect, src, src.Bounds())
		if !bytes.Equal(got.Pix, want.Pix) {
			t.Fatalf("run %d: ScaleCatmullRom differs from draw.CatmullRom.Scale", i+1)
		}
	}
}

func BenchmarkEncodeJPEG2048(b *testing.B) {
	const w, h = 2048, 1536
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			i := img.PixOffset(x, y)
			img.Pix[i+0] = uint8(x)
			img.Pix[i+1] = uint8(y)
			img.Pix[i+2] = uint8(x ^ y)
			img.Pix[i+3] = 0xff
		}
	}
	b.ReportAllocs()
	b.SetBytes(int64(w * h * 4))
	var buf bytes.Buffer
	for b.Loop() {
		buf.Reset()
		if err := EncodeJPEG(&buf, img, 82); err != nil {
			b.Fatal(err)
		}
	}
}
