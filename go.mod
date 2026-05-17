module github.com/dns/photo-viewer

go 1.25.0

require (
	gioui.org v0.9.0
	github.com/mattn/go-sqlite3 v1.14.44
	github.com/rwcarlsen/goexif v0.0.0-20190401172101-9e8deecbddbd
	golang.org/x/image v0.39.0
)

require (
	gioui.org/shader v1.0.8 // indirect
	github.com/go-text/typesetting v0.3.3 // indirect
	golang.org/x/exp/shiny v0.0.0-20250408133849-7e4ce0ab07d0 // indirect
	golang.org/x/sys v0.33.0 // indirect
	golang.org/x/text v0.36.0 // indirect
)

// Local Gio fork patched to fix the math.MaxInt overflow in
// widget/text.go's SingleLine handling, which made every single-line
// editor wrap its glyphs onto a vertical column. See
// third_party/gioui-singleline-fix/widget/text.go around line 247.
replace gioui.org => ./third_party/gioui-singleline-fix
