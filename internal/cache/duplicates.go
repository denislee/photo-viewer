package cache

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"
)

// DuplicateGroup is a set of entries that share identical content.
type DuplicateGroup struct {
	Hash    string
	Entries []Entry
}

func hashFile(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	bp := hashBufPool.Get().(*[]byte)
	defer hashBufPool.Put(bp)
	// Wrap f so io.CopyBuffer can't take the *os.File WriterTo fast path, which
	// would ignore buf and stream in 32 KiB chunks again (C5-05).
	if _, err := io.CopyBuffer(h, struct{ io.Reader }{f}, *bp); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashBufPool recycles hashFile's 1 MiB read buffers: SHA-256 over 32 KiB
// io.Copy reads ran ~45% below what 1 MiB reads reach on a cached file (C5-05).
var hashBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 1<<20)
		return &b
	},
}

// quickHashWindow is the read-window size in bytes for quickHash. 64 KiB is
// chosen so two reads cover the head and tail of a typical media file without
// pulling in arbitrarily large blocks.
const quickHashWindow = 64 * 1024

// quickHashBufPool recycles 64 KiB read buffers across quickHash calls so the
// duplicate scan doesn't burn an allocation per file.
var quickHashBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, quickHashWindow)
		return &b
	},
}

// quickHash hashes the file size plus the first and last 64 KiB of its
// contents. Two files that share size but produce different quick hashes
// cannot be byte-identical, so they can skip the full SHA-256 read. Mixing
// size in means a quick-hash collision implies same size, so callers can
// group by quick alone instead of (size, quick).
func quickHash(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	_ = binary.Write(h, binary.LittleEndian, size)
	if size <= int64(quickHashWindow)*2 {
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
	} else {
		bufp := quickHashBufPool.Get().(*[]byte)
		defer quickHashBufPool.Put(bufp)
		buf := *bufp
		if _, err := io.ReadFull(f, buf); err != nil {
			return "", err
		}
		h.Write(buf)
		if _, err := f.Seek(size-int64(quickHashWindow), io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.ReadFull(f, buf); err != nil {
			return "", err
		}
		h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashBatchSize is how many computed hashes to accumulate before committing
// to SQLite. Smaller = more granular resume on cancel, but more transactions.
const hashBatchSize = 500

// hashCand is one file considered by EnsureHashes, carrying both its cached
// hashes and the ones computed during the current pass. quick/full are empty
// when the hash is not known yet.
type hashCand struct {
	path  string
	size  int64
	quick string // cached or freshly computed quick hash, empty if none
	full  string // cached or freshly computed full hash, empty if none
}

// hashResult is a hash produced by a hashPass worker for todo[k].
type hashResult struct {
	k    int
	hash string
}

// hashPass runs one hashing pass over the todo items listed in items: workers
// call hash for each item, and the collector persists the results in
// hashBatchSize batches, writing each hash back into todo in place through
// field.
//
// One worker pool + batched UPDATE shared by both passes; the statement is
// prepared once and bound per batch with tx.Stmt (C3-01); results are written
// into todo in place on the collector goroutine (C4-03).
//
// The collector runs on the calling goroutine, so an early return (a flush
// error) leaves no reader behind — that is why workers select on ctx.Done()
// when sending.
func (i *Index) hashPass(
	ctx context.Context,
	todo []hashCand,
	items []int,
	workers int,
	hash func(path string, size int64) (string, error),
	field func(c *hashCand) *string,
	updateSQL string,
	waitIfPaused func(),
	progress func(done, total int),
	done, total int,
) error {
	stmt, err := i.db.Prepare(updateSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	jobs := make(chan int, workers*2)
	go func() {
		defer close(jobs)
		for _, k := range items {
			select {
			case <-ctx.Done():
				return
			case jobs <- k:
			}
		}
	}()

	results := make(chan hashResult, workers*2)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for k := range jobs {
				if waitIfPaused != nil {
					waitIfPaused()
				}
				if ctx.Err() != nil {
					continue
				}
				h, err := hash(todo[k].path, todo[k].size)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						_ = i.RemoveEntry(todo[k].path)
					}
					continue
				}
				// Select on ctx.Done() so the worker can't wedge on this send
				// once the reader has bailed out early (flush error); the
				// caller's deferred cancel releases every blocked worker.
				select {
				case results <- hashResult{k: k, hash: h}:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	go func() { wg.Wait(); close(results) }()

	pending := make([]int, 0, hashBatchSize)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		tx, err := i.db.Begin()
		if err != nil {
			return err
		}
		ts := tx.Stmt(stmt)
		for _, k := range pending {
			_, _ = ts.Exec(*field(&todo[k]), todo[k].path)
		}
		ts.Close()
		err = tx.Commit()
		pending = pending[:0]
		return err
	}

	for r := range results {
		*field(&todo[r.k]) = r.hash
		pending = append(pending, r.k)
		done++
		if progress != nil {
			progress(done, total)
		}
		if len(pending) >= hashBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// EnsureHashes fills in missing content hashes for entries that share a size
// with at least one other entry. It runs in two passes: a quick hash of the
// first and last 64 KiB to prune size collisions, then a full SHA-256 only on
// files that survived the prune.
//
// Both the quick hash and the full hash are persisted to the index, and
// commits happen in batches throughout the run — so cancelling part way
// through preserves progress and the next run only processes whatever is
// still missing.
//
// waitIfPaused, if non-nil, is called by each worker before starting a
// new hashing job so the caller can implement pause/resume on top of
// the hashing pass without restarting it. It must be cheap and safe to
// call from any goroutine.
func (i *Index) EnsureHashes(ctx context.Context, progress func(done, total int), waitIfPaused func()) error {
	// Derive a cancellable child context so that every return path — a flush
	// error in particular — tears the worker pools down before returning.
	// Without this, an early return leaves the results channel with no reader:
	// the hash workers block forever on their send, so wg.Wait never fires and
	// the closer + feeder goroutines leak (up to NumCPU*3 hashers per failed
	// pass, stacking on every re-run). The deferred cancel below fires on any
	// return and is what unblocks the ctx.Done() arm of each worker's send.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	rows, err := i.db.Query(`
		SELECT path, size, COALESCE(quick_hash, ''), COALESCE(content_hash, '')
		FROM entries
		WHERE size IN (
			SELECT size FROM entries GROUP BY size HAVING COUNT(*) > 1
		)
	`)
	if err != nil {
		return err
	}
	// A cursor error mid-iteration would leave todo partial and duplicate
	// detection silently incomplete, so it fails the pass. A row that won't
	// scan is skipped (it just isn't hashed this run) but counted and logged.
	var todo []hashCand
	var badRows int
	for rows.Next() {
		var c hashCand
		if err := rows.Scan(&c.path, &c.size, &c.quick, &c.full); err != nil {
			if badRows == 0 {
				log.Printf("cache: EnsureHashes: skip unreadable row: %v", err)
			}
			badRows++
			continue
		}
		todo = append(todo, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("list hash candidates: %w", err)
	}
	if badRows > 1 {
		log.Printf("cache: EnsureHashes: skipped %d unreadable rows", badRows)
	}

	if len(todo) == 0 {
		if progress != nil {
			progress(0, 0)
		}
		return ctx.Err()
	}

	// Quick hash reads two 64 KiB windows per file — overwhelmingly I/O-bound,
	// so oversubscribe (NumCPU * 3) to keep the disk queue saturated on SSD.
	// Full SHA-256 reads every byte and the digest itself burns CPU, so cap
	// at NumCPU. Matches the cpu/ext split that ThumbStore uses.
	cpuWorkers := max(runtime.NumCPU(), 4)
	ioWorkers := max(cpuWorkers*3, 6)

	// Phase 1: quick-hash candidates that don't have a cached quick hash.
	var phase1 []int
	for k, c := range todo {
		if c.quick == "" {
			phase1 = append(phase1, k)
		}
	}

	if progress != nil {
		progress(len(todo)-len(phase1), len(todo))
	}

	if len(phase1) > 0 {
		if err := i.hashPass(ctx, todo, phase1, ioWorkers,
			quickHash,
			func(c *hashCand) *string { return &c.quick },
			"UPDATE entries SET quick_hash = ? WHERE path = ?",
			waitIfPaused, progress,
			len(todo)-len(phase1), len(todo),
		); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	// Group by quick hash alone. Size is mixed into the digest by quickHash,
	// so a quick-hash collision already implies same size — no need for a
	// composite (size, quick) key.
	groups := map[string][]int{}
	for k, c := range todo {
		if c.quick == "" {
			continue // quick hash failed earlier
		}
		groups[c.quick] = append(groups[c.quick], k)
	}

	// Phase 2: full-hash files whose quick hash collides with another and
	// don't already have a cached content hash.
	var toFull []int
	for _, ks := range groups {
		if len(ks) < 2 {
			continue
		}
		for _, k := range ks {
			if todo[k].full == "" {
				toFull = append(toFull, k)
			}
		}
	}

	if len(toFull) == 0 {
		return ctx.Err()
	}

	if progress != nil {
		progress(0, len(toFull))
	}

	err = i.hashPass(ctx, todo, toFull, cpuWorkers,
		hashFile,
		func(c *hashCand) *string { return &c.full },
		"UPDATE entries SET content_hash = ? WHERE path = ?",
		waitIfPaused, progress,
		0, len(toFull),
	)
	if err != nil {
		return err
	}

	return ctx.Err()
}

// findDuplicatesSQL selects every entry that shares its content hash with at
// least one other. It is a package const so plan tests can EXPLAIN the exact
// statement.
//
// There is deliberately no ORDER BY: the only ordering callers need is
// "groups by hash, entries by mtime/path", and FindDuplicates establishes that
// in memory with two sorts over the (small) result set. An ORDER BY here forced
// SQLite to build a temp b-tree over every duplicate row before returning the
// first one.
const findDuplicatesSQL = `
		SELECT path, type, size, mtime, thumb_id, content_hash
		FROM entries
		WHERE content_hash IS NOT NULL AND content_hash != ''
		AND content_hash IN (
			SELECT content_hash FROM entries
			WHERE content_hash IS NOT NULL AND content_hash != ''
			GROUP BY content_hash HAVING COUNT(*) > 1
		)
	`

// FindDuplicates returns groups of entries (size >= 2) that share content
// hash. Groups are ordered by content hash; within each group, entries are
// ordered by ModTime ascending (oldest first), ties broken by Path.
//
// The ordering is done here rather than in SQL (see findDuplicatesSQL): the
// SQLite query would otherwise pay for a temp b-tree across the whole result
// set just to return rows the caller re-reads into groups anyway.
func (i *Index) FindDuplicates() []DuplicateGroup {

	rows, err := i.db.Query(findDuplicatesSQL)
	if err != nil {
		return nil
	}
	defer rows.Close()

	groupsByHash := map[string]*DuplicateGroup{}
	var order []string
	for rows.Next() {
		var e Entry
		var mtimeUnix int64
		var hash string
		if err := rows.Scan(&e.Path, &e.Type, &e.Size, &mtimeUnix, &e.ThumbID, &hash); err != nil {
			continue
		}
		e.ModTime = time.Unix(mtimeUnix, 0)
		g, ok := groupsByHash[hash]
		if !ok {
			g = &DuplicateGroup{Hash: hash}
			groupsByHash[hash] = g
			order = append(order, hash)
		}
		g.Entries = append(g.Entries, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("cache: FindDuplicates: %v", err)
	}

	slices.Sort(order)
	out := make([]DuplicateGroup, 0, len(order))
	for _, h := range order {
		g := groupsByHash[h]
		slices.SortFunc(g.Entries, func(a, b Entry) int {
			if c := a.ModTime.Compare(b.ModTime); c != 0 {
				return c
			}
			return cmp.Compare(a.Path, b.Path)
		})
		out = append(out, *g)
	}
	return out
}

// RemoveEntry deletes a single row from the index by path, together with any
// face rows keyed to that path. faces.path references entries.path with no
// foreign key, so without this the face rows orphan forever: they keep loading
// on every pipeline start (LoadFaceFreshness). Deleting the rows alone is not
// enough to fix the clustering: each removed face is also taken out of its
// cluster's running-mean centroid (subtractFacesTx), otherwise the centroid
// keeps weighting toward files that no longer exist (I-04 6c). A running face
// pipeline's in-memory cluster cache is not told; only pv-scan runs one, and
// it never deletes.
func (i *Index) RemoveEntry(path string) error {
	tx, err := i.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM entries WHERE path = ?", path); err != nil {
		return err
	}
	if err := subtractFacesTx(tx, path); err != nil {
		return err
	}
	// face_tags are kept on purpose: a delete goes to the trash, and a restore
	// must bring the names back. SweepOrphanTags reaps them later.
	if _, err := tx.Exec("DELETE FROM face_tag_links WHERE face_id IN (SELECT id FROM faces WHERE path = ?)", path); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM faces WHERE path = ?", path); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveEntries deletes multiple rows from the index in a single transaction,
// each with its face rows (see RemoveEntry for why the faces cleanup matters).
// Used by bulk-delete flows so the UI doesn't pay for one DB round-trip per
// path. Errors on individual rows are swallowed — the transaction commits
// whichever deletes did succeed.
func (i *Index) RemoveEntries(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := i.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	delEntry, err := tx.Prepare("DELETE FROM entries WHERE path = ?")
	if err != nil {
		return err
	}
	defer delEntry.Close()
	delFaces, err := tx.Prepare("DELETE FROM faces WHERE path = ?")
	if err != nil {
		return err
	}
	defer delFaces.Close()
	if err := subtractFacesTx(tx, paths...); err != nil {
		return err
	}
	delLinks, err := tx.Prepare("DELETE FROM face_tag_links WHERE face_id IN (SELECT id FROM faces WHERE path = ?)")
	if err != nil {
		return err
	}
	defer delLinks.Close()
	for _, p := range paths {
		_, _ = delEntry.Exec(p)
		_, _ = delLinks.Exec(p)
		_, _ = delFaces.Exec(p)
	}
	return tx.Commit()
}
