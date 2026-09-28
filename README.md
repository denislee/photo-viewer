# photo-viewer

A Go + [Gio](https://gioui.org) photo and video browser for Wayland/X11 (Sway, Hyprland, etc.),
with a SQLite index, inline video playback through libmpv, and an optional web gallery.

- Two panes: a directory tree on the left and a recursive thumbnail grid on the right. Choosing
  a folder shows every photo and video in it **and in all of its subfolders**.
- Vim-style keyboard navigation (`h/j/k/l`), a full-screen viewer, favorites, soft delete to a
  trash you can restore from, multi-select with export, search, and sorting by name or video
  length.
- Videos play inside the viewer through libmpv. `o` opens the current file or selection in an
  external program: `mpv` when every item is a video, otherwise `xdg-open` for each file.
- The index is built on the first scan and then updated incrementally. "Rebuild index"
  regenerates it from scratch.
- Workflows:
  - **Import**: files everything from an Inbox (or an SD card, which can be auto-detected)
    into `Outbox/YYYY-MM-DD/`.
  - **Organize**: sorts files into dated folders.
  - **Duplicates**: finds duplicate files by content hash.
  - **Export favorites**: can re-compress the exported copies.
- **Web server** (toolbar → Web server): serves the indexed library to a browser, with
  thumbnails, a viewer, favorites, and video, using HLS for iOS/Safari. It binds to
  `127.0.0.1:8080` by default. Settings can change the port or bind to all interfaces, and an
  optional password turns on HTTP Basic auth with user `viewer`. The password is never written
  to disk.

## Supported formats

- **Photos**: jpg/jpeg, png, webp, gif, bmp, tif/tiff
- **RAW**: cr2, cr3, nef, nrw, arw, dng, raf, orf, rw2, pef, srw, x3f. The thumbnail comes from
  the embedded JPEG preview, falling back to an ffmpeg decode.
- **HEIC/HEIF/AVIF**: decoded with ffmpeg, falling back to `heif-convert`.
- **Videos**: mp4, mov, mkv, webm, avi, m4v, mts, m2ts, 3gp, wmv, flv, mpg/mpeg. Thumbnails come
  from ffmpeg, and playback is inline via libmpv.

## Dependencies

**Build time**: Go (see `go.mod`), cgo, `libmpv`, and Gio's Wayland/X11/EGL headers. On
Debian/Ubuntu:

```sh
sudo apt-get install pkg-config libmpv-dev \
  libwayland-dev libx11-dev libx11-xcb-dev libxkbcommon-dev libxkbcommon-x11-dev \
  libxcursor-dev libxfixes-dev libgles2-mesa-dev libegl1-mesa-dev libffi-dev libvulkan-dev
```

**Runtime**: `libmpv` is linked in and must be installed. The external tools below are all
optional. When a tool is missing, the feature that needs it is degraded (for example, a
placeholder icon instead of a thumbnail).

| Tool | Used for |
| --- | --- |
| `ffmpeg` | Video thumbnails, HEIC/AVIF and large-image decoding, web HLS, export re-compression |
| `ffprobe` | Video durations (sorting by length, web HLS) |
| `exiftool` | RAW previews, capture dates and metadata (import, organize, info panel, `/api/info`). **Required** by `pv-organize` |
| `heif-convert` (libheif) | HEIC fallback decoder |
| `zenity` | Directory pickers and warning dialogs |
| `udisksctl`, `lsblk` | SD-card detection and mounting for import |
| `mpv`, `xdg-open` | "Open externally" (`o`) |
| `pv-face-detect` + Python [`face_recognition`](https://github.com/ageitgey/face_recognition) | Face detection during `pv-scan` (see below) |

## Build & run

```sh
make build            # photo-viewer, pv-scan, pv-organize, pv-export-favorites, ./pv-face-detect
./photo-viewer -root /path/to/photos
```

If `-root` is omitted, the current working directory is used.

## Command-line tools

| Command | What it does |
| --- | --- |
| `pv-scan -root DIR [-no-faces] [-v]` | Headless scan: builds or updates the index and thumbnails (and faces, when `pv-face-detect` is available) without a display. Ctrl-C stops cleanly. |
| `pv-organize -src DIR -dst DIR [-dry-run]` | Moves media into `DST/YYYY/MM/DD/` by capture date (exiftool). Never overwrites: name collisions get `_1`, `_2`, …. |
| `pv-export-favorites -root DIR -dst DIR [-flatten] [-move] [-dry-run] [-max-long-edge N] [-jpeg-quality Q] [-video-crf C]` | Copies (or moves) every favorite out of the library. With `-max-long-edge`, images and videos are re-compressed; RAW files are always copied as-is. |
| `pv-face-detect` | Python helper (`scripts/pv-face-detect.py`, installed by `make build`) that `pv-scan` runs as a long-lived `--server` process. `pv-face-detect --check` smoke-tests the install. It finds `face_recognition` in the current Python or in a pipx venv. |

## Configuration

GUI preferences live in `$XDG_CONFIG_HOME/photo-viewer/config.json`
(`~/.config/photo-viewer/config.json` by default). Change them from Settings (`,`). They include
`inbox_dir` / `outbox_dir` for Import, `import_delete_source`, `sd_card_auto_detect`,
`group_by_year` (sidebar), `sort_mode`, `show_shortcut_hints`, `web_server_port`, and
`web_server_bind_all`. Import refuses an Inbox/Outbox pair where one is inside the other.

## Keyboard shortcuts

Turn on Settings → "Show keyboard-shortcut hints" to see the shortcuts for the current pane. The main
ones:

| Where | Keys |
| --- | --- |
| Grid | `h/j/k/l` navigate · `enter` open · `f` favorite · `d` delete · `v` selection mode · `o` open externally · `ctrl k` search · `ctrl +/-` zoom · `ctrl f/b` page · `ctrl i` import · `ctrl d` duplicates · `ctrl e` export favorites · `,` settings · `q` quit |
| Selection | `h/j/k/l` select · `enter` open · `e` export selected · `o` open externally · `v` leave |
| Viewer | `h/l` or `←/→` prev/next · `f` favorite · `d` delete · `space` play/pause · `[`/`]` seek 5 s · `m` mute · `o` open externally · `esc`/`q` close |
| Sidebar | `j/k` select folder · `enter` open · `l`/`→` focus grid · `ctrl f/b` page |

## Data on disk

```
<library-root>/.photo-viewer.db         # SQLite index (entries, favorites, hashes, faces)
<library-root>/.photo-viewer-trash/     # soft-deleted files, restorable
<drive-root>/.photo-viewer-cache/
├── thumbs/ab/abc123…ef.jpg             # 256 px grid thumbnails, name = SHA-1(absolute path)
└── display/ab/abc123…ef.jpg            # ≤2048 px web-viewer renditions (RAW/HEIC/TIFF/oversized)
```

When a location isn't writable, the data moves:

- The cache falls back to `~/.cache/photo-viewer/`.
- The index falls back to `index-<hash>.db` inside that cache directory.
- The trash falls back to `trash/` inside that cache directory.

## Development

| Target | Runs |
| --- | --- |
| `make test` | `go test ./...` |
| `make race` | the same under the race detector |
| `make test-video` | the libmpv Close/Render race tests against an ffmpeg-generated clip |
| `make lint` | `golangci-lint` with `.golangci.yml` (pinned, via `go run`) |
| `make vuln` | `govulncheck` (pinned, via `go run`) |
| `make check` | everything CI runs: fmt-check, vet, lint, race, test-video, vuln |

The libmpv race tests skip unless `PV_CROP_VIDEO` points at a video file, which is why
`make test-video` exists. `PV_CROP_VIDEO=/path/to/clip go test -run TestRenderCropAssertion
./internal/video/` also runs the (slow) SW-render crop repro against a clip of your choice.

`gioui.org` is replaced by a patched local copy in `third_party/gioui-singleline-fix/`. Its
upstream base, the patch, and how to check whether the fork can be dropped are recorded in
[`FORK.md`](third_party/gioui-singleline-fix/FORK.md).

`CLAUDE.md` and `OPTIMIZATION_*.md` are gitignored, local-only working notes.
