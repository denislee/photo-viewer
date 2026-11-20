package face

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsPythonScript covers the cheap shebang sniff used by spawnDaemon to
// decide whether the helper can be launched through the interpreter --check
// found (F5-02).
func TestIsPythonScript(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		content []byte
		want    bool
	}{
		{"python shebang", []byte("#!/usr/bin/env python3\nimport face_recognition\n"), true},
		{"shell shebang", []byte("#!/bin/sh\n"), false},
		{"binary garbage", []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01, 0xff, 0xfe}, false},
		// The sniff only inspects the shebang line, so a shell script whose
		// body mentions python must not be mistaken for one.
		{"python in body", []byte("#!/bin/sh\nexec python3 \"$@\"\n"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, tc.content, 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			if got := isPythonScript(path); got != tc.want {
				t.Errorf("isPythonScript(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if isPythonScript(filepath.Join(dir, "does-not-exist")) {
			t.Error("isPythonScript(missing) = true, want false")
		}
	})
}

// TestProbeRecordsInterpreter checks that Probe stores the interpreter the
// helper reports after "ok " (the value spawnDaemon later launches the script
// under). It installs a fake helper at the head of PATH so no real Python or
// dlib install is required.
func TestProbeRecordsInterpreter(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, BinaryName)
	const script = `#!/bin/sh
if [ "$1" = "--check" ]; then
	printf 'ok /usr/bin/python3'
	exit 0
fi
exit 1
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}

	// resolveBinary prefers PATH, which t.Setenv replaces for this test only.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	bin, err := resolveBinary()
	if err != nil {
		t.Fatalf("resolveBinary: %v", err)
	}
	if bin != fake {
		// resolveBinary would only land elsewhere if PATH lookup failed; in
		// that case the probe would run a real helper and prove nothing.
		t.Skipf("resolveBinary resolved %q instead of the fake helper %q", bin, fake)
	}

	s := Probe()
	if !s.Working {
		t.Fatalf("Probe() not working, error = %q", s.Error)
	}
	if s.Python != "/usr/bin/python3" {
		t.Errorf("Probe().Python = %q, want %q", s.Python, "/usr/bin/python3")
	}
}
