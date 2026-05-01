package cache

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
	_ "github.com/mattn/go-sqlite3"
)

// Entry is one media file recorded in the index.
type Entry struct {
	Path    string         `json:"path"`
	Type    scan.MediaType `json:"type"`
	Size    int64          `json:"size"`
	ModTime time.Time      `json:"mtime"`
	ThumbID string         `json:"thumb_id"`
}

// Index is the SQLite database representation of the media cache.
type Index struct {
	mu sync.Mutex
	db *sql.DB
}

// Load opens the SQLite database index.
func Load(dbPath string) (*Index, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous = NORMAL;
		CREATE TABLE IF NOT EXISTS entries (
			path TEXT PRIMARY KEY,
			type INTEGER,
			size INTEGER,
			mtime INTEGER,
			thumb_id TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_entries_path ON entries(path);
	`)
	if err != nil {
		return nil, err
	}

	return &Index{db: db}, nil
}

// Save is a no-op for SQLite as transactions handle persistence.
func (i *Index) Save() error {
	return nil
}

// ReconcileBatch inserts or updates a slice of scan results in a single transaction.
func (i *Index) ReconcileBatch(results []scan.Result) []Entry {
	i.mu.Lock()
	defer i.mu.Unlock()

	out := make([]Entry, 0, len(results))
	if len(results) == 0 {
		return out
	}

	tx, err := i.db.Begin()
	if err != nil {
		return out
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare("INSERT OR REPLACE INTO entries (path, type, size, mtime, thumb_id) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return out
	}
	defer stmt.Close()

	for _, r := range results {
		e := Entry{
			Path:    r.Path,
			Type:    r.Type,
			Size:    r.Size,
			ModTime: r.ModTime,
			ThumbID: ThumbIDFor(r.Path),
		}
		_, _ = stmt.Exec(e.Path, e.Type, e.Size, e.ModTime.Unix(), e.ThumbID)
		out = append(out, e)
	}
	_ = tx.Commit()

	return out
}

// Prune drops index entries whose path is not in the given set. Returns the
// list of removed paths so callers can delete their thumbnail files.
func (i *Index) Prune(seen map[string]struct{}) []string {
	i.mu.Lock()
	defer i.mu.Unlock()

	rows, err := i.db.Query("SELECT path FROM entries")
	if err != nil {
		return nil
	}
	var toDelete []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			if _, ok := seen[p]; !ok {
				toDelete = append(toDelete, p)
			}
		}
	}
	rows.Close()

	if len(toDelete) > 0 {
		tx, _ := i.db.Begin()
		if tx != nil {
			stmt, err := tx.Prepare("DELETE FROM entries WHERE path = ?")
			if err == nil {
				for _, p := range toDelete {
					_, _ = stmt.Exec(p)
				}
				stmt.Close()
			}
			_ = tx.Commit()
		}
	}
	return toDelete
}

// ListDir returns a slice of entries under the specific directory prefix.
func (i *Index) ListDir(dir string) []Entry {
	i.mu.Lock()
	defer i.mu.Unlock()

	prefix := dir
	if len(prefix) > 0 && prefix[len(prefix)-1] != filepath.Separator {
		prefix += string(filepath.Separator)
	}
	likePattern := prefix + "%"

	rows, err := i.db.Query("SELECT path, type, size, mtime, thumb_id FROM entries WHERE path LIKE ? OR path = ? ORDER BY path", likePattern, dir)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID); err == nil {
			e.ModTime = time.Unix(mtimeUnix, 0)
			if e.Path == dir || strings.HasPrefix(e.Path, prefix) || filepath.Dir(e.Path) == dir {
				out = append(out, e)
			}
		}
	}
	return out
}

// All returns a snapshot of the entire index.
func (i *Index) All() []Entry {
	i.mu.Lock()
	defer i.mu.Unlock()

	rows, err := i.db.Query("SELECT path, type, size, mtime, thumb_id FROM entries ORDER BY path")
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID); err == nil {
			e.ModTime = time.Unix(mtimeUnix, 0)
			out = append(out, e)
		}
	}
	return out
}

// Wipe deletes the database and thumbs folder.
func Wipe(dbPath, cacheDir string) error {
	os.RemoveAll(filepath.Join(cacheDir, "thumbs"))
	return os.Remove(dbPath)
}

// ThumbIDFor returns the deterministic thumbnail identifier for a media path.
func ThumbIDFor(path string) string {
	sum := sha1.Sum([]byte(path))
	return hex.EncodeToString(sum[:])
}

// Close explicitly closes the database connection.
func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.db != nil {
		return i.db.Close()
	}
	return nil
}
