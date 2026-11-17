package imgorient

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// tiffFixture builds a minimal TIFF file whose IFD0 holds the given entries, so
// the fast-path parser in tiffOrientation can be exercised without any real
// camera file. Layout, per the TIFF 6.0 spec:
//
//	header:  "II", 42, 0 (little endian) or "MM", 0, 42 (big endian)
//	u32      IFD0 offset = 8
//	u16      entry count
//	n × 12B  entry: u16 tag, u16 type, u32 count=1, u32 value (SHORT in the
//	         first two bytes of the value field, zeros in the other two)
//	u32      next-IFD offset = 0
//
// padTo > len pads the file with zero bytes, which is how a real 36 MB RAW has
// its huge pixel payload after the (tiny) IFD. Entries must be sorted by tag,
// as a real file would be.
func tiffFixture(bigEndian bool, entries [][3]uint16, padTo int) []byte {
	var bo binary.ByteOrder = binary.LittleEndian
	var magic []byte
	if bigEndian {
		bo = binary.BigEndian
		magic = []byte{'M', 'M', 0, 42}
	} else {
		magic = []byte{'I', 'I', 42, 0}
	}

	size := 8 + 2 + 12*len(entries) + 4
	if padTo > size {
		size = padTo
	}
	data := make([]byte, size)
	copy(data, magic)
	bo.PutUint32(data[4:], 8)

	off := 8
	bo.PutUint16(data[off:], uint16(len(entries)))
	off += 2
	for _, e := range entries {
		bo.PutUint16(data[off:], e[0])   // tag
		bo.PutUint16(data[off+2:], e[1]) // type
		bo.PutUint32(data[off+4:], 1)    // count
		bo.PutUint16(data[off+8:], e[2]) // value, first two bytes only
		off += 12
	}
	bo.PutUint32(data[off:], 0) // no next IFD
	return data
}

// TestTIFFOrientation covers the direct IFD0 fast path: both byte orders, the
// tag-absent case, out-of-range values, the wrong field type, a truncated IFD
// (both mid-entry and before the entry count) and an IFD offset pointing past
// the end of the file. Every malformed case must degrade to the same safe
// default goexif would produce: 1.
func TestTIFFOrientation(t *testing.T) {
	ii := func(entries [][3]uint16, padTo int) []byte { return tiffFixture(false, entries, padTo) }
	mm := func(entries [][3]uint16, padTo int) []byte { return tiffFixture(true, entries, padTo) }

	withOrientation6 := [][3]uint16{{0x0100, 3, 1}, {0x0112, 3, 6}}

	beyondEnd := make([]byte, 8)
	copy(beyondEnd, []byte{'I', 'I', 42, 0})
	binary.LittleEndian.PutUint32(beyondEnd[4:], 0xFFFF)

	cases := []struct {
		name string
		data []byte
		want int
	}{
		{
			name: "II orientation 6",
			data: ii([][3]uint16{{0x0100, 3, 4000}, {0x0112, 3, 6}}, 0),
			want: 6,
		},
		{
			name: "MM orientation 6",
			data: mm([][3]uint16{{0x0100, 3, 4000}, {0x0112, 3, 6}}, 0),
			want: 6,
		},
		{
			name: "tag absent",
			data: ii([][3]uint16{{0x0100, 3, 4000}}, 0),
			want: 1,
		},
		{
			name: "orientation 9 out of range",
			data: ii([][3]uint16{{0x0100, 3, 4000}, {0x0112, 3, 9}}, 0),
			want: 1,
		},
		{
			name: "orientation 0 out of range",
			data: ii([][3]uint16{{0x0100, 3, 4000}, {0x0112, 3, 0}}, 0),
			want: 1,
		},
		{
			name: "orientation stored as LONG",
			data: ii([][3]uint16{{0x0100, 3, 4000}, {0x0112, 4, 6}}, 0),
			want: 1,
		},
		{
			name: "truncated mid second entry",
			data: ii(withOrientation6, 0)[:8+2+12+4],
			want: 1,
		},
		{
			name: "truncated before entry count",
			data: ii(withOrientation6, 0)[:9],
			want: 1,
		},
		{
			name: "IFD offset beyond end",
			data: beyondEnd,
			want: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReadOrientationFrom(bytes.NewReader(c.data)); got != c.want {
				t.Fatalf("ReadOrientationFrom = %d, want %d", got, c.want)
			}
			path := filepath.Join(t.TempDir(), "fixture.tif")
			if err := os.WriteFile(path, c.data, 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			if got := ReadOrientation(path); got != c.want {
				t.Fatalf("ReadOrientation = %d, want %d", got, c.want)
			}
		})
	}
}

// TestTIFFOrientationNotAtStart pins the "only parse when the stream is at its
// start" guard: after one byte has been consumed, the TIFF magic is shifted out
// of position, so the fast path must be skipped and the fallback goexif decode
// fails, yielding 1.
func TestTIFFOrientationNotAtStart(t *testing.T) {
	data := tiffFixture(true, [][3]uint16{{0x0100, 3, 4000}, {0x0112, 3, 6}}, 0)
	r := bytes.NewReader(data)
	if _, err := r.ReadByte(); err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if got := ReadOrientationFrom(r); got != 1 {
		t.Fatalf("ReadOrientationFrom after 1 byte read = %d, want 1", got)
	}
}

// TestTIFFOrientationAllocs is the T4-01 guard: reading the orientation of a
// 30 MiB RAW-style TIFF must not buffer the file. It must do a couple of tiny
// 8/2/12-byte reads — no per-call allocation beyond the interface plumbing, and
// no allocation proportional to the file size.
func TestTIFFOrientationAllocs(t *testing.T) {
	data := tiffFixture(false, [][3]uint16{{0x0100, 3, 4000}, {0x0112, 3, 6}}, 30<<20)
	r := bytes.NewReader(data)

	// The fast path holds one fixed 768-byte buffer, which escapes to the heap
	// because it reaches ReadAt through an interface. What must never happen is
	// an allocation proportional to the 30 MiB payload, which is what goexif's
	// double buffering used to do.
	const maxAllocsPerCall = 1
	allocs := testing.AllocsPerRun(20, func() {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("Seek: %v", err)
		}
		if got := ReadOrientationFrom(r); got != 6 {
			t.Fatalf("ReadOrientationFrom = %d, want 6", got)
		}
	})
	if allocs > maxAllocsPerCall {
		t.Fatalf("ReadOrientationFrom allocated %.1f objects per call on a 30 MiB TIFF, want <= %d",
			allocs, maxAllocsPerCall)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const calls = 10
	for range calls {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("Seek: %v", err)
		}
		if got := ReadOrientationFrom(r); got != 6 {
			t.Fatalf("ReadOrientationFrom = %d, want 6", got)
		}
	}
	runtime.ReadMemStats(&after)
	if perCall := (after.TotalAlloc - before.TotalAlloc) / calls; perCall >= 64*1024 {
		t.Fatalf("ReadOrientationFrom allocated %d bytes per call on a 30 MiB TIFF, want < 64 KiB", perCall)
	}
}

// BenchmarkReadOrientationTIFF30MB measures the end-to-end cost of the fast
// path on a 30 MiB TIFF, the size where goexif's double buffering used to cost
// tens of milliseconds and hundreds of megabytes of allocation.
func BenchmarkReadOrientationTIFF30MB(b *testing.B) {
	data := tiffFixture(false, [][3]uint16{{0x0100, 3, 4000}, {0x0112, 3, 6}}, 30<<20)
	r := bytes.NewReader(data)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			b.Fatalf("Seek: %v", err)
		}
		ReadOrientationFrom(r)
	}
}
