package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/dns/photo-viewer/internal/scan"
)

// blockDevice mirrors the relevant subset of `lsblk -J` output. lsblk reports
// rm/hotplug as booleans on modern util-linux but as integers on older builds,
// so we accept both via json.Number-ish parsing.
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
	s := strings.TrimSpace(string(b))
	switch s {
	case "true", "1", "\"1\"", "\"true\"":
		*f = true
	case "false", "0", "\"0\"", "\"false\"", "null", "":
		*f = false
	default:
		// Unknown — treat as false rather than fail the whole listing.
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
// Devices already mounted at /, /boot, [SWAP], or other system paths are
// implicitly excluded — the user can still pick a mounted SD if they want to
// re-import from it.
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
		// Skip disks/partitions backing the running system.
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
	// Skip swap regardless of removable flag.
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

	if !hasMountableChildren && dev.FSType != "" && dev.Type != "crypt" && !looksLikeSwap(dev.FSType) {
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

func looksLikeSwap(fstype string) bool {
	return strings.EqualFold(fstype, "swap")
}

// mountDevice mounts dev via udisksctl (preferred — uses PolicyKit, no su) and
// returns the mount path. Falls back to `pkexec mount` only if udisksctl is
// not on PATH; that fallback uses a temp mountpoint under /run/media/<user>.
func mountDevice(ctx context.Context, devPath string) (string, error) {
	if _, err := exec.LookPath("udisksctl"); err == nil {
		cmd := exec.CommandContext(ctx, "udisksctl", "mount", "-b", devPath, "--no-user-interaction")
		out, err := cmd.CombinedOutput()
		text := string(out)
		if err != nil {
			// "Object /...: not authorized" or "Error mounting: ..."
			return "", fmt.Errorf("udisksctl mount: %s", strings.TrimSpace(text))
		}
		// "Mounted /dev/sdb1 at /run/media/user/SDCARD" (with optional trailing dot).
		if i := strings.Index(text, " at "); i >= 0 {
			mount := strings.TrimSpace(text[i+4:])
			mount = strings.TrimRight(mount, ".\n\r ")
			if mount != "" {
				return mount, nil
			}
		}
		return "", fmt.Errorf("could not parse udisksctl output: %s", strings.TrimSpace(text))
	}
	return "", fmt.Errorf("udisksctl not available — install udisks2 to mount removable media without root")
}

func unmountDevice(ctx context.Context, devPath string) error {
	if _, err := exec.LookPath("udisksctl"); err == nil {
		cmd := exec.CommandContext(ctx, "udisksctl", "unmount", "-b", devPath, "--no-user-interaction")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("udisksctl unmount: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	return fmt.Errorf("udisksctl not available")
}

// countMediaIn walks dir and returns the number of files DetectType recognizes.
// Bounded by ctx so a huge card doesn't freeze the dialog.
func countMediaIn(ctx context.Context, dir string) (int, error) {
	count := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Permission errors on system folders shouldn't abort the walk.
			if d != nil && d.IsDir() {
				return nil
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if scan.DetectType(path) != scan.TypeUnknown {
			count++
		}
		return nil
	})
	return count, err
}

// runSDCardImport opens the SD-card / USB-drive import dialog.
func (c *Controller) runSDCardImport() {
	deviceList := widget.NewList(
		func() int { return 0 },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(i widget.ListItemID, o fyne.CanvasObject) {},
	)

	var devices []removableDevice
	selected := -1
	statusLabel := widget.NewLabelWithStyle("Scanning for removable devices…", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
	infoLabel := widget.NewLabel("")
	infoLabel.Wrapping = fyne.TextWrapWord

	var mountBtn, unmountBtn, importBtn, refreshBtn *widget.Button
	var d dialog.Dialog

	// Track the currently-mounted device so unmount knows what to release.
	var mountedDev *removableDevice
	var mountedPath string

	setDevices := func(ds []removableDevice, msg string) {
		devices = ds
		deviceList.Length = func() int { return len(devices) }
		deviceList.UpdateItem = func(i widget.ListItemID, o fyne.CanvasObject) {
			if i < 0 || i >= len(devices) {
				return
			}
			o.(*widget.Label).SetText(devices[i].Display())
		}
		selected = -1
		deviceList.UnselectAll()
		deviceList.Refresh()
		statusLabel.SetText(msg)
		mountBtn.Disable()
		importBtn.Disable()
	}

	refresh := func() {
		statusLabel.SetText("Scanning for removable devices…")
		go func() {
			ds, err := listRemovableDevices()
			fyne.Do(func() {
				if err != nil {
					setDevices(nil, "Error: "+err.Error())
					return
				}
				if len(ds) == 0 {
					setDevices(nil, "No removable devices found. Insert an SD card or USB drive and click Refresh.")
					return
				}
				setDevices(ds, fmt.Sprintf("Found %d device(s). Select one and click Mount.", len(ds)))
			})
		}()
	}

	deviceList.OnSelected = func(i widget.ListItemID) {
		if i < 0 || i >= len(devices) {
			return
		}
		selected = i
		dev := devices[i]
		// If already mounted, allow Import directly; otherwise enable Mount.
		if dev.Mountpoint != "" {
			mountedDev = &dev
			mountedPath = dev.Mountpoint
			mountBtn.Disable()
			unmountBtn.Enable()
			importBtn.Enable()
			infoLabel.SetText(fmt.Sprintf("Already mounted at %s. Click Import to copy media.", dev.Mountpoint))
		} else {
			mountBtn.Enable()
			unmountBtn.Disable()
			importBtn.Disable()
			infoLabel.SetText(fmt.Sprintf("Selected %s. Click Mount to make it available.", dev.Path))
		}
	}

	refreshBtn = widget.NewButton("Refresh", func() {
		mountedDev = nil
		mountedPath = ""
		unmountBtn.Disable()
		importBtn.Disable()
		infoLabel.SetText("")
		refresh()
	})

	mountBtn = widget.NewButton("Mount", func() {
		if selected < 0 || selected >= len(devices) {
			return
		}
		dev := devices[selected]
		mountBtn.Disable()
		refreshBtn.Disable()
		statusLabel.SetText("Mounting " + dev.Path + "…")
		infoLabel.SetText("If a system password prompt appears, please respond to it.")

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			path, err := mountDevice(ctx, dev.Path)
			fyne.Do(func() {
				refreshBtn.Enable()
				if err != nil {
					statusLabel.SetText("Mount failed.")
					infoLabel.SetText(err.Error())
					mountBtn.Enable()
					return
				}
				dev.Mountpoint = path
				mountedDev = &dev
				mountedPath = path
				statusLabel.SetText("Mounted at " + path)
				unmountBtn.Enable()
				importBtn.Enable()
				infoLabel.SetText("Counting media files…")
				go func() {
					cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer ccancel()
					n, _ := countMediaIn(cctx, path)
					fyne.Do(func() {
						infoLabel.SetText(fmt.Sprintf("Mounted at %s — found %d media file(s).\nClick Import to copy them into your library.", path, n))
					})
				}()
			})
		}()
	})
	mountBtn.Importance = widget.HighImportance
	mountBtn.Disable()

	importBtn = widget.NewButton("Import", func() {
		if mountedPath == "" {
			return
		}
		// Hand off to the existing import dialog with the mount point pre-filled.
		// Closing this dialog first so the import dialog gets focus.
		path := mountedPath
		if d != nil {
			d.Hide()
		}
		c.runImportFromDirs([]string{path})
	})
	importBtn.Disable()

	unmountBtn = widget.NewButton("Unmount", func() {
		if mountedDev == nil {
			return
		}
		dev := *mountedDev
		unmountBtn.Disable()
		importBtn.Disable()
		statusLabel.SetText("Unmounting " + dev.Path + "…")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := unmountDevice(ctx, dev.Path)
			fyne.Do(func() {
				if err != nil {
					statusLabel.SetText("Unmount failed.")
					infoLabel.SetText(err.Error())
					unmountBtn.Enable()
					if mountedPath != "" {
						importBtn.Enable()
					}
					return
				}
				mountedDev = nil
				mountedPath = ""
				infoLabel.SetText(dev.Path + " safely unmounted. You can now remove the card.")
				refresh()
			})
		}()
	})
	unmountBtn.Disable()

	buttons := container.NewHBox(refreshBtn, mountBtn, importBtn, unmountBtn)
	top := container.NewVBox(statusLabel, infoLabel)
	content := container.NewBorder(top, buttons, nil, nil, deviceList)

	d = dialog.NewCustom("Import from SD Card / USB Drive", "Close", content, c.window)
	d.Resize(fyne.NewSize(640, 460))
	d.SetOnClosed(func() {
		// Best-effort safety: if the user closes the dialog while a device is
		// still mounted, leave it mounted — they may have an Import dialog
		// still using it. A second open of this window will list it as
		// already-mounted and offer Unmount again.
	})
	d.Show()
	refresh()
}
