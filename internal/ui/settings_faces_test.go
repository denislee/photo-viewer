package ui

import (
	"testing"

	"github.com/dns/photo-viewer/internal/face"
)

// TestFaceStatusText covers the four display states of the helper probe and
// checks that "not probed yet" wins over any status fields, since an empty
// Status before the first probe must not read as "not installed".
func TestFaceStatusText(t *testing.T) {
	cases := []struct {
		name   string
		st     face.Status
		probed bool
		want   string
	}{
		{
			name:   "not probed yet",
			st:     face.Status{},
			probed: false,
			want:   "Checking the face helper…",
		},
		{
			name:   "not probed despite a status",
			st:     face.Status{BinaryPath: "/usr/bin/pv-face-detect", Working: true},
			probed: false,
			want:   "Checking the face helper…",
		},
		{
			name:   "working",
			st:     face.Status{BinaryPath: "/usr/bin/pv-face-detect", Working: true, Python: "/usr/bin/python3"},
			probed: true,
			want:   "Helper: OK (/usr/bin/pv-face-detect)",
		},
		{
			name:   "missing binary",
			st:     face.Status{Error: "pv-face-detect not found on PATH or next to the photo-viewer binary"},
			probed: true,
			want:   "Helper not installed. Install with: pipx install face_recognition",
		},
		{
			name:   "broken binary",
			st:     face.Status{BinaryPath: "/opt/pv-face-detect", Error: "ImportError: No module named dlib"},
			probed: true,
			want:   "Helper broken: ImportError: No module named dlib",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := faceStatusText(tc.st, tc.probed); got != tc.want {
				t.Errorf("faceStatusText(%+v, %v) = %q, want %q", tc.st, tc.probed, got, tc.want)
			}
		})
	}
}

// TestNormaliseStrictness checks that valid presets pass through and empty or
// unknown config values fall back to normal.
func TestNormaliseStrictness(t *testing.T) {
	valid := []string{StrictnessStrict, StrictnessNormal, StrictnessLoose}
	for _, s := range valid {
		if got := normaliseStrictness(s); got != s {
			t.Errorf("normaliseStrictness(%q) = %q, want %q", s, got, s)
		}
	}
	invalid := []string{"", "unknown", "Strict", "tight"}
	for _, s := range invalid {
		if got := normaliseStrictness(s); got != StrictnessNormal {
			t.Errorf("normaliseStrictness(%q) = %q, want %q", s, got, StrictnessNormal)
		}
	}
}
