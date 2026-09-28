# Local Gio fork

This directory is an unmodified copy of upstream Gio plus one patch. `go.mod`
points `gioui.org` here with a `replace` directive.

- **Base:** `gioui.org v0.9.0`, commit `3f4f8ba7c1fae349f97621f1f623ef47e4da3e25`
  (`https://git.sr.ht/~eliasnaur/gio`, tag `v0.9.0`). The upstream `.builds/` and
  `.gitattributes` were not copied over.
- **Patch:** [`singleline-fix.patch`](singleline-fix.patch), which touches only
  `widget/text.go`. With `SingleLine` set, upstream uses `maxWidth = math.MaxInt`. go-text's
  `LineWrapper` turns that into `fixed.I(maxWidth)`, which overflows `fixed.Int26_6` into a
  small negative width, so every single-line editor wraps each glyph onto its own line. The
  patch caps the width at `1 << 24`.

## Checking the fork

The fork should differ from the base only by the patch. To confirm:

```sh
go mod download gioui.org@v0.9.0   # run outside this module, e.g. in a temp dir
diff -ru "$(go env GOMODCACHE)/gioui.org@v0.9.0" third_party/gioui-singleline-fix \
  -x .builds -x .gitattributes -x FORK.md -x singleline-fix.patch
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
   `patch -p1 < singleline-fix.patch`, and update the base line above.
