package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Config is the small persisted-prefs replacement for the Fyne build's
// fyne.Preferences. Stored at ~/.config/photo-viewer/config.json.
type Config struct {
	InboxDir           string `json:"inbox_dir"`
	OutboxDir          string `json:"outbox_dir"`
	ImportDeleteSource bool   `json:"import_delete_source"`
	// SDCardAutoDetect enables a background watcher that polls for newly-
	// attached removable devices and prompts the user to import from them.
	SDCardAutoDetect bool `json:"sd_card_auto_detect"`
	// GroupByYear toggles the sidebar treatment of YYYY-MM-DD subfolders:
	// when true they are bucketed under collapsible YYYY headers.
	GroupByYear bool `json:"group_by_year"`
	// SidebarWidthDp is the persisted sidebar (directory panel) width in dp.
	// Zero means "use the default proportional width" (22% of window).
	SidebarWidthDp int `json:"sidebar_width_dp"`
	// GridCellDp is the persisted thumbnail cell size in dp. Zero means
	// "use defaultCellDp". Clamped to [minCellDp, maxCellDp] on load.
	GridCellDp int `json:"grid_cell_dp"`
	// ShowShortcutHints toggles the bottom keyboard-shortcut helper strip.
	// Defaults to false (hidden) — power users can opt in from Settings.
	ShowShortcutHints bool `json:"show_shortcut_hints"`
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

// SaveConfig writes the supplied config to disk and updates the in-memory
// copy. Returns the disk-write error if any.
func SaveConfig(c Config) error {
	cfgMu.Lock()
	cfg = c
	data, err := json.MarshalIndent(cfg, "", "  ")
	cfgMu.Unlock()
	if err != nil {
		return err
	}
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}
