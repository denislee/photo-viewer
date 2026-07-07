// Command pv-scan is a headless smoke-test entry point: it scans a directory
// and writes the cache, useful for verifying the pipeline without a display.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/face"
	"github.com/dns/photo-viewer/internal/scan"
)

func main() {
	root := flag.String("root", "", "library root to scan")
	noFaces := flag.Bool("no-faces", false, "skip face detection even if pv-face-detect is installed")
	flag.Parse()
	cacheDir, _ := cache.CacheDir(*root)
	fmt.Println("cache dir:", cacheDir)
	dbPath := cache.IndexPath(*root, cacheDir)
	idx, _ := cache.Load(dbPath)
	store, _ := cache.NewThumbStore(cacheDir)
	type detected struct {
		entry cache.Entry
		thumb string
		mtime int64
	}
	var faceJobs []detected

	for r := range scan.Walk(context.Background(), *root) {
		e := idx.ReconcileBatch([]scan.Result{r})[0]
		p, err := store.Path(e)
		fmt.Printf("  %s -> %s (err=%v)\n", e.Path, p, err)
		if err == nil && p != "" {
			if info, statErr := os.Stat(p); statErr == nil {
				faceJobs = append(faceJobs, detected{entry: e, thumb: p, mtime: info.ModTime().Unix()})
			}
		}
	}
	idx.Save()

	if *noFaces {
		return
	}
	pipe := face.NewPipeline(idx, nil)
	if !pipe.Enabled() {
		fmt.Println("face: pv-face-detect unavailable, skipping detection")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pipe.Start(ctx)
	for _, j := range faceJobs {
		pipe.SubmitBlocking(ctx, face.Job{Entry: j.entry, ThumbPath: j.thumb, ThumbMod: j.mtime})
	}
	pipe.Stop()

	clusters := idx.AllClusters()
	fmt.Printf("face: %d clusters\n", len(clusters))
}
