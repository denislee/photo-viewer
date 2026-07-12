package thumb

import (
	"os/exec"
	"testing"
)

// TestToolProbesMatchLookPathAndAreStable checks the cached external-tool
// probes (S-06): each must agree with a fresh exec.LookPath for its tool and
// return the same value on a repeat call (the point of the OnceValue cache is
// that presence is resolved once and never re-walks $PATH).
func TestToolProbesMatchLookPathAndAreStable(t *testing.T) {
	cases := []struct {
		tool  string
		probe func() bool
	}{
		{"ffmpeg", haveFfmpeg},
		{"exiftool", haveExiftool},
		{"heif-convert", haveHeifConvert},
	}
	for _, c := range cases {
		_, err := exec.LookPath(c.tool)
		want := err == nil
		if got := c.probe(); got != want {
			t.Errorf("%s probe = %v, want %v (LookPath err=%v)", c.tool, got, want, err)
		}
		// Cached OnceValue must return the same result on the second call.
		if got := c.probe(); got != want {
			t.Errorf("%s probe not stable: second call = %v, want %v", c.tool, got, want)
		}
	}
}
