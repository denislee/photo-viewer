// Command pv-export-favorites copies every file flagged as a favorite in the
// library index into a target directory. Paths from the library root are
// preserved under dst (so files do not collide and the source layout is
// recognisable in the export).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/export"
)

func main() {
	root := flag.String("root", "", "library root (the same path used when scanning)")
	dst := flag.String("dst", "", "destination directory for exported favorites")
	flatten := flag.Bool("flatten", false, "copy all files into dst directly instead of preserving the relative path under root")
	move := flag.Bool("move", false, "move files instead of copying")
	dryRun := flag.Bool("dry-run", false, "list actions without touching the filesystem")
	maxEdge := flag.Int("max-long-edge", 0, "if > 0, recompress images/videos so their longest edge does not exceed this many pixels (RAW always copied as-is)")
	jpegQ := flag.Int("jpeg-quality", 82, "JPEG quality used when recompressing images (1-100; ignored when -max-long-edge is 0)")
	videoCRF := flag.Int("video-crf", 26, "libx264 constant rate factor used when recompressing videos (lower = better quality; ignored when -max-long-edge is 0)")
	flag.Parse()

	if *root == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "Usage: pv-export-favorites -root <library> -dst <target> [-flatten] [-move] [-dry-run] [-max-long-edge px] [-jpeg-quality q] [-video-crf n]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		log.Fatalf("resolve root: %v", err)
	}
	cacheDir, err := cache.CacheDir(absRoot)
	if err != nil {
		log.Fatalf("cache dir: %v", err)
	}
	dbPath := cache.IndexPath(absRoot, cacheDir)
	if _, err := os.Stat(dbPath); err != nil {
		log.Fatalf("index not found at %s: %v\n(run the GUI or pv-scan against the library first)", dbPath, err)
	}
	idx, err := cache.Load(dbPath)
	if err != nil {
		log.Fatalf("open index: %v", err)
	}

	progress := func(step, total int, action, msg string) {
		prefix := action
		if *dryRun && (action == "COPY" || action == "MOVE") {
			prefix = "DRY-" + action
		}
		fmt.Printf("[%s] (%d/%d) %s\n", prefix, step, total, msg)
	}

	res, err := export.Favorites(context.Background(), idx, export.Options{
		Root:        absRoot,
		Dst:         *dst,
		Flatten:     *flatten,
		Move:        *move,
		DryRun:      *dryRun,
		MaxLongEdge: *maxEdge,
		JpegQuality: *jpegQ,
		VideoCRF:    *videoCRF,
	}, progress)
	if err != nil {
		log.Fatalf("export: %v", err)
	}

	verb := "Copied"
	switch {
	case *move:
		verb = "Moved"
	case *maxEdge > 0:
		verb = fmt.Sprintf("Exported (recompressed to ≤%dpx)", *maxEdge)
	}
	if *dryRun {
		verb = "Would " + strings.ToLower(verb)
	}
	fmt.Printf("\n%s %d favorites (skipped %d, failed %d).\n", verb, res.Done, res.Skipped, res.Failed)
	if res.Failed > 0 {
		os.Exit(1)
	}
}
