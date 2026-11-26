package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"gioui.org/app"
	"gioui.org/unit"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/ui"
)

func main() {
	var rootFlag string
	flag.StringVar(&rootFlag, "root", "", "library root (defaults to the current working directory)")
	flag.Parse()

	// An explicit --root keeps the cache and index inside that library; the
	// working-directory default uses this computer's user cache instead.
	inRoot := rootFlag != ""
	if rootFlag == "" {
		cwd, err := os.Getwd()
		if err != nil {
			log.Fatalf("get working directory: %v", err)
		}
		rootFlag = cwd
	}
	abs, err := filepath.Abs(rootFlag)
	if err != nil {
		log.Fatalf("resolve root: %v", err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		log.Fatalf("library root %q is not a directory: %v", abs, err)
	}

	cacheDir, dbPath, err := cache.Locate(abs, inRoot)
	if err != nil {
		log.Fatalf("cache dir: %v", err)
	}
	idx, err := cache.Load(dbPath)
	if err != nil {
		log.Fatalf("load index: %v", err)
	}
	store, err := cache.NewThumbStore(cacheDir)
	if err != nil {
		log.Fatalf("thumb store: %v", err)
	}

	// The config must be loaded before NewController, which reads the saved
	// sort mode from it (U6-B1).
	ui.LoadConfig()
	ctrl := ui.NewController(abs, idx, store, cacheDir)
	ctrl.SetIndexPath(dbPath)

	go func() {
		w := new(app.Window)
		w.Option(
			app.Title("Photo Viewer"),
			app.Size(unit.Dp(1200), unit.Dp(800)),
		)
		if err := ui.Run(w, ctrl); err != nil {
			log.Fatal(err)
		}
		// Close runs PRAGMA optimize. Background goroutines (a scan, the
		// warm-up, the web server) may still be mid-query; at worst one of
		// them logs a "database is closed" error before the process exits.
		if err := idx.Close(); err != nil {
			log.Printf("close index: %v", err)
		}
		os.Exit(0)
	}()
	app.Main()
}
