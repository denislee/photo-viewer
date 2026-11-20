package scan

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// S5-01: read the duration of ISO base media file format (ISO-BMFF) videos
// straight from the movie header box instead of forking ffprobe per file. The
// box walk costs a handful of ReadAt calls (~10 µs); ffprobe costs ~100 ms of
// process startup plus decode, which dominated the video stage of a scan.
//
// The value returned here is the *movie* (mvhd)
// duration. It can differ from ffprobe's v:0 stream duration by tens of
// milliseconds when the audio track runs longer than the video track. That
// difference is accepted: durations are shown to humans (e.g. "0:06"), and they
// only need to be right to the second.

// maxBoxes bounds the box walk at each level so a pathological file (thousands
// of 8-byte boxes, or a self-referential size) can never turn the walk into a
// long loop. Real files have a few dozen boxes at most.
const maxBoxes = 4096

// mp4Extensions are the ISO-BMFF containers that carry a movie header box.
// Others (.mkv, .webm, .avi, .mts, …) are not ISO-BMFF and fall through to
// ffprobe.
var mp4Extensions = map[string]bool{
	".mp4": true,
	".mov": true,
	".m4v": true,
	".3gp": true,
}

// mp4Box is one parsed box header: its type and where its payload lives.
type mp4Box struct {
	typ        string
	payloadOff int64 // file offset of the first payload byte
	payloadLen int64 // payload bytes available inside the enclosing container
	end        int64 // file offset just past the box
}

// mp4DurationMs reads the duration of an ISO-BMFF video from its moov/mvhd box,
// in milliseconds. ok is false when path is not one of the supported extensions,
// the file is not a well-formed ISO-BMFF file (truncated, malformed box sizes),
// the movie header carries no usable duration (missing mvhd, zero timescale,
// zero or "unknown" duration), or the movie is fragmented (mvex present): a
// fragmented MP4's mvhd duration is not the playback length, so those go to
// ffprobe.
func mp4DurationMs(path string) (int64, bool) {
	if !mp4Extensions[strings.ToLower(filepath.Ext(path))] {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, false
	}
	size := st.Size()

	// Top level: find moov by jumping over each box's payload — moov often
	// follows a multi-gigabyte mdat, and we must not read the media data.
	moov, ok := findBox(f, 0, size, "moov")
	if !ok {
		return 0, false
	}

	// Children of moov: remember mvhd, and keep walking so a later mvex (the
	// "this is a fragmented movie" marker) still disqualifies the file.
	var (
		timescale uint32
		duration  uint64
		haveMvhd  bool
	)
	for off, boxes := moov.payloadOff, 0; off < moov.end; boxes++ {
		if boxes >= maxBoxes {
			return 0, false
		}
		b, ok := readBoxHeader(f, off, moov.end)
		if !ok {
			return 0, false
		}
		switch b.typ {
		case "mvex":
			return 0, false
		case "mvhd":
			ts, d, ok := readMvhd(f, b)
			if !ok {
				return 0, false
			}
			timescale, duration, haveMvhd = ts, d, true
		}
		off = b.end
	}
	if !haveMvhd {
		return 0, false
	}
	return durationMs(duration, timescale)
}

// findBox returns the first box of type typ among the siblings in [start, end).
func findBox(f *os.File, start, end int64, typ string) (mp4Box, bool) {
	for off, boxes := start, 0; off < end; boxes++ {
		if boxes >= maxBoxes {
			return mp4Box{}, false
		}
		b, ok := readBoxHeader(f, off, end)
		if !ok {
			return mp4Box{}, false
		}
		if b.typ == typ {
			return b, true
		}
		off = b.end
	}
	return mp4Box{}, false
}

// readBoxHeader parses the box header at off whose enclosing container ends at
// limit (limit <= file size). A 32-bit size of 1 means a 64-bit largesize
// follows the type, and a size of 0 means the box runs to the end of its
// container. Anything malformed — a box shorter than its own header, or one
// that overruns the container/EOF — reports false.
func readBoxHeader(f *os.File, off, limit int64) (mp4Box, bool) {
	if off < 0 || limit-off < 8 {
		return mp4Box{}, false
	}
	var buf [16]byte
	if _, err := f.ReadAt(buf[:8], off); err != nil {
		return mp4Box{}, false
	}
	size := int64(binary.BigEndian.Uint32(buf[0:4]))
	typ := string(buf[4:8])
	headerLen := int64(8)
	switch size {
	case 0:
		size = limit - off
	case 1:
		if limit-off < 16 {
			return mp4Box{}, false
		}
		if _, err := f.ReadAt(buf[8:16], off+8); err != nil {
			return mp4Box{}, false
		}
		large := binary.BigEndian.Uint64(buf[8:16])
		if large > math.MaxInt64 {
			return mp4Box{}, false
		}
		size = int64(large)
		headerLen = 16
	}
	if size < headerLen || size > limit-off {
		return mp4Box{}, false
	}
	return mp4Box{
		typ:        typ,
		payloadOff: off + headerLen,
		payloadLen: size - headerLen,
		end:        off + size,
	}, true
}

// readMvhd reads the timescale and duration out of an mvhd payload. Version 0
// stores both in 32 bits, version 1 in 64/32 bits after 64-bit timestamps.
func readMvhd(f *os.File, b mp4Box) (timescale uint32, duration uint64, ok bool) {
	if b.payloadLen < 4 {
		return 0, 0, false
	}
	var buf [32]byte
	n := int64(len(buf))
	if b.payloadLen < n {
		n = b.payloadLen
	}
	if _, err := f.ReadAt(buf[:n], b.payloadOff); err != nil {
		return 0, 0, false
	}
	switch buf[0] {
	case 0: // version, then 3 flag bytes
		const need = 20 // 4 + creation(4) + modification(4) + timescale(4) + duration(4)
		if n < need {
			return 0, 0, false
		}
		timescale = binary.BigEndian.Uint32(buf[12:16])
		duration = uint64(binary.BigEndian.Uint32(buf[16:20]))
		if duration == math.MaxUint32 {
			return 0, 0, false // "unknown" duration
		}
	case 1:
		const need = 32 // 4 + creation(8) + modification(8) + timescale(4) + duration(8)
		if n < need {
			return 0, 0, false
		}
		timescale = binary.BigEndian.Uint32(buf[20:24])
		duration = binary.BigEndian.Uint64(buf[24:32])
		if duration == math.MaxUint64 {
			return 0, 0, false // "unknown" duration
		}
	default:
		return 0, 0, false
	}
	return timescale, duration, true
}

// durationMs converts an mvhd duration/timescale pair to milliseconds without
// overflowing: the division happens first, and the remainder (always smaller
// than the 32-bit timescale) is scaled on its own. A duration too large to
// represent is treated as unusable rather than wrapped.
func durationMs(duration uint64, timescale uint32) (int64, bool) {
	if timescale == 0 || duration == 0 {
		return 0, false
	}
	whole := duration / uint64(timescale)
	if whole > math.MaxUint64/1000 {
		return 0, false
	}
	ms := whole*1000 + duration%uint64(timescale)*1000/uint64(timescale)
	if ms == 0 || ms > math.MaxInt64 {
		return 0, false
	}
	return int64(ms), true
}
