package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// Embedded silent video player. Uses ffmpeg to decode the source into a stream
// of raw RGBA frames piped on stdout, paced at native rate via -re. Frames are
// blitted into a shared canvas.Image. No audio (cf. CLAUDE.md option 1).

const (
	maxVideoW = 1280
	maxVideoH = 720
)

type videoPlayer struct {
	src    string
	dispW  int
	dispH  int
	dur    float64
	fps    float64

	display  *canvas.Image
	playBtn  *widget.Button
	progress *widget.Slider
	timeLbl  *widget.Label
	bar      *fyne.Container

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	playing  bool
	position float64
	dragging bool
	closed   bool
}

type ffprobeResult struct {
	Streams []struct {
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		RFrameRate   string `json:"r_frame_rate"`
		Tags         struct {
			Rotate string `json:"rotate"`
		} `json:"tags"`
		SideDataList []struct {
			Rotation float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// probeVideo returns the *display* width/height — i.e. with any rotation
// metadata already applied. ffmpeg auto-rotates by default when decoding, so
// the frames we receive from the scale filter must be sized in the rotated
// orientation, otherwise vertical phone clips come out stretched.
func probeVideo(path string) (w, h int, fps, dur float64, err error) {
	if _, statErr := os.Stat(path); statErr != nil {
		return 0, 0, 0, 0, statErr
	}
	if _, lookErr := exec.LookPath("ffprobe"); lookErr != nil {
		return 0, 0, 0, 0, errors.New("ffprobe not installed")
	}
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,r_frame_rate:stream_tags=rotate:stream_side_data=rotation:format=duration",
		"-of", "json", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var r ffprobeResult
	if err := json.Unmarshal(out, &r); err != nil {
		return 0, 0, 0, 0, err
	}
	if len(r.Streams) == 0 {
		return 0, 0, 0, 0, errors.New("no video stream")
	}
	s := r.Streams[0]
	w, h = s.Width, s.Height
	if w == 0 || h == 0 {
		return 0, 0, 0, 0, errors.New("no video dimensions")
	}

	rot := 0
	if s.Tags.Rotate != "" {
		if v, e := strconv.Atoi(s.Tags.Rotate); e == nil {
			rot = v
		}
	}
	if len(s.SideDataList) > 0 && s.SideDataList[0].Rotation != 0 {
		rot = int(s.SideDataList[0].Rotation)
	}
	rot = ((rot % 360) + 360) % 360
	if rot == 90 || rot == 270 {
		w, h = h, w
	}

	if parts := strings.Split(s.RFrameRate, "/"); len(parts) == 2 {
		num, _ := strconv.ParseFloat(parts[0], 64)
		den, _ := strconv.ParseFloat(parts[1], 64)
		if den > 0 {
			fps = num / den
		}
	}
	if fps <= 0 || fps > 240 {
		fps = 30
	}
	dur, _ = strconv.ParseFloat(r.Format.Duration, 64)
	return w, h, fps, dur, nil
}

// newVideoPlayer probes src and builds the controls. The caller owns the
// display image and supplies it so the player can swap in raw frames in place
// of the photo viewer's existing canvas.Image.
func newVideoPlayer(src string, display *canvas.Image) (*videoPlayer, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, errors.New("ffmpeg not installed")
	}
	w, h, fps, dur, err := probeVideo(src)
	if err != nil {
		return nil, err
	}
	dispW, dispH := scaleToFit(w, h, maxVideoW, maxVideoH)
	// even dims for codec/scaler alignment
	dispW &^= 1
	dispH &^= 1
	if dispW < 2 || dispH < 2 {
		return nil, errors.New("bad video dimensions")
	}

	p := &videoPlayer{
		src:   src,
		dispW: dispW,
		dispH: dispH,
		dur:   dur,
		fps:   fps,
		display: display,
	}

	durLbl := formatTime(dur)
	p.timeLbl = widget.NewLabel("0:00 / " + durLbl)
	p.progress = widget.NewSlider(0, dur)
	if dur <= 0 {
		p.progress.Max = 1
	}
	p.progress.Step = 0.05
	p.progress.OnChanged = func(v float64) {
		p.mu.Lock()
		p.dragging = true
		p.mu.Unlock()
		fyne.Do(func() {
			p.timeLbl.SetText(formatTime(v) + " / " + durLbl)
		})
	}
	p.progress.OnChangeEnded = func(v float64) {
		p.seek(v)
		p.mu.Lock()
		p.dragging = false
		p.mu.Unlock()
	}
	p.playBtn = widget.NewButtonWithIcon("", theme.MediaPlayIcon(), p.toggle)
	p.bar = container.NewBorder(nil, nil, p.playBtn, p.timeLbl, p.progress)

	// Auto-play on open. The decoder publishes frames as soon as it has them,
	// so there's no need to fetch a separate first frame.
	p.play()
	return p, nil
}

func scaleToFit(w, h, maxW, maxH int) (int, int) {
	if w <= maxW && h <= maxH {
		return w, h
	}
	rw := float64(maxW) / float64(w)
	rh := float64(maxH) / float64(h)
	r := rw
	if rh < r {
		r = rh
	}
	return int(float64(w) * r), int(float64(h) * r)
}

func formatTime(s float64) string {
	if s < 0 || s != s { // negative or NaN
		s = 0
	}
	t := int(s)
	return fmt.Sprintf("%d:%02d", t/60, t%60)
}

func (p *videoPlayer) Bar() fyne.CanvasObject { return p.bar }

func (p *videoPlayer) toggle() {
	p.mu.Lock()
	playing := p.playing
	p.mu.Unlock()
	if playing {
		p.pause()
	} else {
		p.play()
	}
}

func (p *videoPlayer) play() {
	p.mu.Lock()
	if p.closed || p.playing {
		p.mu.Unlock()
		return
	}
	pos := p.position
	if pos >= p.dur-0.05 {
		pos = 0
		p.position = 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	p.playing = true
	done := p.done
	p.mu.Unlock()
	fyne.Do(func() { p.playBtn.SetIcon(theme.MediaPauseIcon()) })
	go p.runDecoder(ctx, pos, done)
}

func (p *videoPlayer) pause() {
	p.mu.Lock()
	if !p.playing {
		p.mu.Unlock()
		return
	}
	c := p.cancel
	d := p.done
	p.playing = false
	p.mu.Unlock()
	if c != nil {
		c()
	}
	if d != nil {
		<-d
	}
	fyne.Do(func() { p.playBtn.SetIcon(theme.MediaPlayIcon()) })
}

func (p *videoPlayer) seek(to float64) {
	if to < 0 {
		to = 0
	}
	if p.dur > 0 && to > p.dur {
		to = p.dur
	}
	p.mu.Lock()
	wasPlaying := p.playing
	c := p.cancel
	d := p.done
	p.position = to
	p.playing = false
	p.mu.Unlock()
	if c != nil {
		c()
	}
	if d != nil {
		<-d
	}
	if wasPlaying {
		p.play()
	} else {
		go p.showFrameAt(to)
	}
}

func (p *videoPlayer) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	c := p.cancel
	d := p.done
	p.playing = false
	p.mu.Unlock()
	if c != nil {
		c()
	}
	if d != nil {
		<-d
	}
}

// showFrameAt renders a single frame at pos to the display image. Used for
// initial preview and after seek-while-paused.
func (p *videoPlayer) showFrameAt(pos float64) {
	frameSize := p.dispW * p.dispH * 4
	cmd := exec.Command("ffmpeg",
		"-loglevel", "error",
		"-ss", fmt.Sprintf("%.3f", pos),
		"-i", p.src,
		"-frames:v", "1",
		"-vf", fmt.Sprintf("scale=%d:%d", p.dispW, p.dispH),
		"-f", "rawvideo",
		"-pix_fmt", "rgba",
		"-an",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	defer func() { _ = cmd.Wait() }()
	buf := make([]byte, frameSize)
	if _, err := io.ReadFull(stdout, buf); err != nil {
		return
	}
	p.publishFrame(buf)
}

func (p *videoPlayer) runDecoder(ctx context.Context, fromSec float64, done chan struct{}) {
	defer close(done)

	frameSize := p.dispW * p.dispH * 4
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", "error",
		"-re",
		"-ss", fmt.Sprintf("%.3f", fromSec),
		"-i", p.src,
		"-vf", fmt.Sprintf("scale=%d:%d", p.dispW, p.dispH),
		"-f", "rawvideo",
		"-pix_fmt", "rgba",
		"-an",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	defer func() { _ = cmd.Wait() }()

	durLbl := formatTime(p.dur)
	pos := fromSec
	dt := 1.0 / p.fps
	buf := make([]byte, frameSize)
	lastUI := time.Now().Add(-time.Second)

	for {
		if _, err := io.ReadFull(stdout, buf); err != nil {
			break
		}
		p.publishFrame(buf)

		pos += dt
		if time.Since(lastUI) > 100*time.Millisecond {
			lastUI = time.Now()
			cur := pos
			p.mu.Lock()
			p.position = cur
			drag := p.dragging
			p.mu.Unlock()
			if !drag {
				fyne.Do(func() {
					p.progress.SetValue(cur)
					p.timeLbl.SetText(formatTime(cur) + " / " + durLbl)
				})
			}
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}

	// EOF — natural end of stream
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if p.dur > 0 {
		p.position = p.dur
	}
	p.playing = false
	p.mu.Unlock()
	fyne.Do(func() {
		if p.dur > 0 {
			p.progress.SetValue(p.dur)
			p.timeLbl.SetText(durLbl + " / " + durLbl)
		}
		p.playBtn.SetIcon(theme.MediaPlayIcon())
	})
}

// publishFrame copies the raw RGBA bytes into a fresh image and hands it to
// the canvas. Allocating per-frame avoids races with Fyne reading the pixel
// data on the render thread; at 720p/30fps this is ~110 MB/s of GC churn,
// which is well within budget for a preview player.
func (p *videoPlayer) publishFrame(buf []byte) {
	rgba := image.NewRGBA(image.Rect(0, 0, p.dispW, p.dispH))
	copy(rgba.Pix, buf)
	fyne.DoAndWait(func() {
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return
		}
		p.display.Resource = nil
		p.display.File = ""
		p.display.Image = rgba
		p.display.Refresh()
	})
}
