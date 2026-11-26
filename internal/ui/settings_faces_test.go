package ui

import (
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
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

// TestFaceCheckText covers the Recheck feedback: "Checking…" wins while a
// probe is in flight, and a finished Recheck appends its time.
func TestFaceCheckText(t *testing.T) {
	ok := face.Status{BinaryPath: "/bin/pv-face-detect", Working: true}
	at := time.Date(2026, 11, 25, 14, 3, 7, 0, time.Local)
	cases := []struct {
		name      string
		probed    bool
		checking  bool
		checkedAt time.Time
		want      string
	}{
		{"checking", true, true, at, "Checking the face helper…"},
		{"startup probe, no recheck", true, false, time.Time{}, "Helper: OK (/bin/pv-face-detect)"},
		{"after recheck", true, false, at, "Helper: OK (/bin/pv-face-detect) · checked 14:03:07"},
		{"not probed", false, false, at, "Checking the face helper…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := faceCheckText(ok, tc.probed, tc.checking, tc.checkedAt); got != tc.want {
				t.Errorf("faceCheckText = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFaceTaskText(t *testing.T) {
	cases := []struct {
		task ProcessSnapshot
		want string
	}{
		{ProcessSnapshot{Title: "Regrouping faces"}, "Regrouping faces"},
		{ProcessSnapshot{Title: "Detecting faces", Done: 3, Total: 10}, "Detecting faces: 3 / 10"},
		{ProcessSnapshot{Title: "Detecting faces", Done: 3, Total: 10, Paused: true}, "Detecting faces: 3 / 10 (paused)"},
	}
	for _, tc := range cases {
		if got := faceTaskText(tc.task); got != tc.want {
			t.Errorf("faceTaskText(%+v) = %q, want %q", tc.task, got, tc.want)
		}
	}
}

func TestPeopleCountText(t *testing.T) {
	one := PeopleSnapshot{People: []cache.Person{{}}, SmallGroups: []cache.Group{{}}}
	if got, want := peopleCountText(one), "1 person · 1 unnamed group"; got != want {
		t.Errorf("peopleCountText(one) = %q, want %q", got, want)
	}
	many := PeopleSnapshot{People: []cache.Person{{}}, Hidden: []cache.Person{{}}, Groups: []cache.Group{{}, {}}, SmallGroups: []cache.Group{{}}}
	if got, want := peopleCountText(many), "2 people · 3 unnamed groups"; got != want {
		t.Errorf("peopleCountText(many) = %q, want %q", got, want)
	}
}

func TestPassSummary(t *testing.T) {
	at := time.Date(2026, 11, 25, 9, 5, 0, 0, time.Local)
	if got, want := passSummary(at, 40, 40, 2, false), "Last scan 09:05: 40 photos checked, 2 sent for detection"; got != want {
		t.Errorf("finished: %q, want %q", got, want)
	}
	if got, want := passSummary(at, 12, 40, 5, true), "Last scan 09:05: stopped after 12 of 40 photos, 5 sent for detection"; got != want {
		t.Errorf("stopped: %q, want %q", got, want)
	}
}

func TestRecheckToast(t *testing.T) {
	cases := []struct {
		st   face.Status
		want string
	}{
		{face.Status{Working: true, BinaryPath: "/x"}, "Face helper OK"},
		{face.Status{}, "Face helper not installed"},
		{face.Status{BinaryPath: "/x", Error: "no dlib"}, "Face helper broken: no dlib"},
	}
	for _, tc := range cases {
		if got := recheckToast(tc.st); got != tc.want {
			t.Errorf("recheckToast(%+v) = %q, want %q", tc.st, got, tc.want)
		}
	}
}
