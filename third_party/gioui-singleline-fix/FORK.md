# Local Gio fork

This directory is an unmodified copy of upstream Gio plus three patches. `go.mod`
points `gioui.org` here with a `replace` directive.

- **Base:** `gioui.org v0.9.0`, commit `3f4f8ba7c1fae349f97621f1f623ef47e4da3e25`
  (`https://git.sr.ht/~eliasnaur/gio`, tag `v0.9.0`). The upstream `.builds/` and
  `.gitattributes` were not copied over.
- **Patch:** [`singleline-fix.patch`](singleline-fix.patch), which touches only
  `widget/text.go`. With `SingleLine` set, upstream uses `maxWidth = math.MaxInt`. go-text's
  `LineWrapper` turns that into `fixed.I(maxWidth)`, which overflows `fixed.Int26_6` into a
  small negative width, so every single-line editor wraps each glyph onto its own line. The
  patch caps the width at `1 << 24`.
- **Patch:** [`texture-retention.patch`](texture-retention.patch), which touches only
  `gpu/caches.go`. Upstream's `textureCache.frame` releases any texture not used in the
  previous frame, so closing the full-screen viewer re-uploads every visible grid thumbnail in
  one frame, and scrolling back re-uploads rows that just left the screen. The patch keeps
  unused textures for up to 120 frames, and only while the unused ones total at most 128 MB of
  pixels, releasing the least recently used first. An idle texture whose source `*image.RGBA`
  was drawn again this frame under a new handle (a video frame buffer reused across frames) is
  released at once, so dead video frames don't fill that budget and evict grid thumbnails
  (G5-03). This is a performance patch, not a fix, so it can be dropped at any time; see G6 in
  `PLAN.md`.
- **Patch:** [`pointer-gestures.patch`](pointer-gestures.patch), which touches only `app/`.
  Upstream Gio ignores touchpad pinch gestures on Wayland. The patch binds
  `zwp_pointer_gestures_v1` (generated `app/wayland_pointer_gestures.{c,h}`, see the
  `go:generate` lines in `os_wayland.go`) and delivers each pinch step as an
  `app.PinchEvent` (`app/pinch.go`) from `Window.Event`, merging updates that queue up
  between two reads. The photo viewer uses it for pinch to zoom. This is a feature, not a
  fix: dropping the fork means dropping pinch to zoom unless upstream gains gesture support.
- **Known upstream issue:** `go test gioui.org/gpu` doesn't build at v0.9.0, because
  `clip_test.go` and `pack_test.go` call `testing.B.Loop` while the module declares go1.23.

## Checking the fork

The fork should differ from the base only by the patches. To confirm:

```sh
go mod download gioui.org@v0.9.0   # run outside this module, e.g. in a temp dir
diff -ru "$(go env GOMODCACHE)/gioui.org@v0.9.0" third_party/gioui-singleline-fix \
  -x .builds -x .gitattributes -x FORK.md -x singleline-fix.patch -x texture-retention.patch \
  -x pointer-gestures.patch
```

## Can the fork be dropped?

As of 2026-09-28, upstream `v0.10.2` still sets `maxWidth = math.MaxInt` in
`widget/text.go`. When a new Gio release comes out:

1. Check whether `widget/text.go` still sets `math.MaxInt`, or whether go-text now clamps
   the width.
2. If it has been fixed, remove the `replace` directive and bump `gioui.org` in `go.mod`.
   Check that single-line editors, such as the settings fields, render on one line, then
   delete this directory.
3. If it hasn't, rebase the fork: copy the new release in, apply
   `patch -p1 < singleline-fix.patch`, `patch -p1 < texture-retention.patch` and
   `patch -p1 < pointer-gestures.patch`, and update
   the base line above.

If the singleline fix lands upstream, the texture-retention patch alone doesn't justify
keeping the fork: drop it along with the fork. The pointer-gestures patch does, unless
upstream has added pinch support by then.
