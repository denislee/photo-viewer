package webserver

import (
	_ "embed"
	"fmt"
	"io"
)

// The page CSS and scripts live in assets/ so they can be edited (and linted)
// as CSS/JS. They are still inlined into every page, so the viewer needs no
// extra requests and the CSP stays as it is.

//go:embed assets/style.css
var styleCSS string

//go:embed assets/grid.js
var gridJS string

//go:embed assets/viewer.js
var viewerJS string

// pageHeader opens every page: doctype, viewport, and the shared stylesheet.
var pageHeader = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>Photo Viewer</title>
<style>
` + styleCSS + `</style></head><body>`

// mobileTopBar is emitted at the top of every gallery's .content so the
// hamburger has a place to live. On desktop the bar is hidden via CSS.
const mobileTopBar = `<div class="topbar"><label for="navtoggle" class="hamburger" aria-label="Menu">☰</label><span class="brand">Photo Viewer</span></div>`

// gridScript handles infinite scroll plus keyboard navigation (j/k/h/l +
// arrows) over the cell grid. Cells are built via createElement so we never
// parse network strings as HTML.
var gridScript = "<script>\n" + gridJS + "</script>"

// writeViewerScript writes the viewer's inline <script>: the pvNav literal,
// then viewer.js streamed straight from the embedded string instead of being
// concatenated into a fresh ~4 KB string per /view/ request (W4-12).
//
// nextImg is the /display URL of the next photo ("" when the successor is a
// video or there is none).
func writeViewerScript(w io.Writer, prevHref, nextHref, backHref, nextImg string) {
	fmt.Fprintf(w, "<script>\nvar pvNav = {prev: %q, next: %q, back: %q, nextImg: %q};\n",
		prevHref, nextHref, backHref, nextImg)
	_, _ = io.WriteString(w, viewerJS)
	_, _ = io.WriteString(w, "</script>")
}
