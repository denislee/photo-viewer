package ui

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDevices is a swappable lsblk stand-in that counts invocations.
type fakeDevices struct {
	mu    sync.Mutex
	devs  []removableDevice
	calls atomic.Int32
}

func (f *fakeDevices) list() ([]removableDevice, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]removableDevice(nil), f.devs...), nil
}

func (f *fakeDevices) set(paths ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devs = f.devs[:0]
	for _, p := range paths {
		f.devs = append(f.devs, removableDevice{Path: p})
	}
}

func newTestWatcher(f *fakeDevices, events func(<-chan struct{}) (<-chan struct{}, error), enabled *atomic.Bool) *SDCardWatcher {
	w := NewSDCardWatcher(nil)
	w.interval = 5 * time.Millisecond
	w.settle = 5 * time.Millisecond
	w.list = f.list
	w.events = events
	w.enabled = enabled.Load
	return w
}

// waitFor polls cond for up to a second.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestWatcherScansOnlyOnEvents is the D-01 contract: with an event source, the
// ticker never forks lsblk; a burst of uevents costs one scan, which queues the
// new device.
func TestWatcherScansOnlyOnEvents(t *testing.T) {
	f := &fakeDevices{}
	f.set("/dev/sda1")
	events := make(chan struct{}, 8)
	var enabled atomic.Bool
	enabled.Store(true)
	w := newTestWatcher(f, func(<-chan struct{}) (<-chan struct{}, error) { return events, nil }, &enabled)
	w.Start()
	defer w.Stop()

	time.Sleep(50 * time.Millisecond) // ~10 ticks
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("lsblk ran %d times with no events, want 1 (the startup seed)", n)
	}

	f.set("/dev/sda1", "/dev/sdb1")
	for range 3 {
		events <- struct{}{}
	}
	waitFor(t, "a prompt for the new card", func() bool { _, ok := w.Pending(); return ok })
	if d, _ := w.Pending(); d.Path != "/dev/sdb1" {
		t.Errorf("pending = %q, want /dev/sdb1", d.Path)
	}
	time.Sleep(30 * time.Millisecond)
	if n := f.calls.Load(); n != 2 {
		t.Errorf("lsblk ran %d times, want 2 (seed + one per settled burst)", n)
	}
}

// TestWatcherPollsWithoutEvents: when no event source can be opened the
// ticker scans, as before D-01.
func TestWatcherPollsWithoutEvents(t *testing.T) {
	f := &fakeDevices{}
	var enabled atomic.Bool
	enabled.Store(true)
	w := newTestWatcher(f, func(<-chan struct{}) (<-chan struct{}, error) {
		return nil, errors.New("no netlink")
	}, &enabled)
	w.Start()
	defer w.Stop()

	f.set("/dev/sdc1")
	waitFor(t, "a polled prompt", func() bool { _, ok := w.Pending(); return ok })
}

// TestWatcherFallsBackWhenEventsStop: a closed event channel (the socket
// died) switches the ticker to polling.
func TestWatcherFallsBackWhenEventsStop(t *testing.T) {
	f := &fakeDevices{}
	events := make(chan struct{})
	var enabled atomic.Bool
	enabled.Store(true)
	w := newTestWatcher(f, func(<-chan struct{}) (<-chan struct{}, error) { return events, nil }, &enabled)
	w.Start()
	defer w.Stop()

	close(events)
	f.set("/dev/sdd1")
	waitFor(t, "a polled prompt after the event source closed", func() bool { _, ok := w.Pending(); return ok })
}

// TestWatcherToggleReseeds: devices already attached when auto-detect is
// turned on don't prompt, and while it is off events cost nothing.
func TestWatcherToggleReseeds(t *testing.T) {
	f := &fakeDevices{}
	events := make(chan struct{}, 1)
	var enabled atomic.Bool
	w := newTestWatcher(f, func(<-chan struct{}) (<-chan struct{}, error) { return events, nil }, &enabled)
	w.Start()
	defer w.Stop()

	f.set("/dev/sde1") // plugged in while auto-detect is off
	events <- struct{}{}
	time.Sleep(30 * time.Millisecond)
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("lsblk ran %d times while disabled, want 1 (the startup seed)", n)
	}

	enabled.Store(true)
	waitFor(t, "the re-seed scan", func() bool { return f.calls.Load() == 2 })
	time.Sleep(20 * time.Millisecond)
	if d, ok := w.Pending(); ok {
		t.Fatalf("already-attached %s prompted after enabling", d.Path)
	}

	f.set("/dev/sde1", "/dev/sdf1")
	events <- struct{}{}
	waitFor(t, "a prompt for the card inserted after enabling", func() bool { _, ok := w.Pending(); return ok })
	if d, _ := w.Pending(); d.Path != "/dev/sdf1" {
		t.Errorf("pending = %q, want /dev/sdf1", d.Path)
	}
}

// udevMessage builds a udevd broadcast: the libudev header followed by props.
func udevMessage(props string) []byte {
	const hdr = 40
	msg := make([]byte, hdr+len(props))
	copy(msg, "libudev\x00")
	binary.BigEndian.PutUint32(msg[8:], 0xfeedcafe)
	binary.NativeEndian.PutUint32(msg[12:], hdr)
	binary.NativeEndian.PutUint32(msg[16:], hdr)
	binary.NativeEndian.PutUint32(msg[20:], uint32(len(props)))
	msg[hdr-1] = 0xff // non-zero bloom byte right before the properties
	copy(msg[hdr:], props)
	return msg
}

func TestIsBlockUevent(t *testing.T) {
	cases := []struct {
		name string
		msg  []byte
		want bool
	}{
		{"kernel block add", []byte("add@/devices/pci0000:00/usb1/1-1/block/sdb/sdb1\x00ACTION=add\x00DEVPATH=/devices/x/sdb1\x00SUBSYSTEM=block\x00DEVNAME=sdb1\x00DEVTYPE=partition\x00"), true},
		{"kernel media change", []byte("change@/devices/x/block/sdb\x00ACTION=change\x00SUBSYSTEM=block\x00DISK_MEDIA_CHANGE=1\x00"), true},
		{"kernel usb", []byte("add@/devices/x/usb1/1-1\x00ACTION=add\x00SUBSYSTEM=usb\x00DEVTYPE=usb_device\x00"), false},
		{"udev block first property", udevMessage("SUBSYSTEM=block\x00ACTION=add\x00ID_FS_TYPE=vfat\x00"), true},
		{"udev block", udevMessage("ACTION=remove\x00DEVNAME=/dev/sdb1\x00SUBSYSTEM=block\x00"), true},
		{"udev net", udevMessage("ACTION=add\x00SUBSYSTEM=net\x00"), false},
		{"lookalike value", []byte("add@/x\x00ACTION=add\x00SUBSYSTEM=blockx\x00"), false},
		{"truncated udev header", []byte("libudev\x00\xfe\xed"), false},
	}
	bogus := udevMessage("SUBSYSTEM=block\x00")
	binary.NativeEndian.PutUint32(bogus[20:], 1<<20)
	cases = append(cases, struct {
		name string
		msg  []byte
		want bool
	}{"udev properties out of range", bogus, false})

	for _, c := range cases {
		if got := isBlockUevent(c.msg); got != c.want {
			t.Errorf("%s: isBlockUevent = %v, want %v", c.name, got, c.want)
		}
	}
}
