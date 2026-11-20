package scan

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// box builds one ISO-BMFF box: a 32-bit big-endian size, a 4-byte type and the
// payload. The size includes the 8-byte header, as the format requires.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// largeBox builds a box with size == 1 and a 64-bit largesize, the header form
// used for boxes larger than 4 GiB. Here it is used to make the walker exercise
// the 16-byte header path on a tiny file.
func largeBox(typ string, payload []byte) []byte {
	b := make([]byte, 16+len(payload))
	binary.BigEndian.PutUint32(b[0:4], 1)
	copy(b[4:8], typ)
	binary.BigEndian.PutUint64(b[8:16], uint64(len(b)))
	copy(b[16:], payload)
	return b
}

// mvhdV0 builds a version 0 movie header box with the given timescale and
// 32-bit duration.
func mvhdV0(timescale, duration uint32) []byte {
	p := make([]byte, 20)
	p[0] = 0 // version; 3 flag bytes follow, left zero
	// creation(4) and modification(4) left zero
	binary.BigEndian.PutUint32(p[12:16], timescale)
	binary.BigEndian.PutUint32(p[16:20], duration)
	return box("mvhd", p)
}

// mvhdV1 builds a version 1 movie header box: 64-bit timestamps, 32-bit
// timescale, 64-bit duration.
func mvhdV1(timescale uint32, duration uint64) []byte {
	p := make([]byte, 32)
	p[0] = 1 // version
	// creation(8) and modification(8) left zero
	binary.BigEndian.PutUint32(p[20:24], timescale)
	binary.BigEndian.PutUint64(p[24:32], duration)
	return box("mvhd", p)
}

// ftyp is a minimal file type box, the first box of a normal ISO-BMFF file.
func ftyp() []byte {
	return box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2"))
}

// faststartMP4 builds a minimal faststart file — ftyp, moov holding a 6 s
// mvhd, then an mdat — i.e. moov before the media data. It returns 6000 ms.
func faststartMP4() []byte {
	var b []byte
	b = append(b, ftyp()...)
	b = append(b, box("moov", mvhdV0(1000, 6000))...)
	b = append(b, box("mdat", make([]byte, 256))...)
	return b
}

// writeMP4 writes content to a file named name inside dir and returns its path.
func writeMP4(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestMP4DurationMs covers the synthetic-file cases: faststart layout, moov
// after a large mdat (64-bit largesize header), a version 1 movie header, a
// fragmented movie (mvex), a truncated file, a non-ISO-BMFF extension and a
// zero timescale. S5-01.
func TestMP4DurationMs(t *testing.T) {
	dir := t.TempDir()

	// moov at the end, after a large mdat that must be skipped by size.
	moovAtEnd := append(ftyp(), largeBox("mdat", make([]byte, 1024))...)
	moovAtEnd = append(moovAtEnd, box("moov", mvhdV0(600, 4200))...)

	// Fragmented movie: mvhd plus mvex. A fragmented file's mvhd is not the
	// playback length, so the native reader must decline it.
	fragmented := append(ftyp(), box("moov", append(mvhdV0(1000, 6000), box("mvex", nil)...))...)

	// Truncated in the middle of moov, so its declared size overruns EOF.
	full := faststartMP4()
	truncated := full[:len(ftyp())+8+12]

	cases := []struct {
		name    string
		file    string
		content []byte
		wantMs  int64
		wantOK  bool
	}{
		{"faststart", "faststart.mp4", full, 6000, true},
		{"moov after large mdat", "late.mov", moovAtEnd, 7000, true},
		{"mvhd version 1", "v1.m4v", append(ftyp(), box("moov", mvhdV1(90000, 90000*12))...), 12000, true},
		{"fragmented mvex", "frag.3gp", fragmented, 0, false},
		{"truncated", "trunc.mp4", truncated, 0, false},
		{"mkv extension", "clip.mkv", full, 0, false},
		{"timescale zero", "zero.mp4", append(ftyp(), box("moov", mvhdV0(0, 6000))...), 0, false},
		{"duration zero", "nodur.mp4", append(ftyp(), box("moov", mvhdV0(1000, 0))...), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeMP4(t, dir, tc.file, tc.content)
			gotMs, gotOK := mp4DurationMs(p)
			if gotOK != tc.wantOK || gotMs != tc.wantMs {
				t.Fatalf("mp4DurationMs(%s) = (%d, %v), want (%d, %v)", tc.file, gotMs, gotOK, tc.wantMs, tc.wantOK)
			}
		})
	}
}

// TestMP4DurationMsMissingFile verifies an unreadable path is just a false
// result, not a panic, so the caller falls through to ffprobe.
func TestMP4DurationMsMissingFile(t *testing.T) {
	gotMs, gotOK := mp4DurationMs(filepath.Join(t.TempDir(), "absent.mp4"))
	if gotOK || gotMs != 0 {
		t.Fatalf("mp4DurationMs(absent) = (%d, %v), want (0, false)", gotMs, gotOK)
	}
}

// TestMP4DurationMsAgainstFfmpeg generates a real 2 s H.264 clip with ffmpeg
// and checks the native reader agrees with the requested length. Skipped when
// ffmpeg/ffprobe are unavailable or the encode fails (e.g. no libx264), so the
// suite stays hermetic.
func TestMP4DurationMsAgainstFfmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not on PATH")
	}
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	cmd := exec.Command("ffmpeg",
		"-loglevel", "error",
		"-y",
		"-f", "lavfi",
		"-i", "testsrc2=size=128x96:rate=25",
		"-t", "2",
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		clip,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not generate a clip: %v: %s", err, out)
	}

	ms, ok := mp4DurationMs(clip)
	if !ok {
		t.Fatal("mp4DurationMs found no duration in an ffmpeg-generated mp4")
	}
	if diff := ms - 2000; diff < -100 || diff > 100 {
		t.Fatalf("mp4DurationMs = %d ms, want 2000 ± 100", ms)
	}
}

// BenchmarkMP4DurationMs measures the native read path that replaces the
// per-video ffprobe fork (S5-01). It should sit in the microsecond range.
func BenchmarkMP4DurationMs(b *testing.B) {
	path := filepath.Join(b.TempDir(), "faststart.mp4")
	if err := os.WriteFile(path, faststartMP4(), 0o644); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if ms, ok := mp4DurationMs(path); !ok || ms != 6000 {
			b.Fatalf("mp4DurationMs = (%d, %v), want (6000, true)", ms, ok)
		}
	}
}
