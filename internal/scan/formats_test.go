package scan

import "testing"

// TestDetectType covers the extension→MediaType mapping, with particular
// attention to the extensions added in S-11 and to case-insensitivity (real
// filesystems mix "IMG_0001.JPG" and "clip.MpG").
func TestDetectType(t *testing.T) {
	cases := []struct {
		path string
		want MediaType
	}{
		// Baseline photos / existing types (sanity anchors).
		{"a.jpg", TypePhoto},
		{"a.png", TypePhoto},
		{"a.cr2", TypeRAW},
		{"a.heic", TypeHEIC},
		{"a.mp4", TypeVideo},

		// S-11 video additions (ffmpeg-decoded via thumb.Video).
		{"clip.3gp", TypeVideo},
		{"clip.wmv", TypeVideo},
		{"clip.flv", TypeVideo},
		{"clip.mpg", TypeVideo},
		{"clip.mpeg", TypeVideo},

		// S-11 RAW additions (exiftool preview path via thumb.RAW).
		{"shot.pef", TypeRAW}, // Pentax
		{"shot.srw", TypeRAW}, // Samsung
		{"shot.nrw", TypeRAW}, // Nikon
		{"shot.x3f", TypeRAW}, // Sigma

		// S-11 AVIF: routed to the HEIC backend so it reaches ffmpeg/heif-convert.
		{"pic.avif", TypeHEIC},

		// S-11 JXL is intentionally NOT detected (no working backend).
		{"pic.jxl", TypeUnknown},

		// Case-insensitivity: uppercase and mixed-case extensions resolve the
		// same as lowercase.
		{"CLIP.MPEG", TypeVideo},
		{"CLIP.3GP", TypeVideo},
		{"SHOT.X3F", TypeRAW},
		{"PIC.AVIF", TypeHEIC},
		{"IMG.JPG", TypePhoto},

		// No extension / genuinely unknown.
		{"README", TypeUnknown},
		{"notes.txt", TypeUnknown},
	}

	for _, c := range cases {
		if got := DetectType(c.path); got != c.want {
			t.Errorf("DetectType(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
