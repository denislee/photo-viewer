# photo-viewer

A Go + Fyne photo and video browser for Wayland (Sway, Hyprland, etc.).

- Two-pane UI: directory tree on the left, recursive thumbnail grid on the right.
- Selecting a folder shows thumbnails for every photo and video inside it **and all subfolders**.
- On-disk cache (`.photo-viewer-cache/` at the photo drive's mount point) for fast re-opens.
- Cache builds itself on first scan and updates incrementally on subsequent scans.
- A "Rebuild index" button wipes and regenerates the cache from scratch.

## Supported formats

- **Photos**: jpg, png, webp, gif, bmp, tif/tiff
- **RAW**: cr2, cr3, nef, arw, dng, raf, orf, rw2 (via embedded JPEG preview)
- **HEIC/HEIF**: iPhone photos
- **Videos**: mp4, mov, mkv, webm, avi, m4v (thumbnails only — opens in system player on click)

## Runtime dependencies

| Tool | Used for | Required for |
| --- | --- | --- |
| `ffmpeg` | Video frame extraction | Video thumbnails |
| `exiftool` | RAW preview extraction | RAW thumbnails |
| `heif-convert` (libheif) | HEIC → JPEG | HEIC/HEIF thumbnails |

Plain photo formats need no external tools. Missing tools degrade gracefully —
unsupported files just show a placeholder icon.

## Build & run

```sh
go build -o photo-viewer .
./photo-viewer -root /path/to/photos
```

If `-root` is not given, defaults to `~/Pictures`.

## Cache layout

```
<drive-root>/.photo-viewer-cache/
├── index.json           # all media entries with size + mtime + thumb id
└── thumbs/
    └── ab/abc123…ef.jpg # 256-pixel JPEG, name = SHA-1(absolute path)
```

If the drive root is not writable, the cache falls back to `~/.cache/photo-viewer/`.
