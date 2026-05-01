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
	Path     string         `json:"path"`
	Type     scan.MediaType `json:"type"`
	Size     int64          `json:"size"`
	ModTime  time.Time      `json:"mtime"`
	ThumbID  string         `json:"thumb_id"`
	Favorite bool           `json:"favorite"`
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
			thumb_id TEXT,
			content_hash TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_entries_path ON entries(path);
		CREATE INDEX IF NOT EXISTS idx_entries_size ON entries(size);
	`)
	if err != nil {
		return nil, err
	}
	// Migrate older databases that pre-date the content_hash column. The
	// "duplicate column name" error is ignored on purpose.
	_, _ = db.Exec("ALTER TABLE entries ADD COLUMN content_hash TEXT")
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_entries_hash ON entries(content_hash)"); err != nil {
		return nil, err
	}
	_, _ = db.Exec("ALTER TABLE entries ADD COLUMN favorite INTEGER NOT NULL DEFAULT 0")
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_entries_favorite ON entries(favorite)"); err != nil {
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

	stmt, err := tx.Prepare(`INSERT INTO entries (path, type, size, mtime, thumb_id) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			type = excluded.type,
			size = excluded.size,
			mtime = excluded.mtime,
			thumb_id = excluded.thumb_id`)
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

	rows, err := i.db.Query("SELECT path, type, size, mtime, thumb_id, favorite FROM entries WHERE path LIKE ? OR path = ? ORDER BY path", likePattern, dir)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var fav int
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav); err == nil {
			e.ModTime = time.Unix(mtimeUnix, 0)
			e.Favorite = fav != 0
			if e.Path == dir || strings.HasPrefix(e.Path, prefix) || filepath.Dir(e.Path) == dir {
				out = append(out, e)
			}
		}
	}
	return out
}

// CountDir returns the number of entries under the specific directory prefix.
func (i *Index) CountDir(dir string) int {
	i.mu.Lock()
	defer i.mu.Unlock()

	prefix := dir
	if len(prefix) > 0 && prefix[len(prefix)-1] != filepath.Separator {
		prefix += string(filepath.Separator)
	}
	likePattern := prefix + "%"

	var count int
	err := i.db.QueryRow("SELECT COUNT(*) FROM entries WHERE path LIKE ? OR path = ?", likePattern, dir).Scan(&count)
	if err != nil {
		return 0
	}
	return count
}

// All returns a snapshot of the entire index.
func (i *Index) All() []Entry {
	i.mu.Lock()
	defer i.mu.Unlock()

	rows, err := i.db.Query("SELECT path, type, size, mtime, thumb_id, favorite FROM entries ORDER BY path")
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var fav int
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav); err == nil {
			e.ModTime = time.Unix(mtimeUnix, 0)
			e.Favorite = fav != 0
			out = append(out, e)
		}
	}
	return out
}

// ListFavorites returns all entries flagged as favorites.
func (i *Index) ListFavorites() []Entry {
	i.mu.Lock()
	defer i.mu.Unlock()

	rows, err := i.db.Query("SELECT path, type, size, mtime, thumb_id, favorite FROM entries WHERE favorite = 1 ORDER BY path")
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var fav int
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav); err == nil {
			e.ModTime = time.Unix(mtimeUnix, 0)
			e.Favorite = fav != 0
			out = append(out, e)
		}
	}
	return out
}

// SetFavorite toggles the favorite flag for the given path.
func (i *Index) SetFavorite(path string, favorite bool) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	v := 0
	if favorite {
		v = 1
	}
	_, err := i.db.Exec("UPDATE entries SET favorite = ? WHERE path = ?", v, path)
	return err
}

// IsFavorite reports whether a path is flagged as a favorite.
func (i *Index) IsFavorite(path string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	var v int
	if err := i.db.QueryRow("SELECT favorite FROM entries WHERE path = ?", path).Scan(&v); err != nil {
		return false
	}
	return v != 0
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
