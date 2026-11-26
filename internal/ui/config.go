package ui

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dns/photo-viewer/internal/fsutil"
)

// Config holds the persisted user preferences, loaded from and saved to
// ~/.config/photo-viewer/config.json.
//
// Fields are grouped by the Settings section that owns them. The file stays
// flat: the JSON tags are the on-disk format and must not change (D-1 of the
// settings reorganization; TestConfigRoundTrip guards them). A new field must
// also be added to that test's fixture.
type Config struct {
	// --- Import (settings_import.go; ImportDeleteSource is shared with the
	// import dialog, which re-reads it every time it opens) ---

	InboxDir           string `json:"inbox_dir"`
	OutboxDir          string `json:"outbox_dir"`
	ImportDeleteSource bool   `json:"import_delete_source"`
	// SDCardAutoDetect enables a background watcher that notices newly-
	// attached removable devices (block-device uevents on Linux, lsblk polling
	// elsewhere) and prompts the user to import from them.
	SDCardAutoDetect bool `json:"sd_card_auto_detect"`

	// --- Interface, set in Settings (settings_interface.go) ---

	// ShowShortcutHints toggles the bottom keyboard-shortcut helper strip.
	// Defaults to false (hidden) — power users can opt in from Settings.
	ShowShortcutHints bool `json:"show_shortcut_hints"`

	// --- Interface, remembered from the toolbar / window (not in Settings) ---

	// GroupByYear toggles the sidebar treatment of YYYY-MM-DD subfolders:
	// when true they are bucketed under collapsible YYYY headers.
	GroupByYear bool `json:"group_by_year"`
	// SidebarWidthDp is the persisted sidebar (directory panel) width in dp.
	// Zero means "use the default proportional width" (22% of window).
	SidebarWidthDp int `json:"sidebar_width_dp"`
	// GridCellDp is the persisted thumbnail cell size in dp. Zero means
	// "use defaultCellDp". Clamped to [minCellDp, maxCellDp] on load.
	GridCellDp int `json:"grid_cell_dp"`
	// SortMode controls the grid ordering. Values: "name" (default — path
	// ascending) or "duration" (video length descending, with non-videos
	// pushed to the end ordered by path).
	SortMode string `json:"sort_mode"`

	// --- Face recognition (settings_faces.go) ---

	// FacesDisabled turns face recognition off. Stored inverted so the
	// default (absent key) is on, as the plan asks when the helper exists.
	FacesDisabled bool `json:"faces_disabled"`
	// FaceStrictness is the grouping preset: "strict" (0.50), "normal"
	// (0.60, default) or "loose" (0.65) Euclidean. Changing it regroups.
	FaceStrictness string `json:"face_strictness"`

	// --- Export favorites (settings_export.go). "Move" is deliberately
	// never persisted: a sticky destructive option is a surprise (D-3). ---

	ExportFlatten    bool `json:"export_flatten"`
	ExportRecompress bool `json:"export_recompress"`
	// ExportMaxLongEdge is the recompress limit in px; 0 means the default.
	ExportMaxLongEdge int `json:"export_max_long_edge"`

	// --- Web server (webserver_view.go; linked from Settings) ---

	// WebServerPort is the TCP port used by the optional HTTP server that
	// serves the indexed library to a browser. Zero means "use the default"
	// (8080). The password is intentionally not persisted to disk to avoid
	// stashing credentials in a plaintext config file.
	WebServerPort int `json:"web_server_port"`
	// WebServerBindAll, when true, binds the HTTP server to 0.0.0.0 so other
	// hosts on the network can reach it. The default (false) binds to
	// 127.0.0.1 only.
	WebServerBindAll bool `json:"web_server_bind_all"`
}

var (
	cfgMu sync.Mutex
	cfg   Config
)

func configPath() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "photo-viewer", "config.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "photo-viewer", "config.json")
}

// LoadConfig populates the in-memory config from disk on startup.
func LoadConfig() {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	data, err := os.ReadFile(configPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &cfg)
}

// GetConfig returns a copy of the current config.
func GetConfig() Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg
}

// importDirsProblem reports why an Inbox/Outbox pair is unsafe to import
// with, or "" when it's fine (an unset dir is left to the callers' own "not
// configured" message). Import walks the whole Inbox and files every media
// file under Outbox/YYYY-MM-DD, so an Inbox that is — or contains — the
// Outbox would re-file the library into itself (U-17). An Inbox inside the
// Outbox is fine: source and destination paths never coincide.
func importDirsProblem(inbox, outbox string) string {
	if inbox == "" || outbox == "" {
		return ""
	}
	in, out := resolveDir(inbox), resolveDir(outbox)
	if in == out {
		return "Inbox and Outbox must be different directories."
	}
	if rel, err := filepath.Rel(in, out); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "Outbox must not be inside the Inbox."
	}
	return ""
}

// resolveDir canonicalises p for comparison: absolute, symlinks resolved when
// the path exists, cleaned.
func resolveDir(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p)
}

// UpdateConfig applies mutate to the in-memory config under cfgMu and
// schedules an asynchronous save. It never touches the disk itself, so it is
// safe to call from Layout and key handlers on the frame goroutine (U4-02).
//
// Each call mutates the live config rather than storing a caller-held copy,
// so concurrent updates of different fields (the zoom timer goroutine and a
// frame-goroutine toggle, say) can no longer overwrite each other. Saves are
// coalesced by a single writer goroutine that always persists the newest
// snapshot, so the file can't end up older than memory either.
func UpdateConfig(mutate func(*Config)) {
	cfgMu.Lock()
	mutate(&cfg)
	cfgGen++
	cfgMu.Unlock()
	cfgWriterOnce.Do(func() { go configWriter() })
	select {
	case cfgKick <- struct{}{}:
	default: // a save is already pending; it will pick up this change too
	}
}

var (
	// cfgGen counts UpdateConfig mutations (guarded by cfgMu); cfgSavedGen is
	// the generation last persisted (guarded by cfgWriteMu).
	cfgGen        uint64
	cfgSavedGen   uint64
	cfgWriteMu    sync.Mutex // serialises persistConfig (writer vs FlushConfig)
	cfgKick       = make(chan struct{}, 1)
	cfgWriterOnce sync.Once

	// writeConfigFile is the disk write, indirected so tests can stall it or
	// make it fail.
	writeConfigFile = func(p string, data []byte) error {
		return fsutil.WriteFileAtomic(p, data, 0o644)
	}
)

// configWriter is the single background config writer. A kick that arrives
// while a write is in flight leaves one pending token, so a burst of updates
// collapses into at most one extra write of the latest snapshot.
func configWriter() {
	for range cfgKick {
		if err := persistConfig(); err != nil {
			log.Printf("config: save failed: %v", err)
		}
	}
}

// persistConfig writes the current config if it changed since the last
// successful save. Writes are serialised by cfgWriteMu and each marshals the
// snapshot taken after acquiring it, so the file always ends up holding the
// newest generation.
func persistConfig() error {
	cfgWriteMu.Lock()
	defer cfgWriteMu.Unlock()
	cfgMu.Lock()
	gen, c := cfgGen, cfg
	cfgMu.Unlock()
	if gen == cfgSavedGen {
		return nil
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := writeConfigFile(configPath(), data); err != nil {
		return err
	}
	cfgSavedGen = gen
	return nil
}

// FlushConfig persists any pending config change, waiting at most timeout.
// Called on window close (next to FlushFavorites) so a toggle flipped right
// before quitting isn't lost. Returns the write error, or an error when the
// timeout expires first (e.g. a hung disk wedging the writer).
func FlushConfig(timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- persistConfig() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("config: flush timed out")
	}
}
