package ui

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Netlink multicast groups of NETLINK_KOBJECT_UEVENT: raw kernel events, and
// the copies udevd re-broadcasts once it has processed a device (written the
// udev database lsblk reads FSTYPE/LABEL from, created the /dev node).
const (
	ueventGroupKernel = 1
	ueventGroupUdev   = 2
)

// blockEvents subscribes to kernel and udev uevents and sends on the returned
// channel (coalesced, never blocking) whenever one concerns a block device:
// a disk or partition appearing or disappearing, or a reader reporting a media
// change. Receiving uevents needs no privileges. The channel is closed if the
// socket fails for good (not on stop), so the caller can fall back to polling;
// stop closes the socket and ends the reader.
func blockEvents(stop <-chan struct{}) (<-chan struct{}, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return nil, err
	}
	sa := &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: ueventGroupKernel | ueventGroupUdev}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	// A non-blocking fd goes through Go's poller, so Close from the stop
	// goroutine unblocks a pending Read.
	f := os.NewFile(uintptr(fd), "uevent")

	ch := make(chan struct{}, 1)
	notify := func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	go func() {
		<-stop
		f.Close()
	}()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := f.Read(buf)
			switch {
			case err == nil:
				if isBlockUevent(buf[:n]) {
					notify()
				}
			case errors.Is(err, unix.ENOBUFS):
				// The receive queue overflowed and events were dropped;
				// one of them may have been ours, so rescan.
				notify()
			default:
				select {
				case <-stop:
				default:
					close(ch)
				}
				return
			}
		}
	}()
	return ch, nil
}

// libudevPrefix starts every message udevd broadcasts; kernel messages start
// with "ACTION@DEVPATH" instead.
var libudevPrefix = []byte("libudev\x00")

// isBlockUevent reports whether a uevent message carries SUBSYSTEM=block.
// Kernel messages are "action@devpath\0KEY=VALUE\0…"; udev messages have a
// binary header (prefix, magic, header size, properties offset and length,
// filter hashes) followed by the same NUL-separated KEY=VALUE list.
func isBlockUevent(msg []byte) bool {
	props := msg
	if bytes.HasPrefix(msg, libudevPrefix) {
		// prefix[8], magic, header_size, properties_off, properties_len
		const hdr = 8 + 4*4
		if len(msg) < hdr {
			return false
		}
		off := binary.NativeEndian.Uint32(msg[16:20])
		size := binary.NativeEndian.Uint32(msg[20:24])
		if uint64(off)+uint64(size) > uint64(len(msg)) {
			return false
		}
		props = msg[off : off+size]
	}
	for field := range bytes.SplitSeq(props, []byte{0}) {
		if string(field) == "SUBSYSTEM=block" {
			return true
		}
	}
	return false
}
