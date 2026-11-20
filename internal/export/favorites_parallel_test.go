package export

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// favoritesFixture writes n JPEG favorites under a fresh library root and
// returns it along with an index in which every one is flagged. Two of the
// files share the basename dup.jpg in different subdirectories, so a flattened
// export has to resolve a real name collision. The images are 800x600 gradients
// (generated through image/jpeg by jpegWithOrientation), giving the recompress
// path a real decodable source that needs no external tools.
func favoritesFixture(t *testing.T, n int) (string, *cache.Index) {
	t.Helper()
	root := t.TempDir()

	idx, err := cache.Load(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("load index: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	results := make([]scan.Result, 0, n)
	for i := range n {
		dir, name := "c", fmt.Sprintf("img%02d.jpg", i)
		switch i {
		case 0:
			dir, name = "a", "dup.jpg"
		case 1:
			dir, name = "b", "dup.jpg"
		}
		p := filepath.Join(root, dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, jpegWithOrientation(t, 800, 600, 1), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		results = append(results, scan.Result{
			Path:    p,
			Type:    scan.TypePhoto,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}

	idx.ReconcileBatch(results)
	for _, r := range results {
		if err := idx.SetFavorite(r.Path, true); err != nil {
			t.Fatalf("SetFavorite(%s): %v", r.Path, err)
		}
	}
	return root, idx
}

// TestFavoritesRecompressParallelOrderedProgress drives a real (non-dry-run)
// flattened recompress of 12 JPEGs through the image worker pool. It pins the
// two contracts that parallelism could break: progress still arrives strictly
// in step order, and the collision planning reserves destinations before any
// worker writes them, so the two dup.jpg sources land on dup.jpg and dup_1.jpg
// instead of racing onto the same path. Every reported output must be a
// decodable JPEG within the requested edge.
func TestFavoritesRecompressParallelOrderedProgress(t *testing.T) {
	root, idx := favoritesFixture(t, 12)
	dst := t.TempDir()

	var (
		steps   []int
		actions []string
		dests   []string
	)
	progress := func(step, total int, action, msg string) {
		steps = append(steps, step)
		actions = append(actions, action)
		_, dest, ok := strings.Cut(msg, " -> ")
		if !ok {
			t.Errorf("progress step %d msg %q lacks \" -> \"", step, msg)
			return
		}
		dests = append(dests, dest)
	}

	res, err := Favorites(context.Background(), idx, Options{
		Root:        root,
		Dst:         dst,
		Flatten:     true,
		MaxLongEdge: 256,
	}, progress)
	if err != nil {
		t.Fatalf("Favorites: %v", err)
	}
	if res.Done != 12 || res.Failed != 0 || res.Skipped != 0 {
		t.Fatalf("res = %+v, want 12 done, 0 failed, 0 skipped", res)
	}
	if len(steps) != 12 {
		t.Fatalf("got %d progress calls, want 12: %v", len(steps), steps)
	}
	if len(actions) != 12 || len(dests) != 12 {
		t.Fatalf("got %d actions / %d destinations, want 12 each", len(actions), len(dests))
	}
	for i, step := range steps {
		if step != i+1 {
			t.Fatalf("progress steps not in order: got %v", steps)
		}
		if actions[i] != "RECOMPRESS" {
			t.Errorf("step %d action = %q, want RECOMPRESS", step, actions[i])
		}
	}

	seen := make(map[string]bool, len(dests))
	for i, dest := range dests {
		if seen[dest] {
			t.Fatalf("step %d reuses destination %s (dests: %v)", steps[i], dest, dests)
		}
		seen[dest] = true
		assertJPEGWithin(t, dest, 256)
	}
	for _, want := range []string{
		filepath.Join(dst, "dup.jpg"),
		filepath.Join(dst, "dup_1.jpg"),
	} {
		if !seen[want] {
			t.Errorf("collision destination %s missing; got %v", want, dests)
		}
	}
}

// TestFavoritesRecompressCancel cancels the context from inside the progress
// callback after step 3. The run must stop planning, drain what is in flight,
// keep every reported step in order, and return context.Canceled rather than
// hanging or reporting success.
func TestFavoritesRecompressCancel(t *testing.T) {
	root, idx := favoritesFixture(t, 12)
	dst := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type runResult struct {
		res Result
		err error
	}
	done := make(chan runResult, 1)
	var steps []int
	go func() {
		res, err := Favorites(ctx, idx, Options{
			Root:        root,
			Dst:         dst,
			Flatten:     true,
			MaxLongEdge: 256,
		}, func(step, total int, action, msg string) {
			steps = append(steps, step)
			if step == 3 {
				cancel()
			}
		})
		done <- runResult{res: res, err: err}
	}()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("Favorites error = %v, want context.Canceled", got.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Favorites did not return after cancellation")
	}

	if len(steps) < 3 {
		t.Fatalf("got %d progress steps, want at least 3: %v", len(steps), steps)
	}
	for i := 1; i < len(steps); i++ {
		if steps[i] != steps[i-1]+1 {
			t.Fatalf("progress steps not in order after cancellation: %v", steps)
		}
	}
}
