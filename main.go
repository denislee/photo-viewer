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

	cacheDir, err := cache.CacheDir(abs)
	if err != nil {
		log.Fatalf("cache dir: %v", err)
	}
	dbPath := cache.IndexPath(abs, cacheDir)
	idx, err := cache.Load(dbPath)
	if err != nil {
		log.Fatalf("load index: %v", err)
	}
	store, err := cache.NewThumbStore(cacheDir)
	if err != nil {
		log.Fatalf("thumb store: %v", err)
	}

	ctrl := ui.NewController(abs, idx, store, cacheDir)
	go ctrl.SelectDir(abs)

	go func() {
		w := new(app.Window)
		w.Option(
			app.Title("Photo Viewer"),
			app.Size(unit.Dp(1200), unit.Dp(800)),
		)
		if err := ui.Run(w, ctrl); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}()
	app.Main()
}
