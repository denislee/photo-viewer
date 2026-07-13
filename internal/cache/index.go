package cache

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
	sqlite3 "github.com/mattn/go-sqlite3"
)

// pvDriverName is the name under which we register a customised sqlite3 driver.
// It differs from the stock "sqlite3" driver only by a ConnectHook (see
// registerDriver) that applies the PRAGMAs the DSN can't carry to *every*
// pooled connection.
const pvDriverName = "sqlite3_pv"

// registerDriverOnce guards the one-time sql.Register below. Load runs on every
// "Rebuild index" and repeatedly across the test suite; sql.Register panics on
// a duplicate driver name, so registration must happen exactly once per process.
var registerDriverOnce sync.Once

// registerDriver installs pvDriverName: the stock go-sqlite3 driver plus a
// ConnectHook that runs the PRAGMAs the DSN param set doesn't cover
// (mmap_size, temp_store) on EVERY connection database/sql opens.
//
// Why a hook rather than a single db.Exec: database/sql pools several
// connections and opens them lazily. The scan writer, the UI, the webserver
// and the face pipeline all read/write concurrently, so several connections
// exist at once. The pre-I-08 code ran the PRAGMAs via one db.Exec, which only
// configured whichever single connection happened to serve that call; every
// other pooled connection fell back to SQLite's defaults — temp_store=DEFAULT
// in particular, which spills temp b-trees (the dir/year sorts) to disk. A
// ConnectHook fires on connection creation, so the settings stick pool-wide.
// (journal_mode/synchronous/cache_size/busy_timeout are carried by the DSN,
// which go-sqlite3 also applies per-connection.)
func registerDriver() {
	registerDriverOnce.Do(func() {
		sql.Register(pvDriverName, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				// Run one PRAGMA per Exec so a parse hiccup on either can't
				// mask the other.
				for _, pragma := range []string{
					"PRAGMA mmap_size = 268435456", // 256 MiB memory-mapped I/O
					"PRAGMA temp_store = MEMORY",   // keep temp b-trees off disk
				} {
					if _, err := conn.Exec(pragma, nil); err != nil {
						return err
					}
				}
				return nil
			},
		})
	})
}

// pvDSN builds the connection string for dbPath. The per-connection PRAGMAs
// go-sqlite3 understands as DSN params (journal_mode/synchronous/cache_size/
// busy_timeout) are set here so they apply to every pooled connection, not just
// the first — the values match the historical single-connection PRAGMA block.
// mmap_size and temp_store are not DSN-expressible, so registerDriver's hook
// carries those. go-sqlite3 splits the path from the query on the first '?'.
func pvDSN(dbPath string) string {
	return dbPath + "?_journal_mode=WAL&_synchronous=NORMAL&_cache_size=-64000&_busy_timeout=5000"
}

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

// entryCols is the ordered column list backing every Entry read. It must stay
// in lockstep with the rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix,
// &e.ThumbID, &fav, &e.DurationMs) calls that consume it. entrySelect is the
// bare "SELECT <cols> FROM entries" prefix that every entry query builds on by
// appending its own WHERE/ORDER/LIMIT tail.
const (
	entryCols   = "path, type, size, mtime, thumb_id, favorite, duration_ms"
	entrySelect = "SELECT " + entryCols + " FROM entries"
)

// Index is the SQLite database representation of the media cache.
type Index struct {
	db *sql.DB

	// generation is bumped by Clear so that caches keyed on the index contents
	// (e.g. the webserver's rootSidebarAgg cache) can detect a rebuild and
	// drop stale entries immediately rather than waiting for their TTL.
	generation atomic.Uint64

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

// Generation returns a monotonically increasing counter that is bumped by
// Clear. External caches may compare the generation before and after their TTL
// window to detect a rebuild and discard stale entries immediately.
func (i *Index) Generation() uint64 {
	return i.generation.Load()
}

// addColumn runs an "ALTER TABLE ... ADD COLUMN ..." migration idempotently.
// SQLite has no "ADD COLUMN IF NOT EXISTS", so re-opening a database that was
// already migrated raises a "duplicate column name" error — that one case is
// the benign "already migrated" path and is swallowed. Any OTHER error (I/O
// failure, malformed database) is genuine and returned: ignoring it would leave
// the column missing, so every subsequent multi-column SELECT errors and the
// scan helpers return empty, surfacing as a mysteriously-empty library with no
// trail (C-08).
func addColumn(db *sql.DB, alterSQL string) error {
	if _, err := db.Exec(alterSQL); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

// Load opens the SQLite database index.
func Load(dbPath string) (*Index, error) {
	// Open through pvDriverName so the mmap_size/temp_store PRAGMAs (carried by
	// its ConnectHook) and the DSN PRAGMAs apply to every pooled connection —
	// see registerDriver/pvDSN. The former single-connection PRAGMA block only
	// configured one of the pool's connections.
	registerDriver()
	db, err := sql.Open(pvDriverName, pvDSN(dbPath))
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
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
	// Migrate older databases that pre-date the content_hash column. addColumn
	// tolerates the benign "already migrated" case but propagates real failures.
	if err := addColumn(db, "ALTER TABLE entries ADD COLUMN content_hash TEXT"); err != nil {
		return nil, err
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_entries_hash ON entries(content_hash)"); err != nil {
		return nil, err
	}
	if err := addColumn(db, "ALTER TABLE entries ADD COLUMN favorite INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	// idx_entries_favorite_path (v6) is composite (favorite, path): the leading
	// column serves `WHERE favorite = 1` and the trailing path lets the
	// favorites page and viewer prev/next neighbor probes stream in path order
	// straight off the b-tree. The old single-column idx_entries_favorite
	// served the filter but left `ORDER BY path` to a temp b-tree that
	// materialized and sorted every favorite to return one page/one neighbor;
	// it's dropped in the v6 migration block below.
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_entries_favorite_path ON entries(favorite, path)"); err != nil {
		return nil, err
	}
	if err := addColumn(db, "ALTER TABLE entries ADD COLUMN quick_hash TEXT"); err != nil {
		return nil, err
	}
	if err := addColumn(db, "ALTER TABLE entries ADD COLUMN year INTEGER"); err != nil {
		return nil, err
	}
	if err := addColumn(db, "ALTER TABLE entries ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}

	// idx_entries_thumb backs GetEntryByThumbID, which fronts every /thumb/,
	// /media/, /view/, /hls/ and /api/* request. Without it that lookup was a
	// full table SCAN per HTTP request (≈200 scans of a 100k-row table per
	// gallery page); with it EXPLAIN reports
	// "SEARCH entries USING INDEX idx_entries_thumb (thumb_id=?)".
	//
	// Non-UNIQUE on purpose. thumb_id is sha1(path) and path is the PRIMARY
	// KEY, so values are unique in practice — but a UNIQUE index would abort
	// Load if any legacy database ever held a duplicate, and this index backs
	// every request, so a failed open would take the whole app down. A plain
	// index yields the identical SEARCH plan (verified via EXPLAIN QUERY PLAN),
	// so we trade the unenforced constraint for a guaranteed-safe migration.
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS idx_entries_thumb ON entries(thumb_id)"); err != nil {
		return nil, err
	}
	// idx_entries_yearexpr is an EXPRESSION index whose leading expression is
	// textually identical to yearExpr. SQLite only uses an expression index when
	// the query expression is structurally equal to the index's, so this MUST
	// stay in sync with the yearExpr constant. It lets Years()'s GROUP BY/ORDER
	// BY stream off the ordered index instead of "SCAN entries + USE TEMP B-TREE
	// FOR GROUP BY". The trailing path column (v6) additionally makes the year
	// gallery page and viewer prev/next probes covering — `WHERE <expr> = ?
	// ORDER BY path` streams off the b-tree instead of sorting the whole year via
	// a temp b-tree. The plain column index idx_entries_year could never serve
	// yearExpr (the COALESCE/strftime wrapper hides the bare column), so it's
	// dropped in the v5 block. NOTE: on a pre-v6 database this CREATE IF NOT
	// EXISTS is a no-op (the path-less index already exists under this name); the
	// v6 migration block force-recreates it with path appended.
	if _, err := db.Exec(
		"CREATE INDEX IF NOT EXISTS idx_entries_yearexpr ON entries(" + yearExpr + ", path)",
	); err != nil {
		return nil, err
	}

	// Schema versioning for value-format migrations. Bump when a persisted
	// hash/value format changes so stale rows are invalidated. user_version is
	// written once, after every migration below has run, so a crash mid-upgrade
	// re-runs the migrations rather than skipping them.
	const schemaVersion = 6
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

	if userVersion < 5 {
		// v5 (I-08): drop two write-only indexes that no query reads. Both cost
		// a b-tree maintenance write on every upsert while serving zero reads.
		//   - idx_entries_year: the plain `year` column index. Every year query
		//     goes through yearExpr (COALESCE/strftime), which the column index
		//     can't serve; idx_entries_yearexpr (created above) replaces it.
		//   - idx_entries_duration: no query ever filters or orders by
		//     duration_ms.
		_, _ = db.Exec("DROP INDEX IF EXISTS idx_entries_year")
		_, _ = db.Exec("DROP INDEX IF EXISTS idx_entries_duration")
	}

	if userVersion < 6 {
		// v6: make the favorites and year-view page/neighbor probes covering,
		// and sweep face rows orphaned before the delete/move cleanup landed.
		//
		// (a) Favorites (C-02): the covering idx_entries_favorite_path
		// (favorite, path) was created above; drop the superseded single-column
		// idx_entries_favorite so favorites pages/neighbors stop paying a temp
		// b-tree for `ORDER BY path`. Write cost is unchanged (one b-tree either
		// way).
		_, _ = db.Exec("DROP INDEX IF EXISTS idx_entries_favorite")

		// (b) Year (C-03): recreate idx_entries_yearexpr with path appended. The
		// index name is unchanged, so the top-level CREATE IF NOT EXISTS is a
		// no-op on a pre-v6 database (the path-less index already exists under
		// this name); DROP+CREATE forces the new covering definition. On a fresh
		// database the top-level CREATE already built the (expr, path) form, so
		// this rebuilds an empty index — negligible. The expression MUST stay
		// textually identical to yearExpr for SQLite to keep using the index.
		_, _ = db.Exec("DROP INDEX IF EXISTS idx_entries_yearexpr")
		_, _ = db.Exec("CREATE INDEX IF NOT EXISTS idx_entries_yearexpr ON entries(" + yearExpr + ", path)")

		// (c) Face-row hygiene (C-01, one-time): deletes and organize moves before
		// e07d4ed left face rows keyed to paths no longer in entries — they kept
		// loading on every pipeline start (LoadFaceFreshness) and kept weighting
		// cluster centroids toward vanished files. Sweep the orphans, then null
		// any cluster sample_face_id left dangling by the sweep. Both statements
		// are no-ops on a database that never had orphans (e.g. a fresh one).
		// Going forward RemoveEntry/RemoveEntries delete face rows and MoveFaces
		// relocates them, so no new orphans accrue.
		_, _ = db.Exec("DELETE FROM faces WHERE path NOT IN (SELECT path FROM entries)")
		_, _ = db.Exec("UPDATE face_clusters SET sample_face_id = NULL " +
			"WHERE sample_face_id IS NOT NULL AND sample_face_id NOT IN (SELECT id FROM faces)")
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
		end := min(start+reconcileChunkSize, len(results))
		if err := i.reconcileChunk(results[start:end], &out); err != nil {
			log.Printf("cache: ReconcileBatch: %v", err)
		}
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
// appends the corresponding Entries to out. Returns an error if the
// statement prepare, transaction begin, any row exec, or commit fails;
// entries are only appended to out when the transaction commits successfully.
func (i *Index) reconcileChunk(chunk []scan.Result, out *[]Entry) error {
	stmt, err := i.reconcileStatement()
	if err != nil {
		return err
	}
	tx, err := i.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// On update, clear quick_hash and content_hash whenever size or mtime
	// changed so a re-hash kicks in on the next duplicate scan. (SQLite's
	// `entries.column` inside DO UPDATE refers to the existing row.)
	txStmt := tx.Stmt(stmt)
	defer txStmt.Close()

	// Build entries locally; only append to *out after a successful Commit
	// so the caller never sees entries that weren't actually persisted.
	local := make([]Entry, 0, len(chunk))
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
		if _, err := txStmt.Exec(e.Path, e.Type, e.Size, e.ModTime.Unix(), e.ThumbID, yearArg, e.DurationMs); err != nil {
			return err
		}
		local = append(local, e)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	*out = append(*out, local...)
	return nil
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
//
// The former "(range) OR path = ?" query forced a MULTI-INDEX OR plus a
// "USE TEMP B-TREE FOR ORDER BY" — the whole recursive result set was
// materialised and re-sorted on every listing. The pure range form below
// streams straight off the covering primary-key index in path order with no
// temp b-tree. The dropped `path = dir` disjunct matched the case where a
// *file* path was passed as dir (the web /dir?path= handler doesn't force a
// directory) — that exact row lives outside [dir+"/", ...), so we recover it
// with a standalone point lookup and prepend it. dir sorts strictly before
// dir+"/" (the range's lower bound), so the exact row is always element 0 and
// prepending preserves path order byte-for-byte.
func (i *Index) ListDir(dir string) []Entry {

	lower, upper := dirRange(dir)

	// Grown via append rather than pre-sized: a COUNT(*) pre-pass would walk
	// the same b-tree range a second time, which costs more than the slice
	// regrowth it would save.
	var out []Entry
	if e := i.queryOne(
		entrySelect+" WHERE path = ? LIMIT 1",
		dir,
	); e != nil {
		out = append(out, *e)
	}

	rows, err := i.db.Query(
		entrySelect+" WHERE path >= ? AND path < ? ORDER BY path",
		lower, upper,
	)
	if err != nil {
		return out
	}
	defer rows.Close()

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
	if err := rows.Err(); err != nil {
		log.Printf("cache: ListDir: %v", err)
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
	if err := rows.Err(); err != nil {
		log.Printf("cache: Years: %v", err)
	}
	return out
}

// ListByYear returns entries whose year (per yearExpr) equals year, filtered by
// the same media-type rules as ui.passesFilter. When dir is non-empty, results
// are additionally scoped to the paths recursively under dir; an empty dir means
// library-global.
//
// The dir scoping backs the sidebar's year-preview union (U-08): the date
// folders bucketed under a YYYY header are the YYYY-MM-DD children of the tree
// anchor (treeDir), and each entry's year column is derived from that same
// parent-dir name, so `yearExpr = year AND path under treeDir` returns exactly
// the union the old per-date-dir ListDir loop produced — in one query instead of
// one prefix-LIKE query per shooting day. The `path >= ? AND path < ?` range
// rides the trailing path column of idx_entries_yearexpr, so the year seek and
// the path range stream off one ordered index with no temp b-tree; `ORDER BY
// path` is likewise free off that index.
func (i *Index) ListByYear(year int, filter string, showRAW bool, dir string) []Entry {

	where, args := typeFilterClause(filter, showRAW)
	q := entrySelect + " WHERE " + yearExpr + " = ?"
	queryArgs := []any{year}
	if dir != "" {
		lower, upper := dirRange(dir)
		q += " AND path >= ? AND path < ?"
		queryArgs = append(queryArgs, lower, upper)
	}
	q += where + " ORDER BY path"
	queryArgs = append(queryArgs, args...)

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
	if err := rows.Err(); err != nil {
		log.Printf("cache: ListByYear: %v", err)
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

	rows, err := i.db.Query(entrySelect + " ORDER BY path")
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
	if err := rows.Err(); err != nil {
		log.Printf("cache: All: %v", err)
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
			entrySelect+" WHERE path > ? ORDER BY path LIMIT ?",
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
		if err := rows.Err(); err != nil {
			log.Printf("cache: ForEachEntry: %v", err)
		}
		rows.Close()
		if stop || seen == 0 {
			return
		}
	}
}

// ListByType streams every entry of the given media type to visit, in path
// order, pushing the `type = ?` filter into SQL so callers don't materialize
// the whole index just to keep one type. Like ForEachEntry it pages internally
// (keyset pagination on path) so a slow consumer never pins one long-running
// read transaction — the organize scan drains this while shelling out to
// exiftool per video, which can take minutes. Returning false from visit stops
// iteration early.
func (i *Index) ListByType(t scan.MediaType, visit func(Entry) bool) {
	const pageSize = 1000
	lastPath := ""
	for {
		rows, err := i.db.Query(
			entrySelect+" WHERE type = ? AND path > ? ORDER BY path LIMIT ?",
			int(t), lastPath, pageSize)
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
		if err := rows.Err(); err != nil {
			log.Printf("cache: ListByType: %v", err)
		}
		rows.Close()
		if stop || seen == 0 {
			return
		}
	}
}

// CountByType returns the number of entries of the given media type. Pairs with
// ListByType so a streaming consumer can still show an up-front total without
// materializing the rows.
func (i *Index) CountByType(t scan.MediaType) int {
	var n int
	if err := i.db.QueryRow("SELECT COUNT(*) FROM entries WHERE type = ?", int(t)).Scan(&n); err != nil {
		return 0
	}
	return n
}

// ListFavorites returns all entries flagged as favorites.
func (i *Index) ListFavorites() []Entry {

	rows, err := i.db.Query(entrySelect + " WHERE favorite = 1 ORDER BY path")
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
	if err := rows.Err(); err != nil {
		log.Printf("cache: ListFavorites: %v", err)
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

// SetDurationMs records a video's playback length (in milliseconds) on its
// index row. The HLS handler calls this after an ffprobe fallback so later
// playlist/segment requests read the duration straight off the row instead of
// re-forking ffprobe — and so serveHLSSegment can bound out-of-range segment
// requests, which it skips while the duration is unknown.
//
// A non-positive ms is ignored: 0 is the "duration unknown" sentinel the
// reconcile upsert deliberately preserves, so a bogus probe must never
// overwrite a real value (or a placeholder) with it. A path with no matching
// row updates nothing (e.g. a trash entry), which is harmless.
func (i *Index) SetDurationMs(path string, ms int64) error {
	if ms <= 0 {
		return nil
	}
	_, err := i.db.Exec("UPDATE entries SET duration_ms = ? WHERE path = ?", ms, path)
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

// ToggleFavorite flips the favorite flag for path in one round-trip and returns
// the new value. Collapsing the former IsFavorite (SELECT) + SetFavorite
// (UPDATE) pair into a single `UPDATE ... RETURNING` statement halves the round
// trips per star keystroke and makes the flip atomic against a concurrent scan
// reconciling the same row — the SELECT-then-UPDATE pair could otherwise
// interleave. RETURNING is supported by the embedded SQLite (>= 3.35).
//
// A missing path updates no rows, so QueryRow's Scan reports sql.ErrNoRows; the
// caller treats that as "nothing toggled" and skips the in-memory patch.
func (i *Index) ToggleFavorite(path string) (bool, error) {
	var v int
	if err := i.db.QueryRow(
		"UPDATE entries SET favorite = 1 - favorite WHERE path = ? RETURNING favorite",
		path,
	).Scan(&v); err != nil {
		return false, err
	}
	return v != 0, nil
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
	if err := tx.Commit(); err != nil {
		return err
	}
	i.generation.Add(1)
	return nil
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
		// PRAGMA optimize is SQLite's documented, cheap incremental pattern for
		// keeping the query planner's statistics current: it inspects the schema
		// and only runs ANALYZE on the tables/indexes whose row estimates have
		// drifted far enough to matter, so on an unchanged database it does
		// essentially no work. Running it on the still-open handle, after the
		// prepared statements above are closed and just before the final Close,
		// folds this session's insert/delete churn into the stats the next open
		// starts from — the planner otherwise runs stat-less for the life of
		// every DB (C-09). Best-effort: a failure must not stop Close from
		// releasing the connection, so it is logged, not returned (matching the
		// package's log-and-continue convention for read paths).
		if _, err := i.db.Exec("PRAGMA optimize"); err != nil {
			log.Printf("cache: Close: PRAGMA optimize: %v", err)
		}
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
	if err := rows.Err(); err != nil {
		log.Printf("cache: CountChildDirsFiltered: %v", err)
	}
	return out
}
