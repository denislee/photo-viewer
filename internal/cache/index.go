package cache

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strconv"
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
	// DurationMs is the playback length of a video in milliseconds. 0 for
	// non-videos and for videos whose duration could not be probed.
	DurationMs int64 `json:"duration_ms"`
	Favorite   bool  `json:"favorite"`
}

// Index is the SQLite database representation of the media cache.
type Index struct {
	db *sql.DB

	// Long-lived prepared statements. SQLite reprepares are not free in
	// Go's database/sql layer — caching the statements for the lifetime of
	// the *Index keeps hot writers (scan reconcile, face writes) from
	// burning a Prepare per call.
	stmtMu             sync.Mutex
	reconcileStmt      *sql.Stmt
	faceInsStmt        *sql.Stmt
	faceUpdClusterStmt *sql.Stmt
	faceNewClusterStmt *sql.Stmt
	faceSetClusterStmt *sql.Stmt
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
		PRAGMA mmap_size = 268435456;
		PRAGMA cache_size = -64000;
		PRAGMA busy_timeout = 5000;
		PRAGMA temp_store = MEMORY;
		CREATE TABLE IF NOT EXISTS entries (
			path TEXT PRIMARY KEY,
			type INTEGER,
			size INTEGER,
			mtime INTEGER,
			thumb_id TEXT,
			content_hash TEXT
		);
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
	_, _ = db.Exec("ALTER TABLE entries ADD COLUMN year INTEGER")
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_entries_year ON entries(year)"); err != nil {
		return nil, err
	}
	_, _ = db.Exec("ALTER TABLE entries ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0")
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_entries_duration ON entries(duration_ms)"); err != nil {
		return nil, err
	}

	// Schema versioning for value-format migrations. Bump when a persisted
	// hash/value format changes so stale rows are invalidated. user_version is
	// written once, after every migration below has run, so a crash mid-upgrade
	// re-runs the migrations rather than skipping them.
	const schemaVersion = 4
	var userVersion int
	_ = db.QueryRow("PRAGMA user_version").Scan(&userVersion)
	if userVersion < 1 {
		// v1: quick_hash now mixes the file size into the digest, so any
		// values stored under the old format must be re-hashed.
		_, _ = db.Exec("UPDATE entries SET quick_hash = NULL")
	}
	if userVersion < 2 {
		// v2: year column populated from the YYYY-MM-DD parent dir. Backfill
		// existing rows so the sidebar's Years/ListByYear queries can group
		// in SQL instead of streaming every row to Go.
		backfillYear(db)
	}
	if userVersion < 3 {
		// v3: drop idx_entries_path — it duplicated the primary key on path
		// and only added write overhead.
		_, _ = db.Exec("DROP INDEX IF EXISTS idx_entries_path")
	}

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

	if userVersion < 4 {
		// v4: the incremental face clusterer switched from cosine distance to
		// squared-Euclidean — the metric dlib's canonical 0.6 threshold was
		// actually calibrated for (see internal/face/cluster.go). Clusters
		// built under the wrong cosine metric chained different people together,
		// so every pre-v4 cluster is polluted and can't be trusted. Wipe the
		// face + cluster rows so the next scan re-detects and re-clusters with
		// the correct metric. A plain wipe (rather than an in-place recluster
		// from the stored embeddings) is the simple, provably-correct reset:
		// Load has no clustering logic to reuse here and cache can't import the
		// face package, and re-detection is already gated by thumb_mtime so only
		// the wiped rows pay for it. On a fresh database these tables are empty,
		// so the deletes are harmless no-ops.
		_, _ = db.Exec("DELETE FROM faces")
		_, _ = db.Exec("DELETE FROM face_clusters")
	}

	if userVersion < schemaVersion {
		_, _ = db.Exec("PRAGMA user_version = " + strconv.Itoa(schemaVersion))
	}

	return &Index{db: db}, nil
}

// Save is a no-op for SQLite as transactions handle persistence.
func (i *Index) Save() error {
	return nil
}

// reconcileChunkSize is the maximum number of rows per transaction in
// ReconcileBatch. Large rebuild scans accumulate tens of thousands of files;
// keeping each transaction bounded lets concurrent readers (the UI sidebar,
// the grid) make progress between chunks instead of stalling for one giant
// fsync, at the cost of one extra fsync per chunk.
const reconcileChunkSize = 5000

const reconcileInsertSQL = `INSERT INTO entries (path, type, size, mtime, thumb_id, year, duration_ms) VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(path) DO UPDATE SET
		type = excluded.type,
		size = excluded.size,
		mtime = excluded.mtime,
		thumb_id = excluded.thumb_id,
		year = excluded.year,
		duration_ms = CASE
			WHEN excluded.duration_ms > 0 THEN excluded.duration_ms
			ELSE entries.duration_ms END,
		quick_hash = CASE
			WHEN entries.size != excluded.size OR entries.mtime != excluded.mtime
			THEN NULL ELSE entries.quick_hash END,
		content_hash = CASE
			WHEN entries.size != excluded.size OR entries.mtime != excluded.mtime
			THEN NULL ELSE entries.content_hash END`

// ReconcileBatch inserts or updates a slice of scan results, chunking the
// work into bounded transactions so readers aren't blocked for the whole
// run on very large scans.
func (i *Index) ReconcileBatch(results []scan.Result) []Entry {

	out := make([]Entry, 0, len(results))
	if len(results) == 0 {
		return out
	}

	for start := 0; start < len(results); start += reconcileChunkSize {
		end := start + reconcileChunkSize
		if end > len(results) {
			end = len(results)
		}
		i.reconcileChunk(results[start:end], &out)
	}
	return out
}

// reconcileStatement lazily prepares the long-lived reconcile upsert. The
// statement survives across batches — over a full library scan that's tens
// of thousands of avoided Prepare calls.
func (i *Index) reconcileStatement() (*sql.Stmt, error) {
	i.stmtMu.Lock()
	defer i.stmtMu.Unlock()
	if i.reconcileStmt != nil {
		return i.reconcileStmt, nil
	}
	s, err := i.db.Prepare(reconcileInsertSQL)
	if err != nil {
		return nil, err
	}
	i.reconcileStmt = s
	return s, nil
}

// reconcileChunk writes one bounded transaction's worth of results and
// appends the corresponding Entries to out.
func (i *Index) reconcileChunk(chunk []scan.Result, out *[]Entry) {
	stmt, err := i.reconcileStatement()
	if err != nil {
		return
	}
	tx, err := i.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()

	// On update, clear quick_hash and content_hash whenever size or mtime
	// changed so a re-hash kicks in on the next duplicate scan. (SQLite's
	// `entries.column` inside DO UPDATE refers to the existing row.)
	txStmt := tx.Stmt(stmt)
	defer txStmt.Close()

	for _, r := range chunk {
		e := Entry{
			Path:       r.Path,
			Type:       r.Type,
			Size:       r.Size,
			ModTime:    r.ModTime,
			ThumbID:    ThumbIDFor(r.Path),
			DurationMs: r.DurationMs,
		}
		var yearArg any
		if y := pathYear(r.Path); y != 0 {
			yearArg = y
		}
		_, _ = txStmt.Exec(e.Path, e.Type, e.Size, e.ModTime.Unix(), e.ThumbID, yearArg, e.DurationMs)
		*out = append(*out, e)
	}
	_ = tx.Commit()
}

// Prune drops index entries whose path is not in the given set. Returns the
// list of removed paths so callers can delete their thumbnail files.
func (i *Index) Prune(seen map[string]struct{}) []string {

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

	lower, upper := dirRange(dir)

	rows, err := i.db.Query(
		"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE (path >= ? AND path < ?) OR path = ? ORDER BY path",
		lower, upper, dir,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	// Grown via append rather than pre-sized: a COUNT(*) pre-pass would walk
	// the same b-tree range a second time, which costs more than the slice
	// regrowth it would save.
	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var fav int
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav, &e.DurationMs); err == nil {
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
	d := func(i int) (int, bool) {
		c := dir[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		return int(c - '0'), true
	}
	y3, ok3 := d(0)
	y2, ok2 := d(1)
	y1, ok1 := d(2)
	y0, ok0 := d(3)
	if !(ok3 && ok2 && ok1 && ok0) {
		return 0
	}
	y := y3*1000 + y2*100 + y1*10 + y0
	if y < 1900 || y > 9999 {
		return 0
	}
	if _, ok := d(5); !ok {
		return 0
	}
	if _, ok := d(6); !ok {
		return 0
	}
	if _, ok := d(8); !ok {
		return 0
	}
	if _, ok := d(9); !ok {
		return 0
	}
	return y
}

// yearExpr is the SQL expression for the per-entry year: prefer the stored
// `year` column (populated from a YYYY-MM-DD parent dir on ingest); fall back
// to the year of mtime in UTC.
const yearExpr = "COALESCE(year, CAST(strftime('%Y', mtime, 'unixepoch') AS INTEGER))"

// Years returns the distinct years (descending) present in the index, with
// per-year counts that respect the current filter and showRAW toggle. Year
// is derived from the YYYY-MM-DD parent folder when present, else from mtime.
func (i *Index) Years(filter string, showRAW bool) []YearStat {

	where, args := typeFilterClause(filter, showRAW)
	q := "SELECT " + yearExpr + " AS y, COUNT(*) FROM entries WHERE 1=1" + where + " GROUP BY y ORDER BY y DESC"

	rows, err := i.db.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []YearStat
	for rows.Next() {
		var y, c int
		if err := rows.Scan(&y, &c); err == nil {
			out = append(out, YearStat{Year: y, Count: c})
		}
	}
	return out
}

// ListByYear returns entries whose year (per entryYear) equals year, filtered
// by the same media-type rules as ui.passesFilter.
func (i *Index) ListByYear(year int, filter string, showRAW bool) []Entry {

	where, args := typeFilterClause(filter, showRAW)
	q := "SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE " + yearExpr + " = ?" + where + " ORDER BY path"
	queryArgs := append([]any{year}, args...)

	rows, err := i.db.Query(q, queryArgs...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var fav int
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav, &e.DurationMs); err != nil {
			continue
		}
		e.ModTime = time.Unix(mtimeUnix, 0)
		e.Favorite = fav != 0
		out = append(out, e)
	}
	return out
}

// backfillYear populates the `year` column for legacy rows where it's still
// NULL but the path's parent directory is a YYYY-MM-DD bucket. Runs once on
// schema migration; rows whose parent isn't a date stay NULL and the SQL
// expression falls back to mtime year.
func backfillYear(db *sql.DB) {
	rows, err := db.Query("SELECT path FROM entries WHERE year IS NULL")
	if err != nil {
		return
	}
	type upd struct {
		path string
		year int
	}
	var pending []upd
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			continue
		}
		if y := pathYear(p); y != 0 {
			pending = append(pending, upd{path: p, year: y})
		}
	}
	rows.Close()
	if len(pending) == 0 {
		return
	}
	tx, err := db.Begin()
	if err != nil {
		return
	}
	stmt, err := tx.Prepare("UPDATE entries SET year = ? WHERE path = ?")
	if err != nil {
		tx.Rollback()
		return
	}
	for _, u := range pending {
		_, _ = stmt.Exec(u.year, u.path)
	}
	stmt.Close()
	_ = tx.Commit()
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

	rows, err := i.db.Query("SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries ORDER BY path")
	if err != nil {
		return nil
	}
	defer rows.Close()

	// Grown via append rather than pre-sized: a COUNT(*) pre-pass would walk
	// the full table a second time, which costs more than the slice regrowth
	// it would save.
	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var fav int
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav, &e.DurationMs); err == nil {
			e.ModTime = time.Unix(mtimeUnix, 0)
			e.Favorite = fav != 0
			out = append(out, e)
		}
	}
	return out
}

// Count returns the number of entries in the index.
func (i *Index) Count() int {
	var n int
	_ = i.db.QueryRow("SELECT COUNT(*) FROM entries").Scan(&n)
	return n
}

// ForEachEntry calls visit for every entry in path order. Iteration is
// internally paged so it doesn't pin one long-running SQL read transaction
// (which would block WAL checkpointing during a multi-minute warm-up).
// Returning false from visit stops iteration early.
func (i *Index) ForEachEntry(visit func(Entry) bool) {
	const pageSize = 1000
	lastPath := ""
	for {
		rows, err := i.db.Query(
			"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE path > ? ORDER BY path LIMIT ?",
			lastPath, pageSize)
		if err != nil {
			return
		}
		seen := 0
		stop := false
		for rows.Next() {
			var e Entry
			var mtimeUnix int64
			var fav int
			if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav, &e.DurationMs); err != nil {
				continue
			}
			e.ModTime = time.Unix(mtimeUnix, 0)
			e.Favorite = fav != 0
			lastPath = e.Path
			seen++
			if !visit(e) {
				stop = true
				break
			}
		}
		rows.Close()
		if stop || seen == 0 {
			return
		}
	}
}

// ListFavorites returns all entries flagged as favorites.
func (i *Index) ListFavorites() []Entry {

	rows, err := i.db.Query("SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE favorite = 1 ORDER BY path")
	if err != nil {
		return nil
	}
	defer rows.Close()

	// Grown via append rather than pre-sized: a COUNT(*) pre-pass would scan
	// the favorites a second time, which costs more than the slice regrowth
	// it would save.
	var out []Entry
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var fav int
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &fav, &e.DurationMs); err == nil {
			e.ModTime = time.Unix(mtimeUnix, 0)
			e.Favorite = fav != 0
			out = append(out, e)
		}
	}
	return out
}

// CountFavorites returns the number of entries flagged as favorites that
// also pass the given media-type filter and showRAW toggle.
func (i *Index) CountFavorites(filter string, showRAW bool) int {
	where, args := typeFilterClause(filter, showRAW)
	q := "SELECT COUNT(*) FROM entries WHERE favorite = 1" + where
	var count int
	if err := i.db.QueryRow(q, args...).Scan(&count); err != nil {
		return 0
	}
	return count
}

// SetFavorite toggles the favorite flag for the given path.
func (i *Index) SetFavorite(path string, favorite bool) error {
	v := 0
	if favorite {
		v = 1
	}
	_, err := i.db.Exec("UPDATE entries SET favorite = ? WHERE path = ?", v, path)
	return err
}

// IsFavorite reports whether a path is flagged as a favorite.
func (i *Index) IsFavorite(path string) bool {
	var v int
	if err := i.db.QueryRow("SELECT favorite FROM entries WHERE path = ?", path).Scan(&v); err != nil {
		return false
	}
	return v != 0
}

// Clear empties the index in place — every entry, face, and face cluster is
// deleted in a single transaction while the database file and connection stay
// open. This is the "Rebuild index" primitive.
//
// Resetting the live handle (rather than deleting the db file and reopening a
// fresh *Index) is deliberate: the old file-delete path left the WAL/SHM
// sidecar files behind, which SQLite could replay into the freshly recreated
// database and resurrect deleted rows. It also swapped the *Index pointer,
// invalidating the long-lived captures held by the duplicates view, fuzzy
// search, and the webserver. Clearing in place avoids both — callers keep the
// same handle and see the emptied index immediately.
func (i *Index) Clear() error {
	tx, err := i.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		"DELETE FROM entries",
		"DELETE FROM faces",
		"DELETE FROM face_clusters",
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ThumbIDFor returns the deterministic thumbnail identifier for a media path.
func ThumbIDFor(path string) string {
	sum := sha1.Sum([]byte(path))
	return hex.EncodeToString(sum[:])
}

// Close explicitly closes the database connection.
func (i *Index) Close() error {
	i.stmtMu.Lock()
	for _, s := range []**sql.Stmt{
		&i.reconcileStmt,
		&i.faceInsStmt,
		&i.faceUpdClusterStmt,
		&i.faceNewClusterStmt,
		&i.faceSetClusterStmt,
	} {
		if *s != nil {
			_ = (*s).Close()
			*s = nil
		}
	}
	i.stmtMu.Unlock()
	if i.db != nil {
		return i.db.Close()
	}
	return nil
}

// CountChildDirsFiltered returns, for each immediate child directory of
// parent, the recursive count of entries that pass the given filter. The
// whole result set is produced by a single grouped scan over the parent's
// path range — far cheaper than firing one COUNT(*) per child, which is
// what the sidebar refresh used to do per scan flush.
//
// Keys in the returned map are absolute paths matching what
// listSubdirs returns.
func (i *Index) CountChildDirsFiltered(parent, filter string, showRAW bool) map[string]int {
	lower, upper := dirRange(parent)
	// SQLite uses 1-based positions for substr/instr. lower has a trailing
	// separator, so the child segment starts at column len(lower)+1 and
	// runs up to the next '/' (which marks the boundary into the child's
	// own subtree). Paths directly under parent (no further '/') are not
	// counted; those represent files in parent and aren't subdirs.
	startCol := len(lower) + 1
	where, args := typeFilterClause(filter, showRAW)
	q := `
		SELECT
			substr(path, ?, instr(substr(path, ?), '/') - 1) AS child,
			COUNT(*)
		FROM entries
		WHERE path >= ? AND path < ?
		  AND instr(substr(path, ?), '/') > 0` + where + `
		GROUP BY child`
	queryArgs := []any{startCol, startCol, lower, upper, startCol}
	queryArgs = append(queryArgs, args...)

	rows, err := i.db.Query(q, queryArgs...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[string]int, 16)
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err == nil && name != "" {
			out[filepath.Join(parent, name)] = n
		}
	}
	return out
}
