package tools

import (
	"os/exec"
	"testing"
)

func TestProbes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		probe func() bool
	}{
		{"ffmpeg", Ffmpeg},
		{"ffprobe", FFprobe},
		{"exiftool", Exiftool},
		{"heif-convert", HeifConvert},
		{"udisksctl", Udisksctl},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := exec.LookPath(tc.name)
			want := err == nil
			if got := tc.probe(); got != want {
				t.Fatalf("probe() = %v, want %v (exec.LookPath err = %v)", got, want, err)
			}
			if second := tc.probe(); second != want {
				t.Fatalf("second probe() = %v, want %v (result not cached)", second, want)
			}
		})
	}
}
