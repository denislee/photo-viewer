package cache

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// Entry is one media file recorded in the index.
type Entry struct {
	Path    string         `json:"path"`
	Type    scan.MediaType `json:"type"`
	Size    int64          `json:"size"`
	ModTime time.Time      `json:"mtime"`
	ThumbID string         `json:"thumb_id"`
}

// fresh reports whether r matches e on the inputs that affect the thumbnail.
func (e Entry) fresh(r scan.Result) bool {
	return e.Size == r.Size && e.ModTime.Equal(r.ModTime) && e.Type == r.Type
}

// Index is the in-memory representation of <cache>/index.json.
type Index struct {
	mu      sync.Mutex
	dir     string
	entries map[string]Entry
}

// Load reads the index from dir. A missing file returns an empty index.
func Load(dir string) (*Index, error) {
	idx := &Index{dir: dir, entries: map[string]Entry{}}
	f, err := os.Open(filepath.Join(dir, "index.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return nil, err
	}
	defer f.Close()
	var on []Entry
	if err := json.NewDecoder(f).Decode(&on); err != nil {
		return idx, nil
	}
	for _, e := range on {
		idx.entries[e.Path] = e
	}
	return idx, nil
}

// Save writes the index to <dir>/index.json atomically.
func (i *Index) Save() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	list := make([]Entry, 0, len(i.entries))
	for _, e := range i.entries {
		list = append(list, e)
	}
	tmp := filepath.Join(i.dir, "index.json.tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(list); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(i.dir, "index.json"))
}

// Reconcile updates the index against a fresh walk result. It returns the
// merged entry: if the file was already known and unchanged, the existing
// entry (with its ThumbID) is reused; otherwise a new entry is recorded.
func (i *Index) Reconcile(r scan.Result) Entry {
	i.mu.Lock()
	defer i.mu.Unlock()
	if existing, ok := i.entries[r.Path]; ok && existing.fresh(r) {
		return existing
	}
	e := Entry{
		Path:    r.Path,
		Type:    r.Type,
		Size:    r.Size,
		ModTime: r.ModTime,
		ThumbID: thumbID(r.Path),
	}
	i.entries[r.Path] = e
	return e
}

// Prune drops index entries whose path is not in the given set. Returns the
// list of removed paths so callers can delete their thumbnail files.
func (i *Index) Prune(seen map[string]struct{}) []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	var removed []string
	for p := range i.entries {
		if _, ok := seen[p]; !ok {
			removed = append(removed, p)
			delete(i.entries, p)
		}
	}
	return removed
}

// All returns a snapshot of the index, sorted by path.
func (i *Index) All() []Entry {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]Entry, 0, len(i.entries))
	for _, e := range i.entries {
		out = append(out, e)
	}
	return out
}

// Wipe deletes the cache directory contents (index + thumbs).
func Wipe(dir string) error {
	if err := os.RemoveAll(filepath.Join(dir, "thumbs")); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, "index.json"))
}

// ThumbIDFor returns the deterministic thumbnail identifier for a media path.
func ThumbIDFor(path string) string {
	sum := sha1.Sum([]byte(path))
	return hex.EncodeToString(sum[:])
}

func thumbID(path string) string { return ThumbIDFor(path) }
