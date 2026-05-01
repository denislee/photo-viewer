package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/dialog"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/ui"
)

func main() {
	var rootFlag string
	flag.StringVar(&rootFlag, "root", "", "library root (defaults to ~/Pictures)")
	flag.Parse()

	if rootFlag == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			rootFlag = filepath.Join(home, "Pictures")
		}
	}
	abs, err := filepath.Abs(rootFlag)
	if err != nil {
		log.Fatalf("resolve root: %v", err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		log.Fatalf("library root %q is not a directory: %v", abs, err)
	}

	a := app.NewWithID("com.github.dns.photoviewer")
	w := a.NewWindow("Photo Viewer — " + abs)
	w.Resize(fyne.NewSize(1200, 800))

	cacheDir, err := cache.CacheDir(abs)
	if err != nil {
		dialog.ShowError(err, w)
		w.ShowAndRun()
		return
	}
	dbPath := filepath.Join(abs, ".photo-viewer.db")
	idx, err := cache.Load(dbPath)
	if err != nil {
		log.Printf("load index: %v", err)
		idx, _ = cache.Load(dbPath)
	}
	store, err := cache.NewThumbStore(cacheDir)
	if err != nil {
		log.Fatalf("thumb store: %v", err)
	}

	ctrl := ui.NewController(w, abs, idx, store, cacheDir)
	w.SetContent(ctrl.Build())

	// Trigger an initial scan/refresh from the library root.
	go ctrl.SelectDir(abs)

	w.ShowAndRun()
}
