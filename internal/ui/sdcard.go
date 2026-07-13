package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// blockDevice mirrors the relevant subset of `lsblk -J` output. lsblk reports
// rm/hotplug as booleans on modern util-linux but as integers on older builds,
// so we accept both via flexibleBool.
type blockDevice struct {
	Name       string        `json:"name"`
	Path       string        `json:"path"`
	Size       string        `json:"size"`
	Type       string        `json:"type"`
	FSType     string        `json:"fstype"`
	Mountpoint string        `json:"mountpoint"`
	Label      string        `json:"label"`
	Vendor     string        `json:"vendor"`
	Model      string        `json:"model"`
	RM         flexibleBool  `json:"rm"`
	Hotplug    flexibleBool  `json:"hotplug"`
	Tran       string        `json:"tran"`
	Children   []blockDevice `json:"children"`
}

type flexibleBool bool

func (f *flexibleBool) UnmarshalJSON(b []byte) error {
	switch strings.TrimSpace(string(b)) {
	case "true", "1", "\"1\"", "\"true\"":
		*f = true
	default:
		*f = false
	}
	return nil
}

// removableDevice is a flattened, mountable entry presented to the user.
type removableDevice struct {
	Path       string // /dev/sdb1
	Size       string
	FSType     string
	Label      string
	Vendor     string
	Model      string
	Mountpoint string // "" when not mounted
	Tran       string
}

func (d removableDevice) Display() string {
	parts := []string{d.Path}
	if d.Size != "" {
		parts = append(parts, d.Size)
	}
	if d.Label != "" {
		parts = append(parts, fmt.Sprintf("%q", d.Label))
	}
	if d.FSType != "" {
		parts = append(parts, d.FSType)
	}
	desc := strings.TrimSpace(strings.Join([]string{d.Vendor, d.Model}, " "))
	if desc != "" {
		parts = append(parts, "— "+desc)
	}
	if d.Mountpoint != "" {
		parts = append(parts, "(mounted at "+d.Mountpoint+")")
	}
	return strings.Join(parts, "  ")
}

// listRemovableDevices returns every partition (or whole-disk filesystem) that
// looks removable: rm=1, hotplug=1, or transport in {usb, mmc, sd, usbas}.
func listRemovableDevices() ([]removableDevice, error) {
	if _, err := exec.LookPath("lsblk"); err != nil {
		return nil, fmt.Errorf("lsblk not found: %w", err)
	}
	cmd := exec.Command("lsblk", "-J", "-o", "NAME,PATH,SIZE,TYPE,FSTYPE,MOUNTPOINT,LABEL,VENDOR,MODEL,RM,HOTPLUG,TRAN")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	var parsed struct {
		Blockdevices []blockDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("parse lsblk output: %w", err)
	}
	var devices []removableDevice
	for _, top := range parsed.Blockdevices {
		removable := bool(top.RM) || bool(top.Hotplug) || isRemovableTransport(top.Tran)
		if !removable {
			continue
		}
		collectMountable(top, top, &devices)
	}
	return devices, nil
}

func isRemovableTransport(t string) bool {
	switch strings.ToLower(t) {
	case "usb", "usbas", "mmc", "sd":
		return true
	}
	return false
}

func collectMountable(parent, dev blockDevice, out *[]removableDevice) {
	if strings.EqualFold(dev.Mountpoint, "[SWAP]") {
		return
	}
	hasMountableChildren := false
	for _, ch := range dev.Children {
		if ch.FSType != "" || len(ch.Children) > 0 {
			hasMountableChildren = true
			break
		}
	}
	if !hasMountableChildren && dev.FSType != "" && dev.Type != "crypt" && !strings.EqualFold(dev.FSType, "swap") {
		*out = append(*out, removableDevice{
			Path:       devicePath(dev),
			Size:       dev.Size,
			FSType:     dev.FSType,
			Label:      dev.Label,
			Vendor:     strings.TrimSpace(coalesce(dev.Vendor, parent.Vendor)),
			Model:      strings.TrimSpace(coalesce(dev.Model, parent.Model)),
			Mountpoint: dev.Mountpoint,
			Tran:       coalesce(dev.Tran, parent.Tran),
		})
	}
	for _, ch := range dev.Children {
		collectMountable(parent, ch, out)
	}
}

func devicePath(d blockDevice) string {
	if d.Path != "" {
		return d.Path
	}
	return "/dev/" + d.Name
}

func coalesce(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// mountDevice mounts dev via udisksctl (uses PolicyKit, no su) and returns the
// mount path.
func mountDevice(ctx context.Context, devPath string) (string, error) {
	if _, err := exec.LookPath("udisksctl"); err != nil {
		return "", fmt.Errorf("udisksctl not available — install udisks2 to mount removable media without root")
	}
	cmd := exec.CommandContext(ctx, "udisksctl", "mount", "-b", devPath, "--no-user-interaction")
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil {
		return "", fmt.Errorf("udisksctl mount: %s", strings.TrimSpace(text))
	}
	// "Mounted /dev/sdb1 at /run/media/user/SDCARD" (with optional trailing dot).
	if _, after, ok := strings.Cut(text, " at "); ok {
		mount := strings.TrimSpace(after)
		mount = strings.TrimRight(mount, ".\n\r ")
		if mount != "" {
			return mount, nil
		}
	}
	return "", fmt.Errorf("could not parse udisksctl output: %s", strings.TrimSpace(text))
}

func unmountDevice(ctx context.Context, devPath string) error {
	if _, err := exec.LookPath("udisksctl"); err != nil {
		return fmt.Errorf("udisksctl not available")
	}
	cmd := exec.CommandContext(ctx, "udisksctl", "unmount", "-b", devPath, "--no-user-interaction")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("udisksctl unmount: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
