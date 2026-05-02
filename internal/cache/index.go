package cache

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	_, _ = db.Exec("ALTER TABLE entries ADD COLUMN quick_hash TEXT")

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS face_clusters (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			label          TEXT,
			centroid       BLOB NOT NULL,
			sample_face_id INTEGER
		);
		CREATE TABLE IF NOT EXISTS faces (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			path        TEXT NOT NULL,
			thumb_mtime INTEGER NOT NULL,
			bbox_x      INTEGER NOT NULL,
			bbox_y      INTEGER NOT NULL,
			bbox_w      INTEGER NOT NULL,
			bbox_h      INTEGER NOT NULL,
			embedding   BLOB NOT NULL,
			cluster_id  INTEGER
		);
		CREATE INDEX IF NOT EXISTS idx_faces_path ON faces(path);
		CREATE INDEX IF NOT EXISTS idx_faces_cluster ON faces(cluster_id);
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

	// On update, clear quick_hash and content_hash whenever size or mtime
	// changed so a re-hash kicks in on the next duplicate scan. (SQLite's
	// `entries.column` inside DO UPDATE refers to the existing row.)
	stmt, err := tx.Prepare(`INSERT INTO entries (path, type, size, mtime, thumb_id) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			type = excluded.type,
			size = excluded.size,
			mtime = excluded.mtime,
			thumb_id = excluded.thumb_id,
			quick_hash = CASE
				WHEN entries.size != excluded.size OR entries.mtime != excluded.mtime
				THEN NULL ELSE entries.quick_hash END,
			content_hash = CASE
				WHEN entries.size != excluded.size OR entries.mtime != excluded.mtime
				THEN NULL ELSE entries.content_hash END`)
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

	tx, err := i.db.Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback()

	if _, err := tx.Exec("CREATE TEMP TABLE IF NOT EXISTS prune_seen (path TEXT PRIMARY KEY)"); err != nil {
		return nil
	}
	if _, err := tx.Exec("DELETE FROM prune_seen"); err != nil {
		return nil
	}

	ins, err := tx.Prepare("INSERT OR IGNORE INTO prune_seen (path) VALUES (?)")
	if err != nil {
		return nil
	}
	for p := range seen {
		_, _ = ins.Exec(p)
	}
	ins.Close()

	rows, err := tx.Query("SELECT e.path FROM entries e LEFT JOIN prune_seen s ON s.path = e.path WHERE s.path IS NULL")
	if err != nil {
		return nil
	}
	var toDelete []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			toDelete = append(toDelete, p)
		}
	}
	rows.Close()

	if len(toDelete) > 0 {
		if _, err := tx.Exec("DELETE FROM entries WHERE path NOT IN (SELECT path FROM prune_seen)"); err != nil {
			return nil
		}
	}

	if err := tx.Commit(); err != nil {
		return nil
	}
	return toDelete
}

// dirRange returns the [lower, upper) string range that captures every path
// strictly under dir. With dir="/foo" the bounds are "/foo/" and "/foo0",
// which is index-friendly because the path column is the primary key.
func dirRange(dir string) (string, string) {
	prefix := dir
	if len(prefix) == 0 || prefix[len(prefix)-1] != filepath.Separator {
		prefix += string(filepath.Separator)
	}
	upper := prefix[:len(prefix)-1] + string(rune(filepath.Separator)+1)
	return prefix, upper
}

// ListDir returns a slice of entries under the specific directory prefix.
func (i *Index) ListDir(dir string) []Entry {
	i.mu.Lock()
	defer i.mu.Unlock()

	lower, upper := dirRange(dir)

	rows, err := i.db.Query(
		"SELECT path, type, size, mtime, thumb_id, favorite FROM entries WHERE (path >= ? AND path < ?) OR path = ? ORDER BY path",
		lower, upper, dir,
	)
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

// CountDir returns the number of entries under the specific directory prefix.
func (i *Index) CountDir(dir string) int {
	return i.CountDirFiltered(dir, "All", true)
}

// CountDirFiltered returns the number of entries under dir that match the
// given media-type filter ("All" / "Photos" / "Videos") and showRAW toggle —
// the same semantics as ui.passesFilter.
func (i *Index) CountDirFiltered(dir, filter string, showRAW bool) int {
	i.mu.Lock()
	defer i.mu.Unlock()

	lower, upper := dirRange(dir)
	where, args := typeFilterClause(filter, showRAW)
	q := "SELECT COUNT(*) FROM entries WHERE ((path >= ? AND path < ?) OR path = ?)" + where
	queryArgs := append([]any{lower, upper, dir}, args...)

	var count int
	if err := i.db.QueryRow(q, queryArgs...).Scan(&count); err != nil {
		return 0
	}
	return count
}

// YearStat is a (year, count) pair for the year-grouped sidebar.
type YearStat struct {
	Year  int
	Count int
}

// pathYear returns the year encoded in the immediate parent directory name
// when it matches YYYY-MM-DD, else 0. The import flow names destination
// folders by capture date, but file mtimes get bumped to the import time as
// data flows through the inbox — so the folder name is a more reliable
// source of "when was this taken" than mtime for imported photos.
func pathYear(path string) int {
	dir := filepath.Base(filepath.Dir(path))
	if len(dir) != 10 || dir[4] != '-' || dir[7] != '-' {
		return 0
	}
	y, err := strconv.Atoi(dir[:4])
	if err != nil || y < 1900 || y > 9999 {
		return 0
	}
	if _, err := strconv.Atoi(dir[5:7]); err != nil {
		return 0
	}
	if _, err := strconv.Atoi(dir[8:10]); err != nil {
		return 0
	}
	return y
}

// entryYear returns pathYear(path) when non-zero, else the year of mtime in UTC.
func entryYear(path string, mtime int64) int {
	if y := pathYear(path); y != 0 {
		return y
	}
	return time.Unix(mtime, 0).UTC().Year()
}

// Years returns the distinct years (descending) present in the index, with
// per-year counts that respect the current filter and showRAW toggle. Year
// is derived from the YYYY-MM-DD parent folder when present, else from mtime.
func (i *Index) Years(filter string, showRAW bool) []YearStat {
	i.mu.Lock()
	defer i.mu.Unlock()

	where, args := typeFilterClause(filter, showRAW)
	q := "SELECT path, mtime FROM entries WHERE 1=1" + where

	rows, err := i.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	counts := make(map[int]int)
	for rows.Next() {
		var path string
		var mtimeUnix int64
		if err := rows.Scan(&path, &mtimeUnix); err == nil {
			counts[entryYear(path, mtimeUnix)]++
		}
	}

	out := make([]YearStat, 0, len(counts))
	for y, c := range counts {
		out = append(out, YearStat{Year: y, Count: c})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Year > out[b].Year })
	return out
}

// ListByYear returns entries whose year (per entryYear) equals year, filtered
// by the same media-type rules as ui.passesFilter.
func (i *Index) ListByYear(year int, filter string, showRAW bool) []Entry {
	i.mu.Lock()
	defer i.mu.Unlock()

	where, args := typeFilterClause(filter, showRAW)
	q := "SELECT path, type, size, mtime, thumb_id, favorite FROM entries WHERE 1=1" + where + " ORDER BY path"

	rows, err := i.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var fav int
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav); err != nil {
			continue
		}
		if entryYear(e.Path, mtimeUnix) != year {
			continue
		}
		e.ModTime = time.Unix(mtimeUnix, 0)
		e.Favorite = fav != 0
		out = append(out, e)
	}
	return out
}

// typeFilterClause builds an SQL fragment ("" or " AND ...") and the
// corresponding bound args for the media-type filter. MediaType values come
// from internal/scan: TypeRAW=2, TypeVideo=4. The clause is mirrored to
// ui.passesFilter so SQL- and in-memory filtering agree.
func typeFilterClause(filter string, showRAW bool) (string, []any) {
	var parts []string
	var args []any
	if !showRAW {
		parts = append(parts, "type != ?")
		args = append(args, int(scan.TypeRAW))
	}
	switch filter {
	case "Photos":
		parts = append(parts, "type != ?")
		args = append(args, int(scan.TypeVideo))
	case "Videos":
		parts = append(parts, "type = ?")
		args = append(args, int(scan.TypeVideo))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(parts, " AND "), args
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
