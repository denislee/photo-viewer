package ui

import (
	"encoding/json"
	"image"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
)

func settingsGtx(ops *op.Ops, w, h int) layout.Context {
	return layout.Context{
		Ops:         ops,
		Now:         time.Now(),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(w, h)),
	}
}

// TestSettingsLayoutZeroValue: modals_test.go builds a zero SettingsView, so
// Show and Layout must work without NewSettingsView or a controller.
func TestSettingsLayoutZeroValue(t *testing.T) {
	resetConfigForTest(t)
	v := &SettingsView{}
	var ops op.Ops
	v.Layout(settingsGtx(&ops, 800, 600), NewTheme())
	v.Show()
	if !v.Open {
		t.Fatal("Show did not open the view")
	}
	ops.Reset()
	v.Layout(settingsGtx(&ops, 800, 600), NewTheme())
}

// TestSettingsLayoutWithController lays out every section with a real
// controller, in a window far smaller than the page.
func TestSettingsLayoutWithController(t *testing.T) {
	resetConfigForTest(t)
	root := t.TempDir()
	v := NewSettingsView()
	v.SetController(newTestController(t, root, t.TempDir()))
	v.Show()
	th := NewTheme()
	var ops op.Ops
	for _, size := range []image.Point{{1200, 2000}, {400, 300}} {
		ops.Reset()
		v.Layout(settingsGtx(&ops, size.X, size.Y), th)
	}
}

// TestSettingsShowLoadsConfig checks Show reloads every persisted control.
func TestSettingsShowLoadsConfig(t *testing.T) {
	resetConfigForTest(t)
	UpdateConfig(func(c *Config) {
		c.InboxDir = "/in"
		c.OutboxDir = "/out"
		c.SDCardAutoDetect = true
		c.ShowShortcutHints = true
		c.ImportDeleteSource = true
		c.FaceStrictness = StrictnessLoose
		c.ExportFlatten = true
		c.ExportRecompress = true
		c.ExportMaxLongEdge = 1280
	})
	v := &SettingsView{}
	v.Show()
	s := v.sectionsFor()
	if got := s.imp.inboxEditor.Text(); got != "/in" {
		t.Errorf("inbox = %q", got)
	}
	if got := s.imp.outboxEditor.Text(); got != "/out" {
		t.Errorf("outbox = %q", got)
	}
	if !s.imp.sdAutoDetect.Value || !s.imp.deleteSource.Value {
		t.Errorf("import toggles not loaded: sd=%v delete=%v", s.imp.sdAutoDetect.Value, s.imp.deleteSource.Value)
	}
	if !s.iface.showShortcuts.Value {
		t.Error("shortcut hints not loaded")
	}
	if s.faces.strictness.Value != StrictnessLoose {
		t.Errorf("strictness = %q", s.faces.strictness.Value)
	}
	if !s.export.flatten.Value || !s.export.recompress.Value || s.export.move.Value {
		t.Errorf("export toggles: flatten=%v recompress=%v move=%v", s.export.flatten.Value, s.export.recompress.Value, s.export.move.Value)
	}
	if got := s.export.maxEdge.Text(); got != "1280" {
		t.Errorf("max edge = %q", got)
	}
}

// TestSettingsShowResetsExportMove: "Move" is never sticky (D-3), not even
// within one session.
func TestSettingsShowResetsExportMove(t *testing.T) {
	resetConfigForTest(t)
	v := &SettingsView{}
	v.Show()
	v.sectionsFor().export.move.Value = true
	v.Show()
	if v.sectionsFor().export.move.Value {
		t.Error("Move survived a re-open")
	}
}

// TestSettingsApplyOlderLoses covers U5-08: two Applies race; the older one's
// validation finishes last but must not overwrite the newer config or status.
func TestSettingsApplyOlderLoses(t *testing.T) {
	resetConfigForTest(t)
	release := make(chan struct{})
	orig := checkImportDirs
	checkImportDirs = func(inbox, outbox string) string {
		if inbox == "/old" {
			<-release
		}
		return importDirsProblem(inbox, outbox)
	}
	t.Cleanup(func() { checkImportDirs = orig })

	redraw := make(chan struct{}, 16)
	v := &SettingsView{}
	v.SetInvalidate(func() { redraw <- struct{}{} })
	v.Show()
	imp := v.sectionsFor().imp
	waitStatus := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			if got := imp.statusText(); got == want {
				return
			}
			select {
			case <-redraw:
			case <-deadline:
				t.Fatalf("status = %q, want %q", imp.statusText(), want)
			}
		}
	}

	imp.inboxEditor.SetText("/old")
	imp.outboxEditor.SetText("/old-out")
	imp.apply()
	imp.inboxEditor.SetText("/new")
	imp.outboxEditor.SetText("/new-out")
	imp.apply()
	waitStatus("Saved")
	close(release)
	// Let the stale goroutine finish; it signals a redraw on return.
	time.Sleep(50 * time.Millisecond)
	if c := GetConfig(); c.InboxDir != "/new" || c.OutboxDir != "/new-out" {
		t.Fatalf("config = %q/%q, want /new and /new-out", c.InboxDir, c.OutboxDir)
	}
	if got := imp.statusText(); got != "Saved" {
		t.Fatalf("stale Apply overwrote status: %q", got)
	}
}

// TestSettingsApplyInvalidPair shows the validation error inside Import and
// leaves the config alone.
func TestSettingsApplyInvalidPair(t *testing.T) {
	resetConfigForTest(t)
	UpdateConfig(func(c *Config) { c.InboxDir, c.OutboxDir = "/a", "/b" })
	redraw := make(chan struct{}, 16)
	v := &SettingsView{}
	v.SetInvalidate(func() { redraw <- struct{}{} })
	v.Show()
	imp := v.sectionsFor().imp
	dir := t.TempDir()
	imp.inboxEditor.SetText(dir)
	imp.outboxEditor.SetText(dir + "/sub")
	imp.apply()
	deadline := time.After(5 * time.Second)
	for !strings.HasPrefix(imp.statusText(), "Not saved:") {
		select {
		case <-redraw:
		case <-deadline:
			t.Fatalf("status = %q", imp.statusText())
		}
	}
	if c := GetConfig(); c.InboxDir != "/a" || c.OutboxDir != "/b" {
		t.Fatalf("invalid pair was saved: %q/%q", c.InboxDir, c.OutboxDir)
	}
}

// TestSettingsApplyDoesNotTouchToggles: Apply saves only the paths (P2).
func TestSettingsApplyDoesNotTouchToggles(t *testing.T) {
	resetConfigForTest(t)
	redraw := make(chan struct{}, 16)
	v := &SettingsView{}
	v.SetInvalidate(func() { redraw <- struct{}{} })
	v.Show()
	imp := v.sectionsFor().imp
	imp.sdAutoDetect.Value = true // flipped without an Update: not persisted
	imp.inboxEditor.SetText("/x")
	imp.apply()
	deadline := time.After(5 * time.Second)
	for imp.statusText() != "Saved" {
		select {
		case <-redraw:
		case <-deadline:
			t.Fatalf("status = %q", imp.statusText())
		}
	}
	if c := GetConfig(); c.InboxDir != "/x" || c.SDCardAutoDetect {
		t.Fatalf("config = %+v", c)
	}
}

func TestParseMaxEdge(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"", 1920}, {"  ", 1920}, {"abc", 1920}, {"0", 1920}, {"-5", 1920},
		{"1280", 1280}, {" 640 ", 640}, {"1e3", 1920},
	} {
		if got := parseMaxEdge(c.in); got != c.want {
			t.Errorf("parseMaxEdge(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestSettingsDebugSection: PV_DEBUG_START_VIEW=settings:<id> scrolls to that
// section; unknown ids leave the page at the top.
func TestSettingsDebugSection(t *testing.T) {
	resetConfigForTest(t)
	v := &SettingsView{}
	v.ShowSection("faces")
	if !v.Open || v.list.Position.First != 2 {
		t.Fatalf("faces: open=%v first=%d", v.Open, v.list.Position.First)
	}
	v.ShowSection("nope")
	if v.list.Position.First != 0 {
		t.Fatalf("unknown id: first=%d", v.list.Position.First)
	}
}

// TestOpenWebServerFromSettings: the Settings link closes Settings and opens
// the web-server panel, leaving exactly one modal open (W8).
func TestOpenWebServerFromSettings(t *testing.T) {
	v := testAppViews()
	linkSettingsToWebServer(v.settings, func() { v.webView.Open = true })
	v.settings.Open = true
	v.settings.OnOpenWebServer()
	open := 0
	for i := range v.modals {
		if v.modals[i].open() {
			open++
		}
	}
	if open != 1 || v.topModal() == nil || v.topModal().name != "webServer" {
		t.Fatalf("open modals = %d, top = %v", open, v.topModal())
	}
}

// configFixture holds every config.json key (D-1). A new Config field must be
// added here, so a tag change can't slip through unnoticed.
const configFixture = `{
  "inbox_dir": "/in",
  "outbox_dir": "/out",
  "import_delete_source": true,
  "sd_card_auto_detect": true,
  "show_shortcut_hints": true,
  "group_by_year": true,
  "sidebar_width_dp": 240,
  "grid_cell_dp": 180,
  "sort_mode": "duration",
  "faces_disabled": true,
  "face_strictness": "strict",
  "export_flatten": true,
  "export_recompress": true,
  "export_max_long_edge": 1280,
  "web_server_port": 9090,
  "web_server_bind_all": true
}`

func TestConfigRoundTrip(t *testing.T) {
	var want map[string]any
	if err := json.Unmarshal([]byte(configFixture), &want); err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal([]byte(configFixture), &c); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		keys := func(m map[string]any) []string {
			var k []string
			for s := range m {
				k = append(k, s)
			}
			sort.Strings(k)
			return k
		}
		t.Fatalf("round trip changed config:\n got  %v\n want %v\n got keys  %v\n want keys %v", got, want, keys(got), keys(want))
	}
}
