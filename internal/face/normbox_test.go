package face

import (
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
)

// TestNormBox guards the W5 normalisation: the helper-reported size wins, the
// rendition's JPEG header is the fallback, and an unknown size yields NoNBox
// (so the tag matcher falls back to the embedding).
func TestNormBox(t *testing.T) {
	dir := t.TempDir()
	rend := filepath.Join(dir, "r.jpg")
	f, err := os.Create(rend)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, image.NewRGBA(image.Rect(0, 0, 200, 100)), nil); err != nil {
		t.Fatal(err)
	}
	f.Close()

	d := Detection{BBox: [4]int{50, 25, 20, 10}}
	if got, want := normBox(Detection{BBox: d.BBox, ImgW: 100, ImgH: 50}, rend), [4]float64{0.5, 0.5, 0.2, 0.2}; got != want {
		t.Errorf("helper size: got %v, want %v", got, want)
	}
	if got, want := normBox(d, rend), [4]float64{0.25, 0.25, 0.1, 0.1}; got != want {
		t.Errorf("header fallback: got %v, want %v", got, want)
	}
	if got := normBox(d, filepath.Join(dir, "missing.jpg")); got != cache.NoNBox {
		t.Errorf("unknown size: got %v, want NoNBox", got)
	}
}
